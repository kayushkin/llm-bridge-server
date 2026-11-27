package items

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"

	_ "modernc.org/sqlite"
)

// Copying rows from the SQLite file a standalone noteboard kept. Which row
// goes to which module is noteboardsplit's to say; this file only reads the
// file and writes the rows it is handed a transaction for.

// standaloneColumns are the columns of each table of the SQLite database the
// standalone noteboard kept, items_fts aside, which is an index of items and
// holds nothing of its own. The import refuses a file whose tables have any
// other columns: a column it does not know is data it would drop.
var standaloneColumns = map[string][]string{
	"items": {"id", "type", "title", "body", "tags", "priority", "rank", "status", "list_id", "due_at", "parent_id", "links",
		"created_by", "created_at", "updated_at", "deleted_at", "schedule", "held_at", "hold_reason", "auto_hold_at_usd"},
	"item_revisions": {"id", "item_id", "title", "body", "tags", "status", "priority", "list_id", "parent_id", "links",
		"deleted_at", "reason", "replaced_at"},
}

// StandaloneCopyCounts says what one copy took from a standalone file.
type StandaloneCopyCounts struct {
	Items          int   `json:"items"`
	DeletedItems   int   `json:"deleted_items"`
	HeldItems      int   `json:"held_items"`
	ScheduledItems int   `json:"scheduled_items"`
	Revisions      int   `json:"revisions"`
	LastRevisionID int64 `json:"last_revision_id"`
}

// OpenStandaloneDatabase opens a standalone noteboard's SQLite file to read,
// with the pure-Go driver the server binary carries, and refuses a file whose
// tables have columns the copy does not know. The caller closes it.
func OpenStandaloneDatabase(ctx context.Context, sqlitePath string) (*sql.DB, error) {
	if _, err := os.Stat(sqlitePath); err != nil {
		return nil, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	for table, want := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table, want); err != nil {
			source.Close()
			return nil, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	return source, nil
}

// CopyStandaloneRows copies, from a standalone noteboard's file, the items
// keepItem takes and the revisions keepRevision takes, into the items and
// item_revisions tables tx resolves bare names to, as they are: ids, times,
// holds, rules and soft-deleted rows. Nothing is checked against the types a
// store keeps: the caller chooses which rows go where.
//
// Two things change on the way, both from what Postgres keeps: a time keeps
// microseconds, so the nanoseconds the standalone store wrote are cut off, and
// a due date keeps its instant and its offset, which is what the item gave
// back anyway. It refuses a file it cannot copy whole: tags or links that are
// not a list of strings, a schedule the store could not read, text holding
// U+0000, or a NULL where the store kept none.
func CopyStandaloneRows(ctx context.Context, source *sql.DB, tx *sql.Tx, keepItem func(id, itemType string) bool, keepRevision func(revisionID int64, itemID string) bool) (StandaloneCopyCounts, error) {
	var counts StandaloneCopyCounts
	if err := copyStandaloneItems(ctx, source, tx, keepItem, &counts); err != nil {
		return counts, err
	}
	if err := copyStandaloneRevisions(ctx, source, tx, keepRevision, &counts); err != nil {
		return counts, err
	}
	return counts, nil
}

// StandaloneItemTypes is every item id of a standalone file with its type.
func StandaloneItemTypes(ctx context.Context, source *sql.DB) (map[string]string, error) {
	rows, err := source.QueryContext(ctx, "SELECT id, type FROM items")
	if err != nil {
		return nil, fmt.Errorf("reading item types: %w", err)
	}
	defer rows.Close()
	types := map[string]string{}
	for rows.Next() {
		var id, itemType string
		if err := rows.Scan(&id, &itemType); err != nil {
			return nil, err
		}
		types[id] = itemType
	}
	return types, rows.Err()
}

// StandaloneRevisionItemIDs is every item id a revision of a standalone file
// names, each once.
func StandaloneRevisionItemIDs(ctx context.Context, source *sql.DB) ([]string, error) {
	rows, err := source.QueryContext(ctx, "SELECT DISTINCT item_id FROM item_revisions")
	if err != nil {
		return nil, fmt.Errorf("reading the items revisions name: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		have = append(have, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sortedWant := append([]string(nil), want...)
	sort.Strings(have)
	sort.Strings(sortedWant)
	if strings.Join(have, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %v; the import copies exactly %v", table, have, sortedWant)
	}
	return nil
}

// importedTime is a stored time as Postgres keeps it: in UTC, to the microsecond.
func importedTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func importedNullTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return importedTime(value.Time)
}

// checkedStringList is a tags or links column, refused unless it is a JSON list
// of strings.
func checkedStringList(raw, column, rowName string) (string, error) {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return "", fmt.Errorf("%s: %s is %q, which is not a list of strings: %w", rowName, column, raw, err)
	}
	return marshalStringList(list)
}

func copyStandaloneItems(ctx context.Context, source *sql.DB, tx *sql.Tx, keepItem func(id, itemType string) bool, counts *StandaloneCopyCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, type, title, body, tags, priority, rank, status, list_id, due_at, parent_id, links,
		created_by, created_at, updated_at, deleted_at, schedule, held_at, hold_reason, auto_hold_at_usd FROM items ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("reading items: %w", err)
	}
	defer rows.Close()
	insert, err := tx.PrepareContext(ctx, "INSERT INTO items ("+insertCols+", deleted_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)")
	if err != nil {
		return err
	}
	defer insert.Close()
	for rows.Next() {
		var id, itemType, title, body, tags, status, listID, links, createdBy, holdReason string
		var priority int
		var rank float64
		var dueAt, deletedAt, heldAt sql.NullTime
		var createdAt, updatedAt time.Time
		var parentID, schedule sql.NullString
		var autoHoldAtUSD sql.NullFloat64
		if err := rows.Scan(&id, &itemType, &title, &body, &tags, &priority, &rank, &status, &listID, &dueAt, &parentID, &links,
			&createdBy, &createdAt, &updatedAt, &deletedAt, &schedule, &heldAt, &holdReason, &autoHoldAtUSD); err != nil {
			return fmt.Errorf("reading the item after %q: %w", id, err)
		}
		if !keepItem(id, itemType) {
			continue
		}
		rowName := "item " + id
		if err := refuseNulCharacters(map[string][]string{"id": {id}, "type": {itemType}, "title": {title}, "body": {body}, "tags": scheduleText(tags),
			"status": {status}, "list_id": {listID}, "parent_id": {parentID.String}, "links": scheduleText(links), "created_by": {createdBy},
			"schedule": scheduleText(schedule.String), "hold_reason": {holdReason}}); err != nil {
			return fmt.Errorf("%s: %w; change it in the standalone store before importing", rowName, err)
		}
		if tags, err = checkedStringList(tags, "tags", rowName); err != nil {
			return err
		}
		if links, err = checkedStringList(links, "links", rowName); err != nil {
			return err
		}
		var scheduleValue any
		if schedule.Valid && schedule.String != "" {
			var rule model.Schedule
			if err := json.Unmarshal([]byte(schedule.String), &rule); err != nil {
				return fmt.Errorf("%s has an unreadable schedule: %w", rowName, err)
			}
			scheduleValue = schedule.String
			counts.ScheduledItems++
		}
		var dueAtValue, dueAtOffsetSeconds any
		if dueAt.Valid {
			dueAtValue, dueAtOffsetSeconds = dueAtColumns(&dueAt.Time)
		}
		var parentValue any
		if parentID.Valid {
			parentValue = parentID.String
		}
		var autoHoldValue any
		if autoHoldAtUSD.Valid {
			autoHoldValue = autoHoldAtUSD.Float64
		}
		if _, err := insert.ExecContext(ctx, id, itemType, title, body, tags, priority, rank, status, listID, dueAtValue, dueAtOffsetSeconds,
			parentValue, links, createdBy, importedTime(createdAt), importedTime(updatedAt), scheduleValue, importedNullTime(heldAt),
			holdReason, autoHoldValue, importedNullTime(deletedAt)); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.Items++
		if deletedAt.Valid {
			counts.DeletedItems++
		}
		if heldAt.Valid {
			counts.HeldItems++
		}
	}
	return rows.Err()
}

