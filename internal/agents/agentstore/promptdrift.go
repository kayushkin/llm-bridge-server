package agentstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A prompt file is an output, but people and harnesses edit outputs. Drift is
// the name for that: the file on disk no longer hashes to what the sections
// last accounted for. This file finds drift, works out which section edits
// reproduce it, and applies them — so an edit made in the file travels back
// up to the sections instead of being overwritten by the next render.
//
// The section bodies in a drift's operations are cut out of the file by
// SplitPromptMarkdown. No model writes or rewrites them. The tagging agent
// only annotates: tags for new sections, and a note.

const (
	PromptDriftStatusOpen             = "open"              // seen, operations computed, not yet decided
	PromptDriftStatusHeld             = "held"              // waits: on labels for added sections, on a person for removed or renamed ones, or could not be mapped
	PromptDriftStatusApplied          = "applied"           // the sections now account for the file
	PromptDriftStatusDismissed        = "dismissed"         // the edit is not wanted; the next render overwrites the file
	PromptDriftStatusSuperseded       = "superseded"        // the file changed again before this was settled
	PromptDriftStatusAlreadyAccounted = "already_accounted" // the sections came to render this content by another path, so there was nothing to apply

	// PromptDriftHeldReasonRemovesOrRenames holds an edit that deletes a
	// section or changes a heading: that can drop a rule without anyone
	// noticing, so a person approves it.
	PromptDriftHeldReasonRemovesOrRenames = "removes or renames sections; approve it to apply"
	// PromptDriftHeldReasonAddsSections holds an edit that adds sections only
	// until each new section is tagged, then it applies by itself
	// (SetPromptDriftAnnotation). An agent documenting what it built adds a
	// section; holding that for a person hid it from every harness that gets
	// its prompt injected rather than from the file.
	PromptDriftHeldReasonAddsSections = "adds sections; applies once each new section is tagged"
)

