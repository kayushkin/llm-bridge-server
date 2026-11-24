package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// What the import reads: every table of the SQLite file the standalone
// kanban-store kept, with every column it had after the last of its own
// migrations, in the order the tables must be written for their foreign keys.
//
// Each column says how a value is read from SQLite and written to Postgres.
// The standalone store wrote DATETIME columns through the mattn/go-sqlite3
// driver, as text in one of that driver's layouts. The import reads the file
// with the pure-Go driver the rest of this server uses (the server is built
// with no C), which hands such a column back as a time.Time when it can parse
// it, and as text when not; the text is parsed in mattn's own layouts, read
// as UTC when it carries no offset, as mattn read it.

type importedColumnKind int

const (
	// importedText is text, NULL kept as NULL.
	importedText importedColumnKind = iota
	// importedNumber is an integer or a real, NULL kept as NULL.
	importedNumber
	// importedTime is a DATETIME, cut to the microsecond Postgres keeps.
	importedTime
	// importedFlag is SQLite's 0 or 1, written as a boolean.
	importedFlag
)

type importedColumn struct {
	name string
	kind importedColumnKind
}

type importedTable struct {
	name    string
	columns []importedColumn
}

func textColumn(name string) importedColumn   { return importedColumn{name, importedText} }
func numberColumn(name string) importedColumn { return importedColumn{name, importedNumber} }
func timeColumn(name string) importedColumn   { return importedColumn{name, importedTime} }
func flagColumn(name string) importedColumn   { return importedColumn{name, importedFlag} }

