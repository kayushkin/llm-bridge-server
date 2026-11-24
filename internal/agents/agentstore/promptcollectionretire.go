package agentstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrPromptCollectionRetired is returned when a call would reuse a retired
// collection: EnsurePromptCollection for its root, or retiring it again.
var ErrPromptCollectionRetired = errors.New("prompt collection retired")

// ErrPromptCollectionRetireRefused is returned when a collection cannot be
// retired as it stands. The HTTP layer answers it with 409.
var ErrPromptCollectionRetireRefused = errors.New("prompt collection cannot be retired")

// PromptCollectionRetirement reports what RetirePromptCollection changed.
type PromptCollectionRetirement struct {
	CollectionID       int64    `json:"collection_id"`
	RootPath           string   `json:"root_path"`
	DeletedSectionIDs  []int64  `json:"deleted_section_ids"`
	RemovedOutputPaths []string `json:"removed_output_paths"`
	RetiredAt          int64    `json:"retired_at"`
}

// RetirePromptCollection takes a project or context collection out of use:
// it deletes every section, each with a revision that keeps its text, drops
// the outputs so nothing renders to disk again, and stamps retired_at so lists
// leave the collection out and EnsurePromptCollection refuses its root. The
// row itself stays, so its revisions and id still resolve. Nothing on disk is
// touched: files the collection rendered stay as they are.
//
// It is for a collection that should never have existed, such as one an import
// made for a copy of a repo. It refuses the global collection, one already
// retired, and one with an open or held drift, whose edit must be settled
// first. note is required: it is recorded on every section's delete revision.
func (s *Store) RetirePromptCollection(collectionID int64, note string) (*PromptCollectionRetirement, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, fmt.Errorf("%w: a note saying why is required; it is kept on every deleted section's revision", ErrPromptCollectionRetireRefused)
	}
	collection, err := s.getPromptCollection(collectionID)
	if err != nil {
		return nil, err
	}
	if collection.RetiredAt != 0 {
		return nil, fmt.Errorf("%w: collection %d was retired at %d", ErrPromptCollectionRetired, collectionID, collection.RetiredAt)
	}
	if collection.Scope == promptCollectionScopeGlobal {
		return nil, fmt.Errorf("%w: collection %d is the host's global collection", ErrPromptCollectionRetireRefused, collectionID)
	}
	// Read before the transaction: the store's reads go through s.db, which a
	// transaction holding the only write connection would block.
	sections, err := s.ListPromptSections(collectionID)
	if err != nil {
		return nil, err
	}

	retirement := &PromptCollectionRetirement{CollectionID: collectionID, RootPath: collection.RootPath,
		DeletedSectionIDs: []int64{}, RemovedOutputPaths: []string{}}
	err = s.inPromptTransaction(func(tx *sql.Tx) error {
		var waitingDrifts int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM prompt_drifts WHERE collection_id=? AND status IN (?, ?)`,
			collectionID, PromptDriftStatusOpen, PromptDriftStatusHeld).Scan(&waitingDrifts); err != nil {
			return err
		}
		if waitingDrifts > 0 {
			return fmt.Errorf("%w: collection %d has %d open or held drifts; apply or dismiss them first", ErrPromptCollectionRetireRefused, collectionID, waitingDrifts)
		}
		var sectionsNow int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, collectionID).Scan(&sectionsNow); err != nil {
			return err
		}
		if sectionsNow != len(sections) {
			return fmt.Errorf("%w: collection %d changed while it was being retired (%d sections read, %d now); try again", ErrPromptCollectionRetireRefused, collectionID, len(sections), sectionsNow)
		}
		for i := range sections {
			if err := deletePromptSection(tx, &sections[i], promptRevisionContext{Source: PromptRevisionSourceUI, Note: note}); err != nil {
				return err
			}
			retirement.DeletedSectionIDs = append(retirement.DeletedSectionIDs, sections[i].ID)
		}

		rows, err := tx.Query(`SELECT relative_path FROM prompt_collection_outputs WHERE collection_id=? ORDER BY id`, collectionID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var relativePath string
			if err := rows.Scan(&relativePath); err != nil {
				rows.Close()
				return err
			}
			retirement.RemovedOutputPaths = append(retirement.RemovedOutputPaths, relativePath)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM prompt_collection_outputs WHERE collection_id=?`, collectionID); err != nil {
			return err
		}

		retirement.RetiredAt = now()
		_, err = tx.Exec(`UPDATE prompt_collections SET retired_at=?, updated_at=? WHERE id=?`, retirement.RetiredAt, retirement.RetiredAt, collectionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return retirement, nil
}

// retiredPromptCollectionRoots is every root a retired collection had, by
// scope, so an import can leave its files alone instead of failing on them.
func (s *Store) retiredPromptCollectionRoots() (map[string]map[string]int64, error) {
	rows, err := s.db.Query(`SELECT id, scope, root_path FROM prompt_collections WHERE retired_at<>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := map[string]map[string]int64{}
	for rows.Next() {
		var id int64
		var scope, root string
		if err := rows.Scan(&id, &scope, &root); err != nil {
			return nil, err
		}
		if roots[scope] == nil {
			roots[scope] = map[string]int64{}
		}
		roots[scope][root] = id
	}
	return roots, rows.Err()
}