// PromptDriftOperation is one section write that, with the others in its
// drift, makes the sections account for the drifted file.
type PromptDriftOperation struct {
	Kind            string `json:"kind"`                        // update, insert, delete
	SectionID       int64  `json:"section_id,omitempty"`        // update, delete
	AfterSectionID  int64  `json:"after_section_id,omitempty"`  // insert: the section it follows
	BeforeSectionID int64  `json:"before_section_id,omitempty"` // insert at the top of the file: the section it precedes
	Heading         string `json:"heading,omitempty"`
	// PreviousHeading is set on an update that changes the section's
	// heading: the heading it had. Such an update is a rename.
	PreviousHeading string   `json:"previous_heading,omitempty"`
	Body            string   `json:"body,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

// PromptDriftAnnotation is what the tagging agent (or the person approving)
// adds to a drift. It cannot change a body, a heading or a title. InsertedSections is
// keyed by the operation's index in the drift's operations list.
type PromptDriftAnnotation struct {
	Note             string                            `json:"note,omitempty"`
	InsertedSections []PromptDriftInsertedSectionLabel `json:"inserted_sections,omitempty"`
	AnnotatedBy      string                            `json:"annotated_by,omitempty"`
}

// A label carries tags only. The added section's title is its heading, which
// the person who edited the file already wrote.
type PromptDriftInsertedSectionLabel struct {
	OperationIndex int      `json:"operation_index"`
	Tags           []string `json:"tags"`
}

type PromptDrift struct {
	ID              int64                  `json:"id"`
	CollectionID    int64                  `json:"collection_id"`
	OutputID        int64                  `json:"output_id"`
	Path            string                 `json:"path"`
	AccountedSHA256 string                 `json:"accounted_sha256,omitempty"`
	DiskSHA256      string                 `json:"disk_sha256"`
	Status          string                 `json:"status"`
	HeldReason      string                 `json:"held_reason,omitempty"`
	Operations      []PromptDriftOperation `json:"operations"`
	Annotation      *PromptDriftAnnotation `json:"annotation,omitempty"`
	CreatedAt       int64                  `json:"created_at"`
	ResolvedAt      int64                  `json:"resolved_at,omitempty"`
	// Twins are the other open or held drifts in the collection whose file
	// holds this same content: one edit an agent made to both AGENTS.md and
	// CLAUDE.md. They are one change, so applying or dismissing any of them
	// settles them all, and the lowest id stands for the group.
	Twins []PromptDriftTwin `json:"twins,omitempty"`
}

type PromptDriftTwin struct {
	ID       int64  `json:"id"`
	OutputID int64  `json:"output_id"`
	Path     string `json:"path"`
}

// StandsForItsTwins reports whether this drift is the one shown and annotated
// for its group of twins.
func (d *PromptDrift) StandsForItsTwins() bool {
	for _, twin := range d.Twins {
		if twin.ID < d.ID {
			return false
		}
	}
	return true
}

// NeedsAnnotation reports whether the drift is waiting on labels for sections
// it adds — the one thing a tagging agent is asked for.
func (d *PromptDrift) NeedsAnnotation() bool {
	if d.Status != PromptDriftStatusHeld || d.Annotation != nil || !d.StandsForItsTwins() {
		return false
	}
	for _, operation := range d.Operations {
		if operation.Kind == PromptMarkdownEditInsert {
			return true
		}
	}
	return false
}

const promptDriftColumns = `d.id, d.collection_id, d.output_id, c.root_path, o.relative_path, COALESCE(d.accounted_sha256,''), d.disk_sha256, d.status, COALESCE(d.held_reason,''), COALESCE(d.operations,'[]'), COALESCE(d.annotation,''), d.created_at, COALESCE(d.resolved_at,0)`
const promptDriftJoins = ` FROM prompt_drifts d JOIN prompt_collection_outputs o ON o.id=d.output_id JOIN prompt_collections c ON c.id=d.collection_id `

func scanPromptDrift(row rowScanner) (*PromptDrift, error) {
	var drift PromptDrift
	var root, relativePath, operations, annotation string
	if err := row.Scan(&drift.ID, &drift.CollectionID, &drift.OutputID, &root, &relativePath, &drift.AccountedSHA256, &drift.DiskSHA256, &drift.Status, &drift.HeldReason, &operations, &annotation, &drift.CreatedAt, &drift.ResolvedAt); err != nil {
		return nil, err
	}
	drift.Path = filepath.Join(root, relativePath)
	drift.Operations = []PromptDriftOperation{}
	if err := json.Unmarshal([]byte(operations), &drift.Operations); err != nil {
		return nil, fmt.Errorf("drift %d operations: %w", drift.ID, err)
	}
	if annotation != "" {
		drift.Annotation = &PromptDriftAnnotation{}
		if err := json.Unmarshal([]byte(annotation), drift.Annotation); err != nil {
			return nil, fmt.Errorf("drift %d annotation: %w", drift.ID, err)
		}
	}
	return &drift, nil
}

func (s *Store) GetPromptDrift(id int64) (*PromptDrift, error) {
	drift, err := scanPromptDrift(s.db.QueryRow(`SELECT `+promptDriftColumns+promptDriftJoins+`WHERE d.id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.findPromptDriftTwins(drift); err != nil {
		return nil, err
	}
	return drift, nil
}

// findPromptDriftTwins fills Twins for an open or held drift.
func (s *Store) findPromptDriftTwins(drift *PromptDrift) error {
	drift.Twins = nil
	if drift.Status != PromptDriftStatusOpen && drift.Status != PromptDriftStatusHeld {
		return nil
	}
	rows, err := s.db.Query(`SELECT d.id, d.output_id, c.root_path, o.relative_path`+promptDriftJoins+`
		WHERE d.collection_id=? AND d.disk_sha256=? AND d.id<>? AND d.status IN (?, ?) ORDER BY d.id`,
		drift.CollectionID, drift.DiskSHA256, drift.ID, PromptDriftStatusOpen, PromptDriftStatusHeld)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var twin PromptDriftTwin
		var root, relativePath string
		if err := rows.Scan(&twin.ID, &twin.OutputID, &root, &relativePath); err != nil {
			return err
		}
		twin.Path = filepath.Join(root, relativePath)
		drift.Twins = append(drift.Twins, twin)
	}
	return rows.Err()
}

// GetPromptDriftDiskContent returns the file content the drift was seen with.
func (s *Store) GetPromptDriftDiskContent(id int64) ([]byte, error) {
	var content []byte
	err := s.db.QueryRow(`SELECT disk_content FROM prompt_drifts WHERE id=?`, id).Scan(&content)
	return content, err
}