var standaloneTables = []importedTable{
	{"boards", []importedColumn{textColumn("id"), textColumn("name"), textColumn("description"), numberColumn("archived"), timeColumn("created_at"), timeColumn("updated_at"),
		textColumn("business_hours"), textColumn("default_principal_id"), textColumn("default_agent_id"), textColumn("default_instance_id"), textColumn("classifier"),
		textColumn("default_bundle_id"), textColumn("assignment_pool"), textColumn("organization_id")}},
	{"columns", []importedColumn{textColumn("id"), textColumn("board_id"), textColumn("name"), numberColumn("position"), textColumn("color"), numberColumn("wip_limit"),
		textColumn("auto_status"), timeColumn("created_at"), timeColumn("updated_at"), textColumn("budget_clock_state"), textColumn("lifecycle_state"), textColumn("work_state")}},
	{"placements", []importedColumn{textColumn("card_id"), textColumn("board_id"), textColumn("column_id"), numberColumn("position"), timeColumn("created_at"), timeColumn("updated_at")}},
	{"card_links", []importedColumn{textColumn("id"), textColumn("card_id"), textColumn("entity_type"), textColumn("entity_ref"), textColumn("label"), timeColumn("created_at")}},
	{"card_assignments", []importedColumn{textColumn("card_id"), textColumn("principal_id"), textColumn("assigned_by"), timeColumn("created_at")}},
	{"entity_tags", []importedColumn{textColumn("entity_type"), textColumn("entity_ref"), textColumn("tag"), timeColumn("created_at")}},
	{"card_events", []importedColumn{textColumn("id"), textColumn("card_id"), textColumn("board_id"), textColumn("kind"), textColumn("clock_state"), textColumn("actor"), textColumn("summary"),
		textColumn("from_column_id"), textColumn("to_column_id"), textColumn("note_id"), textColumn("detail"), timeColumn("occurred_at"), timeColumn("recorded_at"), textColumn("supersedes_event_id")}},
	{"card_notes", []importedColumn{textColumn("id"), textColumn("card_id"), textColumn("board_id"), textColumn("kind"), textColumn("body"), textColumn("actor"), timeColumn("created_at"), textColumn("visibility")}},
	{"board_priority_levels", []importedColumn{textColumn("board_id"), numberColumn("priority_value"), textColumn("label"), numberColumn("budget_seconds"), numberColumn("default_auto_hold_at_usd")}},
	// The message tables kept their times as RFC 3339 text, and keep them so.
	{"message_triggers", []importedColumn{textColumn("id"), textColumn("board_id"), textColumn("name"), textColumn("event_kind"), textColumn("to_column_id"), numberColumn("priority_value"),
		textColumn("recipient_user_id"), textColumn("recipient_display_name"), textColumn("message_template"), flagColumn("enabled"), textColumn("created_at"), textColumn("updated_at")}},
	{"message_deliveries", []importedColumn{textColumn("id"), textColumn("trigger_id"), textColumn("board_id"), textColumn("event_id"), textColumn("card_id"), textColumn("recipient_user_id"),
		textColumn("rendered_message"), textColumn("status"), textColumn("error"), textColumn("room_id"), textColumn("matrix_event_id"), textColumn("created_at")}},
	{"board_tag_rules", []importedColumn{textColumn("id"), textColumn("board_id"), numberColumn("position"), textColumn("tags"), textColumn("default_principal_id"), textColumn("default_agent_id"),
		textColumn("default_instance_id"), textColumn("default_bundle_id"), timeColumn("created_at"), timeColumn("updated_at")}},
	{"tickets", []importedColumn{textColumn("card_id"), textColumn("requester_principal_id"), textColumn("channel"), timeColumn("created_at"), timeColumn("updated_at"),
		textColumn("source_entity_type"), textColumn("source_entity_ref")}},
	{"card_attachments", []importedColumn{textColumn("file_id"), textColumn("card_id"), textColumn("visibility"), textColumn("attached_by"), timeColumn("created_at")}},
	{"classification_identifiers", []importedColumn{numberColumn("sequence"), textColumn("id"), textColumn("kind"), textColumn("board_id"), textColumn("axis_id"), timeColumn("created_at")}},
	{"classification_taxonomy_revisions", []importedColumn{textColumn("board_id"), numberColumn("revision"), textColumn("taxonomy"), textColumn("semantic_digest"), timeColumn("created_at"), textColumn("created_by")}},
	{"classification_taxonomy_migrations", []importedColumn{textColumn("board_id"), textColumn("axis_name"), textColumn("value_name"), textColumn("assigned_id"), timeColumn("migrated_at")}},
	{"classification_policy_revisions", []importedColumn{textColumn("board_id"), numberColumn("revision"), textColumn("axis_policies"), numberColumn("taxonomy_revision"), timeColumn("created_at"), textColumn("created_by")}},
	{"classification_decisions", []importedColumn{textColumn("id"), numberColumn("sequence"), textColumn("board_id"), textColumn("organization_id"), textColumn("card_id"), textColumn("publication_key"),
		textColumn("publication_digest"), textColumn("operation_id"), textColumn("checkpoint_id"), numberColumn("attempt_number"), textColumn("initiating_principal_id"), textColumn("source_digest"),
		textColumn("source_revision"), textColumn("source_snapshot"), timeColumn("source_removed_at"), numberColumn("taxonomy_revision"), textColumn("taxonomy_digest"), numberColumn("policy_revision"),
		textColumn("prompt_revision"), textColumn("model"), textColumn("origin"), textColumn("selections"), textColumn("rationale"), numberColumn("model_reported_confidence"), flagColumn("needs_review"),
		textColumn("review_reasons"), timeColumn("completed_at"), timeColumn("published_at")}},
	{"classification_reviews", []importedColumn{textColumn("id"), textColumn("decision_id"), textColumn("board_id"), textColumn("card_id"), textColumn("reviewer_principal_id"), textColumn("action"),
		textColumn("axis_ids"), textColumn("corrections"), textColumn("explanation"), textColumn("supersedes_review_ids"), numberColumn("expected_labels_revision"), numberColumn("resulting_labels_revision"),
		numberColumn("taxonomy_revision"), numberColumn("policy_revision"), textColumn("idempotency_key"), textColumn("request_digest"), timeColumn("created_at")}},
	{"classification_labels", []importedColumn{textColumn("board_id"), textColumn("card_id"), textColumn("axis_id"), textColumn("value_ids"), textColumn("origin_decision_id"), textColumn("origin_review_id"), timeColumn("updated_at")}},
	{"classification_label_revisions", []importedColumn{textColumn("board_id"), textColumn("card_id"), numberColumn("revision"), timeColumn("updated_at")}},
}

// ImportCounts says what an import copied: the rows of each table, and where
// the two numberings stood.
type ImportCounts struct {
	Boards                           int `json:"boards"`
	Columns                          int `json:"columns"`
	Placements                       int `json:"placements"`
	CardLinks                        int `json:"card_links"`
	CardAssignments                  int `json:"card_assignments"`
	EntityTags                       int `json:"entity_tags"`
	CardEvents                       int `json:"card_events"`
	CardNotes                        int `json:"card_notes"`
	BoardPriorityLevels              int `json:"board_priority_levels"`
	MessageTriggers                  int `json:"message_triggers"`
	MessageDeliveries                int `json:"message_deliveries"`
	BoardTagRules                    int `json:"board_tag_rules"`
	Tickets                          int `json:"tickets"`
	CardAttachments                  int `json:"card_attachments"`
	ClassificationIdentifiers        int `json:"classification_identifiers"`
	ClassificationTaxonomyRevisions  int `json:"classification_taxonomy_revisions"`
	ClassificationTaxonomyMigrations int `json:"classification_taxonomy_migrations"`
	ClassificationPolicyRevisions    int `json:"classification_policy_revisions"`
	ClassificationDecisions          int `json:"classification_decisions"`
	ClassificationReviews            int `json:"classification_reviews"`
	ClassificationLabels             int `json:"classification_labels"`
	ClassificationLabelRevisions     int `json:"classification_label_revisions"`
	// LastClassificationIdentifierSequence is the highest axis or value
	// number the file had handed out; the next one minted is the one after.
	LastClassificationIdentifierSequence int64 `json:"last_classification_identifier_sequence"`
	// LastClassificationDecisionSequence is the highest decision sequence copied.
	LastClassificationDecisionSequence int64 `json:"last_classification_decision_sequence"`
}

