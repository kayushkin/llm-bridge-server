// Package mailmodule is where mailstack keeps its records: the Postgres module
// mail (role mail_module), built by the migrations embedded here, and the
// connection type the account and local mail stores run their statements
// through.
//
// The stores' statements are written with ? placeholders, as they were for
// SQLite, and every statement passes through NumberPlaceholders on its way to
// the database, which turns them into Postgres's $1, $2, … in order. Many of
// the queries are assembled from optional pieces (a filter, a cursor), and
// numbering each piece by hand is where a wrong argument would slip in. A ?
// inside a quoted literal is left alone; nothing here uses Postgres's ? jsonb
// operator, and a statement that already carries a $n is refused, so the two
// spellings never mix. kanban-store's db package does the same.
package mailmodule

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role mailstack keeps its rows in, with the
// migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "mail", Role: "mail_module", Migrations: migrations}, nil
}

// NumberPlaceholders turns each ? outside a quoted literal into $1, $2, … in
// order. It panics on a statement that mixes ? with $n or leaves a literal
// open: both are mistakes in the code, not in any input.
func NumberPlaceholders(query string) string {
	if strings.Contains(query, "$1") {
		panic("mailmodule: a statement mixes ? and $n placeholders: " + query)
	}
	var numbered strings.Builder
	numbered.Grow(len(query) + 16)
	number := 0
	inLiteral := false
	for _, character := range query {
		switch {
		case character == '\'':
			inLiteral = !inLiteral
			numbered.WriteRune(character)
		case character == '?' && !inLiteral:
			number++
			numbered.WriteByte('$')
			numbered.WriteString(strconv.Itoa(number))
		default:
			numbered.WriteRune(character)
		}
	}
	if inLiteral {
		panic("mailmodule: a statement has an unclosed quoted literal: " + query)
	}
	return numbered.String()
}

// IsUniqueViolation reports whether err is Postgres refusing a row that
// repeats a unique key.
func IsUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

// DB is the module's pool, numbering every statement's placeholders.
type DB struct {
	pool *sql.DB
}

// New wraps the module's pool, as postgresstorage.Open returned it.
func New(pool *sql.DB) *DB {
	if pool == nil {
		panic("mailmodule: New needs the mail module's pool")
	}
	return &DB{pool: pool}
}

// Pool is the pool itself, for the import, which writes its own numbered
// statements.
func (db *DB) Pool() *sql.DB { return db.pool }

func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return db.pool.Exec(NumberPlaceholders(query), args...)
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.pool.ExecContext(ctx, NumberPlaceholders(query), args...)
}

func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.pool.Query(NumberPlaceholders(query), args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.pool.QueryContext(ctx, NumberPlaceholders(query), args...)
}

func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return db.pool.QueryRow(NumberPlaceholders(query), args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.pool.QueryRowContext(ctx, NumberPlaceholders(query), args...)
}

func (db *DB) Begin() (*Tx, error) {
	return db.BeginTx(context.Background(), nil)
}

func (db *DB) BeginTx(ctx context.Context, options *sql.TxOptions) (*Tx, error) {
	tx, err := db.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Tx is one transaction, numbering every statement's placeholders.
type Tx struct {
	tx *sql.Tx
}

func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(NumberPlaceholders(query), args...)
}

func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, NumberPlaceholders(query), args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, NumberPlaceholders(query), args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, NumberPlaceholders(query), args...)
}

func (tx *Tx) Prepare(query string) (*sql.Stmt, error) {
	return tx.tx.Prepare(NumberPlaceholders(query))
}

func (tx *Tx) Commit() error   { return tx.tx.Commit() }
func (tx *Tx) Rollback() error { return tx.tx.Rollback() }