// ListPromptDrifts lists newest first. collectionID 0 means every collection;
// an empty statuses means every status.
func (s *Store) ListPromptDrifts(collectionID int64, statuses []string) ([]PromptDrift, error) {
	query := `SELECT ` + promptDriftColumns + promptDriftJoins + `WHERE 1=1`
	var args []any
	if collectionID != 0 {
		query += ` AND d.collection_id=?`
		args = append(args, collectionID)
	}
	if len(statuses) > 0 {
		query += ` AND d.status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += ` ORDER BY d.id DESC LIMIT 500`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptDrift{}
	for rows.Next() {
		drift, err := scanPromptDrift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *drift)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err := s.findPromptDriftTwins(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) promptDriftDismissed(outputID int64, diskSHA string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM prompt_drifts WHERE output_id=? AND disk_sha256=? AND status=?`, outputID, diskSHA, PromptDriftStatusDismissed).Scan(&count)
	return count > 0, err
}

// PromptDriftReconciliation is what one pass over the outputs found and did.
type PromptDriftReconciliation struct {
	Detected []PromptDrift `json:"detected"` // drifts first seen on this pass, in their state after it
	Rendered []int64       `json:"rendered_collection_ids"`
	Refused  []string      `json:"refused"`
}

// ReconcilePromptDrifts looks at every collection's outputs, records new
// drift, applies the drifts that only edit existing sections, and re-renders
// the collections it changed so their other files catch up.
func (s *Store) ReconcilePromptDrifts() (*PromptDriftReconciliation, error) {
	out := &PromptDriftReconciliation{Detected: []PromptDrift{}, Rendered: []int64{}, Refused: []string{}}
	detected, err := s.DetectPromptDrifts(0)
	if err != nil {
		return nil, err
	}
	out.Detected = detected
	changed := map[int64]bool{}
	for _, drift := range detected {
		if drift.Status == PromptDriftStatusApplied && !changed[drift.CollectionID] {
			changed[drift.CollectionID] = true
			result, err := s.RenderPromptCollection(drift.CollectionID)
			if err != nil {
				return out, err
			}
			if result.RefusedReason != "" {
				out.Refused = append(out.Refused, result.RefusedReason)
				continue
			}
			out.Rendered = append(out.Rendered, drift.CollectionID)
		}
	}
	return out, nil
}