// countOf is the field of counts that holds table's rows.
func (counts *ImportCounts) countOf(table string) *int {
	fields := map[string]*int{
		"boards": &counts.Boards, "columns": &counts.Columns, "placements": &counts.Placements, "card_links": &counts.CardLinks,
		"card_assignments": &counts.CardAssignments, "entity_tags": &counts.EntityTags, "card_events": &counts.CardEvents,
		"card_notes": &counts.CardNotes, "board_priority_levels": &counts.BoardPriorityLevels, "message_triggers": &counts.MessageTriggers,
		"message_deliveries": &counts.MessageDeliveries, "board_tag_rules": &counts.BoardTagRules, "tickets": &counts.Tickets,
		"card_attachments": &counts.CardAttachments, "classification_identifiers": &counts.ClassificationIdentifiers,
		"classification_taxonomy_revisions":  &counts.ClassificationTaxonomyRevisions,
		"classification_taxonomy_migrations": &counts.ClassificationTaxonomyMigrations,
		"classification_policy_revisions":    &counts.ClassificationPolicyRevisions,
		"classification_decisions":           &counts.ClassificationDecisions, "classification_reviews": &counts.ClassificationReviews,
		"classification_labels": &counts.ClassificationLabels, "classification_label_revisions": &counts.ClassificationLabelRevisions,
	}
	field, known := fields[table]
	if !known {
		panic("kanban-store import: ImportCounts has no field for table " + table)
	}
	return field
}

