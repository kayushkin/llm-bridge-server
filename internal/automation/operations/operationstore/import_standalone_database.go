package operationstore

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneColumns are the columns of each table of the SQLite file the
// server kept operations in (operations.db). The import refuses a file whose
// tables have any other columns: a column it does not know is data it would
// drop, and a missing one means the file is not an operations database.
var standaloneColumns = map[string][]string{
	"operations": {"id", "type", "organization_id", "principal_id", "idempotency_key", "intent_fingerprint",
		"parent_operation_id", "root_operation_id", "state", "revision", "intent_json", "receipt_json",
		"lease_owner", "lease_token", "lease_expires_at", "created_at", "updated_at"},
	"operation_events": {"operation_id", "sequence", "kind", "event_json", "at"},
	"operation_spend": {"operation_id", "root_operation_id", "organization_id", "model", "input_tokens",
		"output_tokens", "cache_read_tokens", "cache_write_tokens", "cost_usd", "cost_known", "at"},
	"organization_budgets": {"organization_id", "monthly_limit_usd", "updated_by", "updated_at"},
}

// standaloneIntegerColumns, standaloneRealColumns and standaloneBooleanColumns
// say how each column that is not text is read; every other column is text.
var (
	standaloneIntegerColumns = map[string]bool{"revision": true, "lease_token": true, "lease_expires_at": true,
		"created_at": true, "updated_at": true, "sequence": true, "at": true, "input_tokens": true,
		"output_tokens": true, "cache_read_tokens": true, "cache_write_tokens": true}
	standaloneRealColumns    = map[string]bool{"cost_usd": true, "monthly_limit_usd": true}
	standaloneBooleanColumns = map[string]bool{"cost_known": true}
)

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Operations          int64            `json:"operations"`
	OperationsByState   map[string]int64 `json:"operations_by_state"`
	Events              int64            `json:"events"`
	SpendRows           int64            `json:"spend_rows"`
	OrganizationBudgets int64            `json:"organization_budgets"`
}

// ImportStandaloneDatabase copies every operation, event, spend row and
// organization budget from the server's SQLite operations file into the
// module behind db, as they are: operation ids, revisions, lease tokens,
// times to the nanosecond, and the intent, receipt and event JSON byte for
// byte. It runs in one transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, a file it cannot copy
// whole (a table or column it does not know, a value of the wrong type, JSON
// that is not UTF-8 text), and a file in which an operation's revision is not
// the sequence of its last event, since a stream resumed from such a record
// would skip or repeat a change.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	counts := ImportCounts{OperationsByState: map[string]int64{}}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	if err := refuseUnknownStandaloneTables(ctx, source, sqlitePath); err != nil {
		return counts, err
	}
	for table, want := range standaloneColumns {
		if err := standalonesqlite.CheckColumns(ctx, source, "operations", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	var unmatchedRevisions int64
	if err := source.QueryRowContext(ctx, `SELECT count(*) FROM operations
		WHERE revision != COALESCE((SELECT max(sequence) FROM operation_events WHERE operation_id = operations.id), 0)`).Scan(&unmatchedRevisions); err != nil {
		return counts, fmt.Errorf("%s: comparing revisions with events: %w", sqlitePath, err)
	}
	if unmatchedRevisions > 0 {
		return counts, fmt.Errorf("%s: %d operations have a revision that is not the sequence of their last event", sqlitePath, unmatchedRevisions)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "operations", "operations", "operation_events", "operation_spend", "organization_budgets"); err != nil {
			return err
		}
		copyTable := func(table, orderBy string) (int64, error) {
			columns := standaloneColumns[table]
			return standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
				Name: table, Columns: columns, OrderBy: orderBy,
				Insert:  insertStatement(table, columns),
				Convert: func(row []any) ([]any, error) { return convertStandaloneRow(columns, row) },
			})
		}
		var err error
		// Parents were created before their children, so created_at order
		// never inserts a child ahead of the parent its row names.
		if counts.Operations, err = copyTable("operations", "created_at, id"); err != nil {
			return err
		}
		if counts.Events, err = copyTable("operation_events", "operation_id, sequence"); err != nil {
			return err
		}
		// rowid is the order the calls were recorded in.
		if counts.SpendRows, err = copyTable("operation_spend", "rowid"); err != nil {
			return err
		}
		if counts.OrganizationBudgets, err = copyTable("organization_budgets", "organization_id"); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT state, count(*) FROM operations GROUP BY state`)
		if err != nil {
			return fmt.Errorf("counting the imported operations by state: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var count int64
			if err := rows.Scan(&state, &count); err != nil {
				return err
			}
			counts.OperationsByState[state] = count
		}
		return rows.Err()
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func insertStatement(table string, columns []string) string {
	statement := "INSERT INTO " + table + " ("
	values := ""
	for index, column := range columns {
		if index > 0 {
			statement += ", "
			values += ", "
		}
		statement += column
		values += fmt.Sprintf("$%d", index+1)
	}
	return statement + ") VALUES (" + values + ")"
}

// refuseUnknownStandaloneTables refuses a file holding a table the import
// does not copy: its rows would be lost without a word.
func refuseUnknownStandaloneTables(ctx context.Context, source *sql.DB, sqlitePath string) error {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
		}
		if _, copied := standaloneColumns[name]; !copied {
			return fmt.Errorf("%s has a table %s, which the import does not know; it copies only a file it can copy whole", sqlitePath, name)
		}
	}
	return rows.Err()
}

func convertStandaloneRow(columns []string, row []any) ([]any, error) {
	values := make([]any, len(row))
	for index, column := range columns {
		var err error
		switch {
		case standaloneIntegerColumns[column]:
			values[index], err = standalonesqlite.Integer(column, row[index])
		case standaloneRealColumns[column]:
			values[index], err = realNumber(column, row[index])
		case standaloneBooleanColumns[column]:
			values[index], err = standalonesqlite.Boolean(column, row[index])
		default:
			values[index], err = textOrTextBytes(column, row[index])
		}
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

// textOrTextBytes reads a text column. The SQLite store wrote its JSON
// columns as Go byte slices, which SQLite keeps as blobs; those are taken as
// text when they are UTF-8.
func textOrTextBytes(column string, value any) (string, error) {
	switch text := value.(type) {
	case string:
		return text, nil
	case []byte:
		if !utf8.Valid(text) {
			return "", fmt.Errorf("%s holds bytes that are not UTF-8 text", column)
		}
		return string(text), nil
	}
	return "", fmt.Errorf("%s is %T %v, not text", column, value, value)
}

// realNumber reads a REAL column. SQLite keeps a whole number written to it
// as an integer when it reads back exactly, so both are taken.
func realNumber(column string, value any) (float64, error) {
	switch number := value.(type) {
	case float64:
		return number, nil
	case int64:
		return float64(number), nil
	}
	return 0, fmt.Errorf("%s is %T %v, not a number", column, value, value)
}