// DetectPromptDrifts records a drift for every enabled output whose file no
// longer hashes to what the sections account for, and applies it on the spot
// when it only edits the bodies or headings of sections that already exist.
// It never renders. collectionID 0 means every collection.
func (s *Store) DetectPromptDrifts(collectionID int64) ([]PromptDrift, error) {
	query := `SELECT o.id, o.collection_id, c.root_path, o.relative_path, COALESCE(o.accounted_sha256,'')
		FROM prompt_collection_outputs o JOIN prompt_collections c ON c.id=o.collection_id
		WHERE o.enabled=1 AND o.accounted_sha256 IS NOT NULL`
	var args []any
	if collectionID != 0 {
		query += ` AND o.collection_id=?`
		args = append(args, collectionID)
	}
	query += ` ORDER BY o.id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		outputID, collectionID int64
		path, accountedSHA     string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var root, relativePath string
		if err := rows.Scan(&c.outputID, &c.collectionID, &root, &relativePath, &c.accountedSHA); err != nil {
			rows.Close()
			return nil, err
		}
		c.path = filepath.Join(root, relativePath)
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	detected := []PromptDrift{}
	for _, c := range candidates {
		data, err := os.ReadFile(c.path)
		if os.IsNotExist(err) {
			continue // a missing output is shown as missing, and the next render writes it
		}
		if err != nil {
			return detected, fmt.Errorf("read %s: %w", c.path, err)
		}
		diskSHA := sha256Hex(data)
		if diskSHA == c.accountedSHA {
			continue
		}
		// Render now, not once before the loop: a drift applied earlier in
		// this pass may have just brought the sections to this file's
		// content, as when one edit lands in both AGENTS.md and CLAUDE.md.
		renderedSHA, err := s.renderedPromptCollectionSHA(c.collectionID)
		if err != nil {
			return detected, err
		}
		if diskSHA == renderedSHA {
			if err := s.settleOutputTheSectionsAlreadyRender(c.outputID, renderedSHA); err != nil {
				return detected, err
			}
			continue
		}
		var knownID int64
		var knownStatus string
		err = s.db.QueryRow(`SELECT id, status FROM prompt_drifts WHERE output_id=? AND disk_sha256=?`, c.outputID, diskSHA).Scan(&knownID, &knownStatus)
		if err != nil && err != sql.ErrNoRows {
			return detected, err
		}
		if err == nil && knownStatus != PromptDriftStatusApplied && knownStatus != PromptDriftStatusSuperseded {
			continue // already open, held or dismissed for exactly this content
		}

		// Whatever was pending for this file described content that is gone.
		if _, err := s.db.Exec(`UPDATE prompt_drifts SET status=?, resolved_at=? WHERE output_id=? AND status IN (?, ?)`,
			PromptDriftStatusSuperseded, now(), c.outputID, PromptDriftStatusOpen, PromptDriftStatusHeld); err != nil {
			return detected, err
		}

		operations, heldReason := s.computePromptDriftOperations(c.collectionID, c.path, c.accountedSHA, data)
		status := PromptDriftStatusOpen
		if heldReason != "" {
			status = PromptDriftStatusHeld
		}
		encoded, err := json.Marshal(operations)
		if err != nil {
			return detected, err
		}
		var driftID int64
		if knownID != 0 {
			// The file has gone back to content seen before. Reopen that row
			// rather than break the (output, content) uniqueness.
			driftID = knownID
			_, err = s.db.Exec(`UPDATE prompt_drifts SET accounted_sha256=?, status=?, held_reason=?, operations=?, annotation=NULL, resolved_at=NULL WHERE id=?`,
				c.accountedSHA, status, nullIfEmpty(heldReason), string(encoded), driftID)
		} else {
			var res sql.Result
			res, err = s.db.Exec(`
				INSERT INTO prompt_drifts (collection_id, output_id, accounted_sha256, disk_sha256, disk_content, status, held_reason, operations, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.collectionID, c.outputID, c.accountedSHA, diskSHA, data, status, nullIfEmpty(heldReason), string(encoded), now())
			if err == nil {
				driftID, err = res.LastInsertId()
			}
		}
		if err != nil {
			return detected, err
		}

		if status == PromptDriftStatusOpen {
			if reason := promptDriftHoldReason(operations); reason == "" {
				if err := s.ApplyPromptDrift(driftID, nil); err != nil {
					if holdErr := s.holdPromptDrift(driftID, "could not be applied: "+err.Error()); holdErr != nil {
						return detected, holdErr
					}
				}
			} else if err := s.holdPromptDrift(driftID, reason); err != nil {
				return detected, err
			}
		}
		drift, err := s.GetPromptDrift(driftID)
		if err != nil {
			return detected, err
		}
		detected = append(detected, *drift)
	}
	return detected, nil
}

// promptDriftHoldReason says why a drift with these operations waits, or ""
// when it applies at once: text changes to existing sections apply at once, an
// edit that adds sections waits only for their tags, and one that removes a
// section or renames a heading waits for a person.
func promptDriftHoldReason(operations []PromptDriftOperation) string {
	addsSections := false
	for _, operation := range operations {
		switch {
		case operation.Kind == PromptMarkdownEditDelete,
			operation.Kind == PromptMarkdownEditUpdate && operation.PreviousHeading != "":
			return PromptDriftHeldReasonRemovesOrRenames
		case operation.Kind == PromptMarkdownEditInsert:
			addsSections = true
		}
	}
	if addsSections {
		return PromptDriftHeldReasonAddsSections
	}
	return ""
}

// everyInsertedSectionIsTagged says whether the annotation gives at least one
// tag to every section the drift adds.
func everyInsertedSectionIsTagged(drift *PromptDrift, annotation *PromptDriftAnnotation) bool {
	tagged := map[int]bool{}
	for _, label := range annotation.InsertedSections {
		if len(label.Tags) > 0 {
			tagged[label.OperationIndex] = true
		}
	}
	for index, operation := range drift.Operations {
		if operation.Kind == PromptMarkdownEditInsert && !tagged[index] {
			return false
		}
	}
	return true
}

