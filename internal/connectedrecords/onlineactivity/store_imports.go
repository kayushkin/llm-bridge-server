package onlineactivity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const importColumns = `seq, owner_principal_id, kind, sha256, source_machine, source_path, imported_at, finished_at, counts`

// scannedImport is an import and its number.
type scannedImport struct {
	Import
	seq int64
}

func scanImport(row rowScanner) (scannedImport, error) {
	var imported scannedImport
	var counts string
	if err := row.Scan(&imported.seq, &imported.OwnerPrincipalID, &imported.Kind, &imported.Sha256, &imported.SourceMachine, &imported.SourcePath,
		&imported.ImportedAt, &imported.FinishedAt, &counts); err != nil {
		return scannedImport{}, err
	}
	imported.ID = postgresstorage.FormatRecordID(IDPrefixImport, imported.seq)
	if err := json.Unmarshal([]byte(counts), &imported.Counts); err != nil {
		return scannedImport{}, fmt.Errorf("import %s has counts that are not JSON: %w", imported.ID, err)
	}
	return imported, nil
}

// getImport reads one import in scope; one out of scope is ErrNotFound, as a
// missing one is. lock takes the row for the transaction q is.
func getImport(ctx context.Context, q queryer, scope ownership.Scope, id string, lock bool) (scannedImport, error) {
	seq, err := numberOf(IDPrefixImport, id)
	if err != nil {
		return scannedImport{}, err
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return scannedImport{}, err
	}
	query := `SELECT ` + importColumns + ` FROM imports WHERE seq = $1 AND ` + condition
	if lock {
		query += ` FOR UPDATE`
	}
	imported, err := scanImport(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return scannedImport{}, fmt.Errorf("%w: no import %s", ErrNotFound, id)
	}
	return imported, err
}

// GetImport reads one import.
func (s *Store) GetImport(ctx context.Context, scope ownership.Scope, id string) (Import, error) {
	imported, err := getImport(ctx, s.db, scope, id, false)
	return imported.Import, err
}

// ListImports reads every import in scope, newest first.
func (s *Store) ListImports(ctx context.Context, scope ownership.Scope) ([]Import, error) {
	var args arguments
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+importColumns+` FROM imports WHERE `+condition+` ORDER BY seq DESC`, args...)
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
		imports = append(imports, imported.Import)
	}
	return imports, rows.Err()
}