// ImportStandaloneDatabase copies every row of every table from the SQLite
// file of a standalone kanban-store into the module behind database, as they
// are: every id, every time and every value. It runs in one transaction, so it
// copies everything or nothing.
//
// One thing changes on the way, from what Postgres keeps: a DATETIME keeps
// microseconds, so the nanoseconds the standalone store wrote are cut off. The
// message tables' times were text, and are copied as text.
//
// It refuses a module that already holds any row, so it can never mix two
// stores or run twice, and a file it cannot copy whole: a table missing, or
// with a column more or less than the store's last schema. A file the
// standalone store has not opened since an older release is refused for the
// columns it lacks; opening it once with that store brings it up to date.
// Afterwards classification ids number on from the highest the file handed
// out, so none is handed out twice.
func ImportStandaloneDatabase(ctx context.Context, database *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite3", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for _, table := range standaloneTables {
		if err := checkStandaloneColumns(ctx, source, table); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, database, func(tx *sql.Tx) error {
		for _, table := range standaloneTables {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table.name, err)
			}
			if held > 0 {
				return fmt.Errorf("the kanban-store module already holds %d rows in %s; the import copies into an empty module only", held, table.name)
			}
		}
		for _, table := range standaloneTables {
			copied, err := importTable(ctx, source, tx, table)
			if err != nil {
				return err
			}
			*counts.countOf(table.name) = copied
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) FROM classification_decisions").Scan(&counts.LastClassificationDecisionSequence); err != nil {
			return err
		}
		lastHandedOut, err := lastClassificationIdentifierHandedOut(ctx, source)
		if err != nil {
			return err
		}
		counts.LastClassificationIdentifierSequence = lastHandedOut
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "classification_identifiers", "sequence", lastHandedOut)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// checkStandaloneColumns refuses a table whose columns are not exactly the
// ones the import copies.
func checkStandaloneColumns(ctx context.Context, source *sql.DB, table importedTable) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table.name)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table.name, err)
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
	if len(have) == 0 {
		return fmt.Errorf("it has no table %s; open it once with the standalone kanban-store, which makes every table, then import it", table.name)
	}
	var missing, unknown []string
	for _, column := range table.columns {
		if !slices.Contains(have, column.name) {
			missing = append(missing, column.name)
		}
	}
	for _, name := range have {
		if !slices.ContainsFunc(table.columns, func(column importedColumn) bool { return column.name == name }) {
			unknown = append(unknown, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("table %s lacks columns %s; open it once with the standalone kanban-store, which adds them, then import it",
			table.name, strings.Join(missing, ", "))
	}
	if len(unknown) > 0 {
		return fmt.Errorf("table %s has columns %s that the import does not copy; it copies only what the store's last schema holds (a boards.taxonomy column is a name-only taxonomy the standalone store moves into revisions the first time it opens the file)",
			table.name, strings.Join(unknown, ", "))
	}
	return nil
}

// importTable copies every row of one table and answers how many.
func importTable(ctx context.Context, source *sql.DB, tx *sql.Tx, table importedTable) (int, error) {
	names := make([]string, len(table.columns))
	placeholders := make([]string, len(table.columns))
	for index, column := range table.columns {
		names[index] = column.name
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	list := strings.Join(names, ", ")
	insert, err := tx.PrepareContext(ctx, "INSERT INTO "+table.name+" ("+list+") VALUES ("+strings.Join(placeholders, ", ")+")")
	if err != nil {
		return 0, fmt.Errorf("preparing the copy of %s: %w", table.name, err)
	}
	defer insert.Close()
	rows, err := source.QueryContext(ctx, "SELECT "+list+" FROM "+table.name+" ORDER BY rowid")
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", table.name, err)
	}
	defer rows.Close()
	copied := 0
	read := make([]any, len(table.columns))
	destinations := make([]any, len(table.columns))
	for index := range read {
		destinations[index] = &read[index]
	}
	for rows.Next() {
		if err := rows.Scan(destinations...); err != nil {
			return copied, fmt.Errorf("reading row %d of %s: %w", copied+1, table.name, err)
		}
		values := make([]any, len(table.columns))
		for index, column := range table.columns {
			value, err := importedValue(column, read[index])
			if err != nil {
				return copied, fmt.Errorf("%s row %d: %w", table.name, copied+1, err)
			}
			values[index] = value
		}
		if _, err := insert.ExecContext(ctx, values...); err != nil {
			return copied, fmt.Errorf("copying row %d of %s: %w", copied+1, table.name, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("reading %s: %w", table.name, err)
	}
	return copied, nil
}

// importedValue is one SQLite value as its Postgres column takes it. A value
// of a type the column cannot hold is an error, never a guess.
func importedValue(column importedColumn, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch column.kind {
	case importedText:
		switch typed := value.(type) {
		case string:
			return typed, nil
		case []byte:
			return string(typed), nil
		}
	case importedNumber:
		switch value.(type) {
		case int64, float64:
			return value, nil
		}
	case importedTime:
		switch typed := value.(type) {
		case time.Time:
			return storedTime(typed), nil
		case string:
			return parsedStandaloneTime(column.name, typed)
		case []byte:
			return parsedStandaloneTime(column.name, string(typed))
		}
	case importedFlag:
		if typed, ok := value.(int64); ok && (typed == 0 || typed == 1) {
			return typed == 1, nil
		}
	}
	return nil, fmt.Errorf("column %s holds %T %v, which the import cannot copy as that column's kind", column.name, value, value)
}

// standaloneTimeLayouts are the layouts mattn/go-sqlite3 writes and reads a
// DATETIME in (its SQLiteTimestampFormats), in its order.
var standaloneTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parsedStandaloneTime reads a DATETIME the standalone store wrote as text.
func parsedStandaloneTime(columnName, text string) (time.Time, error) {
	trimmed := strings.TrimSuffix(text, "Z")
	for _, layout := range standaloneTimeLayouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			return storedTime(parsed), nil
		}
	}
	return time.Time{}, fmt.Errorf("column %s holds %q, which is not a time in any layout the standalone store wrote", columnName, text)
}

// lastClassificationIdentifierHandedOut is the highest identifier number the
// file ever handed out: SQLite's AUTOINCREMENT counter, which remembers a
// number whose row was deleted since, or the highest row when the counter has
// no entry.
func lastClassificationIdentifierHandedOut(ctx context.Context, source *sql.DB) (int64, error) {
	var highest int64
	if err := source.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) FROM classification_identifiers").Scan(&highest); err != nil {
		return 0, fmt.Errorf("reading the highest classification identifier: %w", err)
	}
	var counter sql.NullInt64
	err := source.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = 'classification_identifiers'").Scan(&counter)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reading the classification identifier counter: %w", err)
	}
	if counter.Valid && counter.Int64 > highest {
		highest = counter.Int64
	}
	return highest, nil
}