func (s *Store) renderedPromptCollectionSHA(collectionID int64) (string, error) {
	sections, err := s.ListPromptSections(collectionID)
	if err != nil {
		return "", err
	}
	return sha256Hex([]byte(renderPromptSections(sections))), nil
}

// settleOutputTheSectionsAlreadyRender records that an output's file holds
// exactly what the sections render now. The file is accounted for at that
// content, and any open or held drift describing that same content is
// settled, since nothing in it is left to apply.
func (s *Store) settleOutputTheSectionsAlreadyRender(outputID int64, renderedSHA string) error {
	return s.inPromptTransaction(func(tx *sql.Tx) error {
		if err := s.markPromptOutputAccounted(tx, outputID, renderedSHA); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE prompt_drifts SET status=?, held_reason=NULL, resolved_at=? WHERE output_id=? AND disk_sha256=? AND status IN (?, ?)`,
			PromptDriftStatusAlreadyAccounted, now(), outputID, renderedSHA, PromptDriftStatusOpen, PromptDriftStatusHeld)
		return err
	})
}

func (s *Store) holdPromptDrift(id int64, reason string) error {
	_, err := s.db.Exec(`UPDATE prompt_drifts SET status=?, held_reason=? WHERE id=?`, PromptDriftStatusHeld, reason, id)
	return err
}

// computePromptDriftOperations diffs the drifted file against the content the
// sections last accounted for, then maps each changed piece onto the section
// that holds it. Diffing against that base, and not against the sections
// directly, is what makes this right for a file that holds only some of a
// collection's sections. A non-empty heldReason means the mapping failed and
// a person has to look.
func (s *Store) computePromptDriftOperations(collectionID int64, path, accountedSHA string, disk []byte) (operations []PromptDriftOperation, heldReason string) {
	operations = []PromptDriftOperation{}
	file, err := s.findTrackedFileByPath(path)
	if err != nil {
		return operations, fmt.Sprintf("%s is not a tracked file, so its earlier content is not in history: %v", path, err)
	}
	baseVersion, err := s.FindVersionByHash(file.ID, accountedSHA)
	if err != nil || baseVersion == nil {
		return operations, fmt.Sprintf("the content the sections last accounted for (sha %s) is not in %s's version history", shortSHA(accountedSHA), path)
	}
	baseContent, err := s.ReadVersionContent(baseVersion.ID)
	if err != nil {
		return operations, fmt.Sprintf("could not read version %d of %s: %v", baseVersion.ID, path, err)
	}
	base := SplitPromptMarkdown(string(baseContent))
	changed := SplitPromptMarkdown(string(disk))
	edits := DiffPromptMarkdownSections(base, changed)

	sections, err := s.ListPromptSections(collectionID)
	if err != nil {
		return operations, "could not list sections: " + err.Error()
	}
	claimed := map[int64]bool{}
	sectionIDForBase := make([]int64, len(base))
	for baseIndex, markdown := range base {
		for _, section := range sections {
			if !claimed[section.ID] && section.Enabled && section.Heading == markdown.Heading && section.Body == markdown.Body {
				claimed[section.ID] = true
				sectionIDForBase[baseIndex] = section.ID
				break
			}
		}
	}
	need := func(baseIndex int) (int64, bool) {
		if id := sectionIDForBase[baseIndex]; id != 0 {
			return id, true
		}
		heldReason = fmt.Sprintf("the section %q this edit touches has itself changed in the sections since the file was last accounted for", base[baseIndex].Heading)
		return 0, false
	}

	for _, edit := range edits {
		switch edit.Kind {
		case PromptMarkdownEditUpdate:
			id, ok := need(edit.BaseIndex)
			if !ok {
				return []PromptDriftOperation{}, heldReason
			}
			operation := PromptDriftOperation{Kind: edit.Kind, SectionID: id, Heading: changed[edit.ChangedIndex].Heading, Body: changed[edit.ChangedIndex].Body}
			if operation.Heading != base[edit.BaseIndex].Heading {
				operation.PreviousHeading = base[edit.BaseIndex].Heading
			}
			operations = append(operations, operation)
		case PromptMarkdownEditDelete:
			id, ok := need(edit.BaseIndex)
			if !ok {
				return []PromptDriftOperation{}, heldReason
			}
			operations = append(operations, PromptDriftOperation{Kind: edit.Kind, SectionID: id, Heading: base[edit.BaseIndex].Heading})
		case PromptMarkdownEditInsert:
			operation := PromptDriftOperation{Kind: edit.Kind, Heading: changed[edit.ChangedIndex].Heading, Body: changed[edit.ChangedIndex].Body, Tags: []string{}}
			switch {
			case edit.AfterBaseIndex >= 0:
				id, ok := need(edit.AfterBaseIndex)
				if !ok {
					return []PromptDriftOperation{}, heldReason
				}
				operation.AfterSectionID = id
			case len(base) > 0:
				id, ok := need(0)
				if !ok {
					return []PromptDriftOperation{}, heldReason
				}
				operation.BeforeSectionID = id
			}
			operations = append(operations, operation)
		}
	}
	return operations, ""
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// SetPromptDriftAnnotation stores labels for a held drift. It is refused for
// a drift that is already settled, and for a label that points at anything
// but an insert.
func (s *Store) SetPromptDriftAnnotation(id int64, annotation PromptDriftAnnotation) (*PromptDrift, error) {
	drift, err := s.GetPromptDrift(id)
	if err != nil {
		return nil, err
	}
	if drift.Status != PromptDriftStatusHeld && drift.Status != PromptDriftStatusOpen {
		return nil, fmt.Errorf("drift %d is %s; only an open or held drift can be annotated", id, drift.Status)
	}
	if err := validatePromptDriftAnnotation(drift, &annotation); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(annotation)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE prompt_drifts SET annotation=? WHERE id=?`, string(encoded), id); err != nil {
		return nil, err
	}
	// An edit that only adds sections was waiting for exactly these labels.
	if drift.Status == PromptDriftStatusHeld && drift.HeldReason == PromptDriftHeldReasonAddsSections && everyInsertedSectionIsTagged(drift, &annotation) {
		if err := s.ApplyPromptDrift(id, &annotation); err != nil {
			if holdErr := s.holdPromptDrift(id, "could not be applied: "+err.Error()); holdErr != nil {
				return nil, holdErr
			}
		}
	}
	return s.GetPromptDrift(id)
}

func validatePromptDriftAnnotation(drift *PromptDrift, annotation *PromptDriftAnnotation) error {
	seen := map[int]bool{}
	for i := range annotation.InsertedSections {
		label := &annotation.InsertedSections[i]
		if label.OperationIndex < 0 || label.OperationIndex >= len(drift.Operations) {
			return fmt.Errorf("inserted_sections[%d].operation_index %d is outside the drift's %d operations", i, label.OperationIndex, len(drift.Operations))
		}
		if drift.Operations[label.OperationIndex].Kind != PromptMarkdownEditInsert {
			return fmt.Errorf("inserted_sections[%d] points at operation %d, which is a %s; only an insert takes a label", i, label.OperationIndex, drift.Operations[label.OperationIndex].Kind)
		}
		if seen[label.OperationIndex] {
			return fmt.Errorf("operation %d is labeled twice", label.OperationIndex)
		}
		seen[label.OperationIndex] = true
		label.Tags = normalizeTags(label.Tags)
	}
	return nil
}

// ApplyPromptDrift makes the sections account for the drifted file. annotation
// overrides the stored one when given. Everything happens in one transaction
// and is checked before it commits: afterwards every section of the drifted
// file must be present in the collection, in the file's order. It does not
// render; the caller does. Its twins whose files still hold the same content
// are applied with it.
func (s *Store) ApplyPromptDrift(id int64, annotation *PromptDriftAnnotation) error {
	drift, err := s.GetPromptDrift(id)
	if err != nil {
		return err
	}
	if drift.Status != PromptDriftStatusOpen && drift.Status != PromptDriftStatusHeld {
		return fmt.Errorf("drift %d is %s; only an open or held drift can be applied", id, drift.Status)
	}
	if annotation == nil {
		annotation = drift.Annotation
	}
	if annotation == nil {
		annotation = &PromptDriftAnnotation{}
	}
	if err := validatePromptDriftAnnotation(drift, annotation); err != nil {
		return err
	}
	disk, err := s.GetPromptDriftDiskContent(id)
	if err != nil {
		return err
	}
	// The file must still hold the content this drift describes.
	current, err := os.ReadFile(drift.Path)
	if err != nil {
		return err
	}
	if sha256Hex(current) != drift.DiskSHA256 {
		return fmt.Errorf("%s changed again since drift %d was recorded; reconcile to pick up the new content", drift.Path, id)
	}
	// A twin whose file has changed again since is left for the next scan,
	// which supersedes it.
	var twinsStillHoldingTheEdit []PromptDriftTwin
	for _, twin := range drift.Twins {
		twinContent, err := os.ReadFile(twin.Path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && sha256Hex(twinContent) == drift.DiskSHA256 {
			twinsStillHoldingTheEdit = append(twinsStillHoldingTheEdit, twin)
		}
	}
	labels := map[int]PromptDriftInsertedSectionLabel{}
	for _, label := range annotation.InsertedSections {
		labels[label.OperationIndex] = label
	}
	ctx := promptRevisionContext{Source: PromptRevisionSourceDrift, DriftID: id, Note: strings.TrimSpace(annotation.Note)}
	if ctx.Note == "" {
		ctx.Note = "edited in " + drift.Path
	}

	return s.inPromptTransaction(func(tx *sql.Tx) error {
		// The diff anchors every insert in a run of new sections to the same
		// existing section, the last one before the run. Placing each straight
		// after that section would reverse the run, so each insert follows the
		// one placed before it against the same anchor.
		lastInsertedAfter := map[int64]int64{}
		for index, operation := range drift.Operations {
			switch operation.Kind {
			case PromptMarkdownEditUpdate:
				section, err := scanPromptSection(tx.QueryRow(`SELECT id, collection_id, title, heading, body, tags, position, enabled, created_at, updated_at FROM prompt_sections WHERE id=?`, operation.SectionID))
				if err != nil {
					return fmt.Errorf("operation %d: section %d: %w", index, operation.SectionID, err)
				}
				section.Heading, section.Body = operation.Heading, operation.Body
				if err := updatePromptSection(tx, section, ctx); err != nil {
					return fmt.Errorf("operation %d: %w", index, err)
				}
			case PromptMarkdownEditDelete:
				section, err := scanPromptSection(tx.QueryRow(`SELECT id, collection_id, title, heading, body, tags, position, enabled, created_at, updated_at FROM prompt_sections WHERE id=?`, operation.SectionID))
				if err != nil {
					return fmt.Errorf("operation %d: section %d: %w", index, operation.SectionID, err)
				}
				if err := deletePromptSection(tx, section, ctx); err != nil {
					return fmt.Errorf("operation %d: %w", index, err)
				}
			case PromptMarkdownEditInsert:
				placement := operation
				if previous, ok := lastInsertedAfter[operation.AfterSectionID]; ok && operation.AfterSectionID != 0 {
					placement.AfterSectionID = previous
				}
				position, err := promptSectionPositionForInsert(tx, drift.CollectionID, placement)
				if err != nil {
					return fmt.Errorf("operation %d: %w", index, err)
				}
				section := &PromptSection{CollectionID: drift.CollectionID, Heading: operation.Heading, Body: operation.Body, Position: position, Enabled: true, Tags: operation.Tags}
				if label, ok := labels[index]; ok {
					section.Tags = label.Tags
				}
				if err := insertPromptSection(tx, section, ctx); err != nil {
					return fmt.Errorf("operation %d: %w", index, err)
				}
				if operation.AfterSectionID != 0 {
					lastInsertedAfter[operation.AfterSectionID] = section.ID
				}
			default:
				return fmt.Errorf("operation %d: unknown kind %q", index, operation.Kind)
			}
		}

		if err := verifySectionsAccountForFile(tx, drift.CollectionID, disk); err != nil {
			return err
		}
		encoded, err := json.Marshal(annotation)
		if err != nil {
			return err
		}
		settled := append([]PromptDriftTwin{{ID: id, OutputID: drift.OutputID, Path: drift.Path}}, twinsStillHoldingTheEdit...)
		for _, settledDrift := range settled {
			if err := s.markPromptOutputAccounted(tx, settledDrift.OutputID, drift.DiskSHA256); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE prompt_drifts SET status=?, held_reason=NULL, annotation=?, resolved_at=? WHERE id=?`, PromptDriftStatusApplied, string(encoded), now(), settledDrift.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func promptDriftTwinIDs(twins []PromptDriftTwin) []int64 {
	ids := make([]int64, 0, len(twins))
	for _, twin := range twins {
		ids = append(ids, twin.ID)
	}
	return ids
}

// promptSectionPositionForInsert finds a position between the neighbors the
// operation names, renumbering the collection first when they leave no room.
func promptSectionPositionForInsert(tx *sql.Tx, collectionID int64, operation PromptDriftOperation) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		rows, err := tx.Query(`SELECT id, position FROM prompt_sections WHERE collection_id=? ORDER BY position, id`, collectionID)
		if err != nil {
			return 0, err
		}
		var ids []int64
		var positions []int
		for rows.Next() {
			var id int64
			var position int
			if err := rows.Scan(&id, &position); err != nil {
				rows.Close()
				return 0, err
			}
			ids, positions = append(ids, id), append(positions, position)
		}
		rows.Close()

		lower, upper := 0, 0 // positions to land between; upper 0 = end of the collection
		found := operation.AfterSectionID == 0 && operation.BeforeSectionID == 0
		if found && len(positions) > 0 {
			lower = positions[len(positions)-1]
		}
		for i, id := range ids {
			if id == operation.AfterSectionID {
				lower, found = positions[i], true
				if i+1 < len(ids) {
					upper = positions[i+1]
				}
			}
			if id == operation.BeforeSectionID {
				upper, found = positions[i], true
				if i > 0 {
					lower = positions[i-1]
				}
			}
		}
		if !found {
			return 0, errors.New("the section this insert is placed against no longer exists")
		}
		if upper == 0 {
			return lower + promptSectionPositionStep, nil
		}
		if upper-lower >= 2 {
			return lower + (upper-lower)/2, nil
		}
		for i, id := range ids {
			if _, err := tx.Exec(`UPDATE prompt_sections SET position=? WHERE id=?`, (i+1)*promptSectionPositionStep, id); err != nil {
				return 0, err
			}
		}
	}
	return 0, errors.New("no room for the insert even after renumbering")
}

// verifySectionsAccountForFile is the check that makes applying a drift safe:
// every section of the file, heading and body, must be found among the
// collection's enabled sections, in the same order as in the file.
func verifySectionsAccountForFile(tx *sql.Tx, collectionID int64, disk []byte) error {
	rows, err := tx.Query(`SELECT heading, body FROM prompt_sections WHERE collection_id=? AND enabled=1 ORDER BY position, id`, collectionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var stored []PromptMarkdownSection
	for rows.Next() {
		var markdown PromptMarkdownSection
		if err := rows.Scan(&markdown.Heading, &markdown.Body); err != nil {
			return err
		}
		stored = append(stored, markdown)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	next := 0
	for _, wanted := range SplitPromptMarkdown(string(disk)) {
		for next < len(stored) && stored[next] != wanted {
			next++
		}
		if next == len(stored) {
			return fmt.Errorf("after applying, the sections still do not hold the file's section %q as written; nothing was changed", wanted.Heading)
		}
		next++
	}
	return nil
}

// DismissPromptDrift records that the edit in the file is not wanted. The
// file is left alone until the next render, which overwrites it. The edit is
// not lost: the drift row keeps the content, and so does the file's history.
// Its twins are dismissed with it.
func (s *Store) DismissPromptDrift(id int64) (*PromptDrift, error) {
	drift, err := s.GetPromptDrift(id)
	if err != nil {
		return nil, err
	}
	if drift.Status != PromptDriftStatusOpen && drift.Status != PromptDriftStatusHeld {
		return nil, fmt.Errorf("drift %d is %s; only an open or held drift can be dismissed", id, drift.Status)
	}
	err = s.inPromptTransaction(func(tx *sql.Tx) error {
		for _, dismissedID := range append([]int64{id}, promptDriftTwinIDs(drift.Twins)...) {
			if _, err := tx.Exec(`UPDATE prompt_drifts SET status=?, resolved_at=? WHERE id=?`, PromptDriftStatusDismissed, now(), dismissedID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetPromptDrift(id)
}
