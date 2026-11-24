package toolstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// toolsTableCreateStatement returns the CREATE TABLE statement for tools from
// the embedded schema.sql, renamed to tableName. schema.sql stays the one
// definition of the table; the rebuild below creates its new table from it.
func toolsTableCreateStatement(tableName string) (string, error) {
	const opening = "CREATE TABLE IF NOT EXISTS tools ("
	if strings.Count(schemaSQL, opening) != 1 {
		return "", fmt.Errorf("schema.sql must hold exactly one %q", opening)
	}
	start := strings.Index(schemaSQL, opening)
	end := strings.Index(schemaSQL[start:], "\n);")
	if end < 0 {
		return "", errors.New(`schema.sql: the tools table has no closing "\n);"`)
	}
	body := schemaSQL[start+len(opening) : start+end]
	return "CREATE TABLE " + tableName + " (" + body + "\n)", nil
}

// queryer is what *sql.Conn and *sql.Tx share for reading.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tableColumns lists a table's column names in order; none if the table does
// not exist.
func tableColumns(ctx context.Context, db queryer, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// migrateToolsTableToAcceptHarnessTools rebuilds a tools table made before the
// harness kind: its CHECK on kind has no 'harness' and it has no harness or
// harness_tool_name column. SQLite cannot alter a CHECK, so the table is
// rebuilt the way SQLite documents (https://sqlite.org/lang_altertable.html,
// "other kinds of table schema changes"): with foreign keys off, create the
// new table, copy every row with its id, drop the old table, rename the new
// one, check foreign keys, commit. instance_tools rows point at tools by id
// and survive because the ids do; the AUTOINCREMENT high-water mark is carried
// over so a deleted id is never handed out again.
//
// Every column of the old table must exist in the new one, or the rebuild
// refuses rather than drop data. A database older than the credentials column
// gets it with its default. Runs before schema.sql, whose index on
// tools(harness) cannot be made on the old table. Does nothing to a database
// with no tools table (schema.sql makes it) or one that already has
// harness_tool_name.
func migrateToolsTableToAcceptHarnessTools(db *sql.DB) (err error) {
	ctx := context.Background()
	// PRAGMA foreign_keys is per connection and ignored inside a transaction,
	// so the whole rebuild runs on one connection taken from the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	oldColumns, err := tableColumns(ctx, conn, "tools")
	if err != nil {
		return fmt.Errorf("read tools columns: %w", err)
	}
	if len(oldColumns) == 0 {
		return nil
	}
	for _, column := range oldColumns {
		if column == "harness_tool_name" {
			return nil
		}
	}

	createRebuilt, err := toolsTableCreateStatement("tools_rebuilt")
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("turn foreign keys off: %w", err)
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); restoreErr != nil && err == nil {
			err = fmt.Errorf("turn foreign keys back on: %w", restoreErr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, createRebuilt); err != nil {
		return fmt.Errorf("create tools_rebuilt: %w", err)
	}
	newColumns, err := tableColumns(ctx, tx, "tools_rebuilt")
	if err != nil {
		return fmt.Errorf("read tools_rebuilt columns: %w", err)
	}
	newColumnSet := make(map[string]bool, len(newColumns))
	for _, column := range newColumns {
		newColumnSet[column] = true
	}
	for _, column := range oldColumns {
		if !newColumnSet[column] {
			return fmt.Errorf("tools column %q has no place in the rebuilt table; refusing to drop it", column)
		}
	}

	var oldRowCount int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tools`).Scan(&oldRowCount); err != nil {
		return err
	}
	var highestIssuedID sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'tools'`).Scan(&highestIssuedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read tools autoincrement sequence: %w", err)
	}

	columnList := strings.Join(oldColumns, ", ")
	if _, err = tx.ExecContext(ctx, `INSERT INTO tools_rebuilt (`+columnList+`) SELECT `+columnList+` FROM tools`); err != nil {
		return fmt.Errorf("copy tools rows: %w", err)
	}
	var copiedRowCount int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tools_rebuilt`).Scan(&copiedRowCount); err != nil {
		return err
	}
	if copiedRowCount != oldRowCount {
		return fmt.Errorf("copied %d tools rows of %d", copiedRowCount, oldRowCount)
	}

	if _, err = tx.ExecContext(ctx, `DROP TABLE tools`); err != nil {
		return fmt.Errorf("drop old tools: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE tools_rebuilt RENAME TO tools`); err != nil {
		return fmt.Errorf("rename tools_rebuilt: %w", err)
	}
	if highestIssuedID.Valid {
		// An empty old table leaves the new one no sequence row to update.
		var result sql.Result
		if result, err = tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = max(seq, ?) WHERE name = 'tools'`, highestIssuedID.Int64); err != nil {
			return fmt.Errorf("carry the tools autoincrement sequence over: %w", err)
		}
		var updated int64
		if updated, err = result.RowsAffected(); err != nil {
			return err
		}
		if updated == 0 {
			if _, err = tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('tools', ?)`, highestIssuedID.Int64); err != nil {
				return fmt.Errorf("carry the tools autoincrement sequence over: %w", err)
			}
		}
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyIndex int
		if err = rows.Scan(&table, &rowID, &parent, &foreignKeyIndex); err != nil {
			rows.Close()
			return err
		}
		violations = append(violations, fmt.Sprintf("%s row %d -> %s", table, rowID.Int64, parent))
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(violations) > 0 {
		err = fmt.Errorf("foreign key violations after rebuilding tools: %s", strings.Join(violations, "; "))
		return err
	}

	return tx.Commit()
}

// addLastSeenAtColumnToTools adds tools.last_seen_at to a tools table made
// before it, with the definition schema.sql gives it (a test holds the two
// equal). Runs after the harness rebuild
// (which makes the table from schema.sql and so already has the column) and
// before schema.sql. Does nothing when there is no tools table yet or the
// column is already there.
func addLastSeenAtColumnToTools(db *sql.DB) error {
	ctx := context.Background()
	columns, err := tableColumns(ctx, db, "tools")
	if err != nil {
		return fmt.Errorf("read tools columns: %w", err)
	}
	if len(columns) == 0 {
		return nil
	}
	for _, column := range columns {
		if column == "last_seen_at" {
			return nil
		}
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE tools ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add tools.last_seen_at: %w", err)
	}
	return nil
}