func copyStandaloneRevisions(ctx context.Context, source *sql.DB, tx *sql.Tx, keepRevision func(revisionID int64, itemID string) bool, counts *StandaloneCopyCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, item_id, title, body, tags, status, priority, list_id, parent_id, links,
		deleted_at, reason, replaced_at FROM item_revisions ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading item_revisions: %w", err)
	}
	defer rows.Close()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO item_revisions
		(id, item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at)
		OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for rows.Next() {
		var id int64
		var itemID, title, body, tags, status, listID, links, reason string
		var priority int
		var parentID sql.NullString
		var deletedAt sql.NullTime
		var replacedAt time.Time
		if err := rows.Scan(&id, &itemID, &title, &body, &tags, &status, &priority, &listID, &parentID, &links,
			&deletedAt, &reason, &replacedAt); err != nil {
			return fmt.Errorf("reading the revision after %d: %w", counts.LastRevisionID, err)
		}
		if !keepRevision(id, itemID) {
			continue
		}
		rowName := fmt.Sprintf("revision %d of item %s", id, itemID)
		if err := refuseNulCharacters(map[string][]string{"item_id": {itemID}, "title": {title}, "body": {body}, "tags": scheduleText(tags),
			"status": {status}, "list_id": {listID}, "parent_id": {parentID.String}, "links": scheduleText(links), "reason": {reason}}); err != nil {
			return fmt.Errorf("%s: %w; a revision cannot be changed, so it has to be decided by hand before importing", rowName, err)
		}
		if tags, err = checkedStringList(tags, "tags", rowName); err != nil {
			return err
		}
		if links, err = checkedStringList(links, "links", rowName); err != nil {
			return err
		}
		var parentValue any
		if parentID.Valid {
			parentValue = parentID.String
		}
		if _, err := insert.ExecContext(ctx, id, itemID, title, body, tags, status, priority, listID, parentValue, links,
			importedNullTime(deletedAt), reason, importedTime(replacedAt)); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.Revisions++
		counts.LastRevisionID = id
	}
	return rows.Err()
}

// LastStandaloneRevisionNumberHandedOut is the highest revision number the standalone
// store ever gave out, which AUTOINCREMENT keeps in sqlite_sequence even after
// the row is gone. A file with no such record has handed out none beyond the
// rows it holds.
func LastStandaloneRevisionNumberHandedOut(ctx context.Context, source *sql.DB) (int64, error) {
	var hasSequenceTable int
	if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&hasSequenceTable); err != nil {
		return 0, fmt.Errorf("looking for sqlite_sequence: %w", err)
	}
	if hasSequenceTable == 0 {
		return 0, nil
	}
	var last sql.NullInt64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'item_revisions'`).Scan(&last)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the last revision number: %w", err)
	}
	return last.Int64, nil
}