// StartImport begins an import of a file, owned by ownerPrincipalID, or
// answers that owner's import already made for the same hash: finished, so
// there is nothing to send, or unfinished, so the importer resumes it.
func (s *Store) StartImport(ctx context.Context, ownerPrincipalID string, start ImportStart) (ImportStarted, error) {
	if err := ownership.CheckPrincipalID("the owner", ownerPrincipalID); err != nil {
		return ImportStarted{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !knownWord(ImportKinds, start.Kind) {
		return ImportStarted{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, start.Kind, ImportKinds)
	}
	if !sha256Pattern.MatchString(start.Sha256) {
		return ImportStarted{}, fmt.Errorf("%w: sha256 must be 64 lowercase hex digits", ErrInvalid)
	}
	var answer ImportStarted
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		existing, err := scanImport(tx.QueryRowContext(ctx, `SELECT `+importColumns+` FROM imports WHERE owner_principal_id = $1 AND sha256 = $2`,
			ownerPrincipalID, start.Sha256))
		if err == nil {
			if existing.Kind != start.Kind {
				return fmt.Errorf("%w: this file was imported as %s (%s), not %s", ErrConflict, existing.Kind, existing.ID, start.Kind)
			}
			answer = ImportStarted{Import: existing.Import, AlreadyFinished: existing.FinishedAt != 0}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		created, err := scanImport(tx.QueryRowContext(ctx, `INSERT INTO imports (owner_principal_id, kind, sha256, source_machine, source_path, imported_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+importColumns,
			ownerPrincipalID, start.Kind, start.Sha256, start.SourceMachine, start.SourcePath, now()))
		answer.Import = created.Import
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

// ImportBatch writes one batch of an unfinished import in scope in one
// transaction, into the import owner's activities. One bad record is
// ErrInvalid naming it and writes nothing; a file it handed file-store before
// the transaction failed is destroyed again.
//
// A record whose (source, source_ref) the owner already holds is skipped. A
// record whose url and time match one of the owner's activities is folded
// into it as a sighting when that activity came from another source, or from
// the same product in another import (a later export of the same history);
// then it is the same thing done once, seen twice. Anything else is a new
// activity.
func (s *Store) ImportBatch(ctx context.Context, scope ownership.Scope, importID string, batch ImportBatch) (ImportCounts, error) {
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
	var keptFiles []string
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		imported, err := getImport(ctx, tx, scope, importID, true)
		if err != nil {
			return err
		}
		if imported.FinishedAt != 0 {
			return fmt.Errorf("%w: import %s is finished", ErrConflict, importID)
		}
		for index, record := range batch.Activities {
			if err := s.importOne(ctx, tx, imported, record, decoded[index], &counts, &keptFiles); err != nil {
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
		_, err = tx.ExecContext(ctx, `UPDATE imports SET counts = $1 WHERE seq = $2`, string(encoded), imported.seq)
		return err
	})
	if err != nil {
		return ImportCounts{}, errors.Join(err, s.purgeFiles(ctx, keptFiles))
	}
	return counts, nil
}

// purgeFiles destroys files a failed transaction handed file-store, so no
// row-less bytes are left behind; any it cannot destroy are named.
func (s *Store) purgeFiles(ctx context.Context, fileIDs []string) error {
	var errs []error
	for _, fileID := range fileIDs {
		if err := s.files.Purge(ctx, fileID); err != nil {
			errs = append(errs, fmt.Errorf("file-store keeps %s, which no attachment names: destroying it: %w", fileID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) importOne(ctx context.Context, tx *sql.Tx, imported scannedImport, record ImportActivity, attachments []decodedAttachment,
	counts *ImportCounts, keptFiles *[]string) error {
	owner := imported.OwnerPrincipalID
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sightings WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		owner, record.Source, record.SourceRef).Scan(&known); err != nil {
		return err
	}
	if known > 0 {
		counts.Known++
		return nil
	}
	stamp := now()
	dedupeKey := DedupeKey(record.URL, record.OccurredAt)
	var activitySeq int64
	purged := false
	if dedupeKey != "" {
		var purgedAt int64
		err := tx.QueryRowContext(ctx, `SELECT a.seq, a.purged_at FROM activities a
			WHERE a.owner_principal_id = $1 AND a.dedupe_key = $2 AND (a.source <> $3 OR a.product = $4)
			AND NOT EXISTS (SELECT 1 FROM sightings g WHERE g.activity_seq = a.seq AND g.import_seq = $5)
			ORDER BY a.seq LIMIT 1`, owner, dedupeKey, record.Source, record.Product, imported.seq).Scan(&activitySeq, &purgedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		purged = purgedAt != 0
	}
	if activitySeq != 0 {
		counts.Folded++
	} else {
		details, err := json.Marshal(record.Details)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO activities (owner_principal_id, product, action, action_text, title, url, occurred_at, latitude, longitude,
			details, source, source_ref, import_seq, dedupe_key, created_at, updated_at, search_document)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, `+searchDocumentSQL("$5", "$6", "$17")+`)
			RETURNING seq`,
			owner, record.Product, record.Action, record.ActionText, record.Title, record.URL, record.OccurredAt, record.Latitude, record.Longitude,
			string(details), record.Source, record.SourceRef, imported.seq, dedupeKey, stamp, stamp, searchableDetails(record.Details)).Scan(&activitySeq); err != nil {
			return err
		}
		counts.Created++
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sightings (owner_principal_id, activity_seq, source, source_ref, product, action, import_seq, occurred_at, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, owner, activitySeq, record.Source, record.SourceRef, record.Product, record.Action,
		imported.seq, record.OccurredAt, stamp); err != nil {
		return err
	}
	if purged {
		// A purged activity takes no new files.
		return nil
	}
	for _, attachment := range attachments {
		stored, err := s.addAttachment(ctx, tx, owner, activitySeq, attachment, keptFiles)
		if err != nil {
			return err
		}
		if stored {
			counts.AttachmentsStored++
		}
	}
	return nil
}

// FinishImport stamps an import in scope finished; the same file is then a
// no-op.
func (s *Store) FinishImport(ctx context.Context, scope ownership.Scope, importID string) (Import, error) {
	var finished Import
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		imported, err := getImport(ctx, tx, scope, importID, true)
		if err != nil {
			return err
		}
		if imported.FinishedAt == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE imports SET finished_at = $1 WHERE seq = $2`, now(), imported.seq); err != nil {
				return err
			}
		}
		refreshed, err := getImport(ctx, tx, scope, importID, false)
		finished = refreshed.Import
		return err
	})
	return finished, err
}
