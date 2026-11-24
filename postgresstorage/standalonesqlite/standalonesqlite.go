// Package standalonesqlite reads the SQLite file of a standalone store that
// moved into a Postgres module, for that module's import command. It only
// reads: the file is a copy (sqlite3 .backup) of a live store's database, and
// nothing here writes to it.
//
// A store's import keeps its own rules (which ids, which numbering, which
// values it refuses). What every import shares is here: opening the file read
// only, refusing a table whose columns it does not know, refusing a module
// that already holds rows, and copying rows column by column.
package standalonesqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite file at path read only. A missing file is an error,
// not an empty database.
func Open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := source.Ping(); err != nil {
		source.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return source, nil
}

// CheckColumns refuses a table whose columns are not exactly want: a column
// the import does not know is data it would drop, and a missing one means the
// file is not the store's.
func CheckColumns(ctx context.Context, source *sql.DB, store, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a %s database", table, store)
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

// RefuseRowsInModule refuses a module that already holds a row in any of
// tables, so an import can never mix two stores' records or run twice.
func RefuseRowsInModule(ctx context.Context, tx *sql.Tx, module string, tables ...string) error {
	for _, table := range tables {
		var held int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
			return fmt.Errorf("counting %s: %w", table, err)
		}
		if held > 0 {
			return fmt.Errorf("the %s module already holds %d rows in %s; the import copies into an empty module only", module, held, table)
		}
	}
	return nil
}

// Table is one table to copy: the columns read from the SQLite file, in
// order, and the Postgres insert their values go into.
type Table struct {
	// Name is the table in the SQLite file.
	Name string
	// Columns are read in this order.
	Columns []string
	// OrderBy orders the read, so a failure names the same row each time.
	OrderBy string
	// Insert is the Postgres statement, with $1… in the order of Columns
	// after Convert.
	Insert string
	// Convert turns one row as SQLite holds it into the values Insert takes,
	// refusing a value the module would not store. Nil passes them as read.
	Convert func(row []any) ([]any, error)
}

// Copy reads every row of table from source and inserts it into tx. It
// answers how many rows it copied. Any error stops it, naming the row.
func Copy(ctx context.Context, source *sql.DB, tx *sql.Tx, table Table) (int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT "+strings.Join(table.Columns, ", ")+" FROM "+table.Name+" ORDER BY "+table.OrderBy)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", table.Name, err)
	}
	defer rows.Close()
	statement, err := tx.PrepareContext(ctx, table.Insert)
	if err != nil {
		return 0, fmt.Errorf("preparing the copy of %s: %w", table.Name, err)
	}
	defer statement.Close()
	var copied int64
	for rows.Next() {
		values := make([]any, len(table.Columns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return copied, fmt.Errorf("reading row %d of %s: %w", copied+1, table.Name, err)
		}
		arguments := values
		if table.Convert != nil {
			if arguments, err = table.Convert(values); err != nil {
				return copied, fmt.Errorf("%s row %s: %w", table.Name, describeRow(table.Columns, values), err)
			}
		}
		if _, err := statement.ExecContext(ctx, arguments...); err != nil {
			return copied, fmt.Errorf("copying %s row %s: %w", table.Name, describeRow(table.Columns, values), err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("reading %s: %w", table.Name, err)
	}
	return copied, nil
}

// describeRow names a row by its first column, which is its key in every
// table an import copies.
func describeRow(columns []string, values []any) string {
	return fmt.Sprintf("%s=%v", columns[0], values[0])
}

// Integer reads an INTEGER column's value. A NULL or a value of another type
// is an error naming the column.
func Integer(column string, value any) (int64, error) {
	number, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("%s is %T %v, not an integer", column, value, value)
	}
	return number, nil
}

// Boolean reads an INTEGER column that holds 0 or 1.
func Boolean(column string, value any) (bool, error) {
	number, err := Integer(column, value)
	if err != nil {
		return false, err
	}
	switch number {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, fmt.Errorf("%s is %d, not 0 or 1", column, number)
}

// Text reads a TEXT column's value. A NULL or a value of another type is an
// error naming the column.
func Text(column string, value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is %T %v, not text", column, value, value)
	}
	return text, nil
}
