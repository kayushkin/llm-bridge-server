package possessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// database is one of the store's Postgres pools, the one a Store's scope
// reads and writes through (store.go). It changes two things on the way in,
// and nothing on the way out:
//
//   - The store's statements are written with ? placeholders, as they were
//     for SQLite, and each is handed to Postgres with them numbered $1, $2…
//     in order (numberPlaceholders). A statement that already carries $1 is
//     passed as it is.
//   - SQLite kept a Go bool as 1 or 0, and the columns that held them are
//     bigint here, so a bool argument is passed as 1 or 0.
//
// The method set is the one the store uses; a statement can reach Postgres
// only through one of them.
type database struct {
	pool *sql.DB
}

// databaseTransaction is one transaction on a database, with the same two changes.
type databaseTransaction struct {
	tx *sql.Tx
}

func (d *database) checkOpen() {
	if d == nil || d.pool == nil {
		// A Store reaches records only after Narrowed chose whose; a query
		// before that is a bug in this package, never a request to answer.
		panic("possession-store: a query on a store no scope was chosen for; call Narrowed first")
	}
}

func (d *database) Query(query string, arguments ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), query, arguments...)
}

func (d *database) QueryContext(ctx context.Context, query string, arguments ...any) (*sql.Rows, error) {
	d.checkOpen()
	return d.pool.QueryContext(ctx, numberPlaceholders(query), postgresArguments(arguments)...)
}

func (d *database) QueryRow(query string, arguments ...any) *sql.Row {
	return d.QueryRowContext(context.Background(), query, arguments...)
}

func (d *database) QueryRowContext(ctx context.Context, query string, arguments ...any) *sql.Row {
	d.checkOpen()
	return d.pool.QueryRowContext(ctx, numberPlaceholders(query), postgresArguments(arguments)...)
}

func (d *database) Exec(query string, arguments ...any) (sql.Result, error) {
	return d.ExecContext(context.Background(), query, arguments...)
}

func (d *database) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	d.checkOpen()
	return d.pool.ExecContext(ctx, numberPlaceholders(query), postgresArguments(arguments)...)
}

func (d *database) Begin() (*databaseTransaction, error) {
	return d.BeginTx(context.Background(), nil)
}

func (d *database) BeginTx(ctx context.Context, options *sql.TxOptions) (*databaseTransaction, error) {
	d.checkOpen()
	tx, err := d.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &databaseTransaction{tx: tx}, nil
}

func (t *databaseTransaction) Query(query string, arguments ...any) (*sql.Rows, error) {
	return t.tx.Query(numberPlaceholders(query), postgresArguments(arguments)...)
}

func (t *databaseTransaction) QueryRow(query string, arguments ...any) *sql.Row {
	return t.tx.QueryRow(numberPlaceholders(query), postgresArguments(arguments)...)
}

func (t *databaseTransaction) Exec(query string, arguments ...any) (sql.Result, error) {
	return t.tx.Exec(numberPlaceholders(query), postgresArguments(arguments)...)
}

func (t *databaseTransaction) Commit() error { return t.tx.Commit() }

// Rollback undoes the transaction. After a commit it does nothing and
// answers sql.ErrTxDone, which every deferred Rollback ignores.
func (t *databaseTransaction) Rollback() error { return t.tx.Rollback() }

// numberPlaceholders writes each ? outside a quoted string, a quoted name or a
// comment as $1, $2… in order, and SQLite's ?NNN as $NNN. A statement that already numbers its
// placeholders ($1 outside quotes) is answered unchanged; one that mixes both
// reaches Postgres with its ? and fails there as a syntax error.
func numberPlaceholders(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var numbered strings.Builder
	numbered.Grow(len(query) + 16)
	count := 0
	for index := 0; index < len(query); index++ {
		character := query[index]
		switch {
		case character == '\'' || character == '"':
			end := closingQuote(query, index, character)
			numbered.WriteString(query[index:end])
			index = end - 1
		case character == '-' && index+1 < len(query) && query[index+1] == '-':
			end := strings.IndexByte(query[index:], '\n')
			if end < 0 {
				end = len(query) - index
			}
			numbered.WriteString(query[index : index+end])
			index += end - 1
		case character == '$' && index+1 < len(query) && query[index+1] >= '0' && query[index+1] <= '9':
			return query
		case character == '?' && index+1 < len(query) && query[index+1] >= '0' && query[index+1] <= '9':
			// SQLite's ?NNN names parameter NNN, as $NNN does.
			numbered.WriteByte('$')
		case character == '?':
			count++
			numbered.WriteByte('$')
			numbered.WriteString(strconv.Itoa(count))
		default:
			numbered.WriteByte(character)
		}
	}
	return numbered.String()
}

// closingQuote is the index just past the quote that closes the one at
// start; a doubled quote inside is part of the string. An unclosed quote
// runs to the end.
func closingQuote(query string, start int, quote byte) int {
	for index := start + 1; index < len(query); index++ {
		if query[index] != quote {
			continue
		}
		if index+1 < len(query) && query[index+1] == quote {
			index++
			continue
		}
		return index + 1
	}
	return len(query)
}

// postgresArguments passes every argument unchanged but a bool, which is 1 or
// 0 as SQLite kept it.
func postgresArguments(arguments []any) []any {
	converted := arguments
	for index, argument := range arguments {
		value, isBool := argument.(bool)
		if !isBool {
			continue
		}
		if &converted[0] == &arguments[0] {
			converted = append([]any(nil), arguments...)
		}
		converted[index] = boolToInteger(value)
	}
	return converted
}

// isUniqueViolation reports whether err is Postgres refusing a row that a
// unique constraint already holds.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// jsonField is one "name":value pair as JSON writes it, to find a value in
// a column that holds JSON text.
func jsonField(name, value string) string {
	encoded, _ := json.Marshal(value)
	return fmt.Sprintf(`"%s":%s`, name, encoded)
}
