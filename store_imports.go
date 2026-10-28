package activitystore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const importColumns = `id, kind, sha256, source_machine, source_path, imported_at, finished_at, counts`

func scanImport(row rowScanner) (Import, error) {
	var imported Import
	var counts string
	if err := row.Scan(&imported.ID, &imported.Kind, &imported.Sha256, &imported.SourceMachine, &imported.SourcePath,
		&imported.ImportedAt, &imported.FinishedAt, &counts); err != nil {
		return Import{}, err
	}
	if err := json.Unmarshal([]byte(counts), &imported.Counts); err != nil {
		return Import{}, fmt.Errorf("import %s has counts that are not JSON: %w", imported.ID, err)
	}
	return imported, nil
}

func getImport(q queryer, id string) (Import, error) {
	imported, err := scanImport(q.QueryRow(`SELECT `+importColumns+` FROM imports WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, fmt.Errorf("%w: no import %s", ErrNotFound, id)
	}
	return imported, err
}

// GetImport reads one import.
func (s *Store) GetImport(id string) (Import, error) { return getImport(s.db, id) }

// ListImports reads every import, newest first.
func (s *Store) ListImports() ([]Import, error) {
	rows, err := s.db.Query(`SELECT ` + importColumns + ` FROM imports ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	imports := []Import{}
	for rows.Next() {
		imported, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		imports = append(imports, imported)
	}
	return imports, rows.Err()
}

// StartImport begins an import of a file, or answers the import already made
// for the same hash: finished, so there is nothing to send, or unfinished, so
// the importer resumes it.
func (s *Store) StartImport(start ImportStart) (ImportStarted, error) {
	if !knownWord(ImportKinds, start.Kind) {
		return ImportStarted{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, start.Kind, ImportKinds)
	}
	if !sha256Pattern.MatchString(start.Sha256) {
		return ImportStarted{}, fmt.Errorf("%w: sha256 must be 64 lowercase hex digits", ErrInvalid)
	}
	var answer ImportStarted
	err := s.inTransaction(func(tx *sql.Tx) error {
		existing, err := scanImport(tx.QueryRow(`SELECT `+importColumns+` FROM imports WHERE sha256 = ?`, start.Sha256))
		if err == nil {
			if existing.Kind != start.Kind {
				return fmt.Errorf("%w: this file was imported as %s (%s), not %s", ErrConflict, existing.Kind, existing.ID, start.Kind)
			}
			answer = ImportStarted{Import: existing, AlreadyFinished: existing.FinishedAt != 0}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, seq, err := nextID(tx, "imports", IDPrefixImport)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO imports (id, seq, kind, sha256, source_machine, source_path, imported_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, seq, start.Kind, start.Sha256, start.SourceMachine, start.SourcePath, now()); err != nil {
			return err
		}
		answer.Import, err = getImport(tx, id)
		return err
	})
	return answer, err
}

func validateImportActivity(index int, record ImportActivity) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: activity %d (%s %s): %s", ErrInvalid, index, record.Source, record.SourceRef, fmt.Sprintf(format, args...))
	}
	switch {
	case strings.TrimSpace(record.Product) == "":
		return fail("no product")
	case !knownWord(Actions, record.Action):
		return fail("action %q is not one of %v", record.Action, Actions)
	case !knownWord(Sources, record.Source):
		return fail("source %q is not one of %v", record.Source, Sources)
	case strings.TrimSpace(record.SourceRef) == "":
		return fail("no source_ref")
	case record.OccurredAt <= 0:
		return fail("occurred_at must be epoch seconds after 1970")
	case (record.Latitude == nil) != (record.Longitude == nil):
		return fail("latitude and longitude come together or not at all")
	}
	if record.Latitude != nil && (math.Abs(*record.Latitude) > 90 || math.Abs(*record.Longitude) > 180) {
		return fail("latitude %v, longitude %v is not on Earth", *record.Latitude, *record.Longitude)
	}
	for _, location := range record.Details.Locations {
		if (location.Latitude == nil) != (location.Longitude == nil) {
			return fail("a location's latitude and longitude come together or not at all")
		}
	}
	return nil
}

// ImportBatch writes one batch of an unfinished import in one transaction.
// One bad record is ErrInvalid naming it and writes nothing.
//
// A record whose (source, source_ref) is known is skipped. A record whose url
// and time match an activity is folded into it as a sighting when that
// activity came from another source, or from the same product in another
// import (a later export of the same history); then it is the same thing done
// once, seen twice. Anything else is a new activity.
func (s *Store) ImportBatch(importID string, batch ImportBatch) (ImportCounts, error) {
	decoded := make([][]decodedAttachment, len(batch.Activities))
	for index, record := range batch.Activities {
		if err := validateImportActivity(index, record); err != nil {
			return ImportCounts{}, err
		}
		for _, attachment := range record.Attachments {
			file, err := decodeAttachment(attachment)
			if err != nil {
				return ImportCounts{}, fmt.Errorf("activity %d: %w", index, err)
			}
			decoded[index] = append(decoded[index], file)
		}
	}
	var counts ImportCounts
	err := s.inTransaction(func(tx *sql.Tx) error {
		imported, err := getImport(tx, importID)
		if err != nil {
			return err
		}
		if imported.FinishedAt != 0 {
			return fmt.Errorf("%w: import %s is finished", ErrConflict, importID)
		}
		for index, record := range batch.Activities {
			if err := s.importOne(tx, importID, record, decoded[index], &counts); err != nil {
				return fmt.Errorf("activity %d (%s %s): %w", index, record.Source, record.SourceRef, err)
			}
		}
		total := imported.Counts
		total.Created += counts.Created
		total.Folded += counts.Folded
		total.Known += counts.Known
		total.AttachmentsStored += counts.AttachmentsStored
		encoded, err := json.Marshal(total)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE imports SET counts = ? WHERE id = ?`, string(encoded), importID)
		return err
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func (s *Store) importOne(tx *sql.Tx, importID string, record ImportActivity, attachments []decodedAttachment, counts *ImportCounts) error {
	var known int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sightings WHERE source = ? AND source_ref = ?`, record.Source, record.SourceRef).Scan(&known); err != nil {
		return err
	}
	if known > 0 {
		counts.Known++
		return nil
	}
	stamp := now()
	dedupeKey := DedupeKey(record.URL, record.OccurredAt)
	activityID := ""
	purged := false
	if dedupeKey != "" {
		var purgedAt int64
		err := tx.QueryRow(`SELECT a.id, a.purged_at FROM activities a
			WHERE a.dedupe_key = ? AND a.dedupe_key != '' AND (a.source != ? OR a.product = ?)
			AND NOT EXISTS (SELECT 1 FROM sightings g WHERE g.activity_id = a.id AND g.import_id = ?)
			ORDER BY a.seq LIMIT 1`, dedupeKey, record.Source, record.Product, importID).Scan(&activityID, &purgedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		purged = purgedAt != 0
	}
	if activityID != "" {
		counts.Folded++
	} else {
		id, seq, err := nextID(tx, "activities", IDPrefixActivity)
		if err != nil {
			return err
		}
		details, err := json.Marshal(record.Details)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO activities (id, seq, product, action, action_text, title, url, occurred_at, latitude, longitude,
			details, source, source_ref, import_id, dedupe_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, seq, record.Product, record.Action, record.ActionText, record.Title, record.URL, record.OccurredAt, record.Latitude, record.Longitude,
			string(details), record.Source, record.SourceRef, importID, dedupeKey, stamp, stamp); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO activities_fts (rowid, title, url, details) VALUES (?, ?, ?, ?)`,
			seq, record.Title, record.URL, searchableDetails(record.Details)); err != nil {
			return err
		}
		activityID = id
		counts.Created++
	}
	if _, err := tx.Exec(`INSERT INTO sightings (activity_id, source, source_ref, product, action, import_id, occurred_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, activityID, record.Source, record.SourceRef, record.Product, record.Action, importID, record.OccurredAt, stamp); err != nil {
		return err
	}
	if purged {
		// A purged activity takes no new files.
		return nil
	}
	for _, attachment := range attachments {
		stored, err := s.addAttachment(tx, activityID, attachment)
		if err != nil {
			return err
		}
		if stored {
			counts.AttachmentsStored++
		}
	}
	return nil
}

// FinishImport stamps an import finished; the same file is then a no-op.
func (s *Store) FinishImport(importID string) (Import, error) {
	var finished Import
	err := s.inTransaction(func(tx *sql.Tx) error {
		imported, err := getImport(tx, importID)
		if err != nil {
			return err
		}
		if imported.FinishedAt == 0 {
			if _, err := tx.Exec(`UPDATE imports SET finished_at = ? WHERE id = ?`, now(), importID); err != nil {
				return err
			}
		}
		finished, err = getImport(tx, importID)
		return err
	})
	return finished, err
}
