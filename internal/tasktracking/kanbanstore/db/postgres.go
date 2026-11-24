package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// The store keeps its rows in Postgres, in the module's own schema and role.
//
// Its statements are written with ? placeholders, as they were for SQLite,
// and every statement passes through numberPlaceholders on its way to the
// database, which turns them into Postgres's $1, $2, … in order. Many of the
// queries are assembled from optional pieces (a filter here, a cursor there),
// and numbering each piece by hand is where a wrong argument would slip in.
// A ? inside a quoted literal is left alone; nothing here uses Postgres's ?
// jsonb operator, and numberPlaceholders refuses a statement that already
// carries a $n, so the two spellings never mix in one statement.
//
// Every row read passes its destinations through utcDestinations, which reads
// a timestamptz into a time.Time in UTC: the driver gives it back in the
// process's own zone, and the store has always answered in UTC.

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its rows in, with the
// migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "kanban", Role: "kanban_module", Migrations: migrations}, nil
}

// now is the time the store stamps on what it writes: UTC, cut to the
// microsecond Postgres keeps, so a row read back equals the row written.
func now() time.Time { return storedTime(time.Now()) }

// storedTime is t as Postgres keeps it: in UTC, to the microsecond.
func storedTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// numberPlaceholders turns each ? outside a quoted literal into $1, $2, … in
// order.
func numberPlaceholders(query string) string {
	if strings.Contains(query, "$1") {
		panic("kanban-store db: a statement mixes ? and $n placeholders: " + query)
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
		panic("kanban-store db: a statement has an unclosed quoted literal: " + query)
	}
	return numbered.String()
}

// utcTime reads a timestamptz into a time.Time in UTC.
type utcTime time.Time

func (t *utcTime) Scan(source any) error {
	value, ok := source.(time.Time)
	if !ok {
		return fmt.Errorf("kanban-store db: a time column held %T, not a time", source)
	}
	*t = utcTime(value.UTC())
	return nil
}

// nullUTCTime reads a nullable timestamptz into a sql.NullTime in UTC.
type nullUTCTime sql.NullTime

func (t *nullUTCTime) Scan(source any) error {
	if source == nil {
		*t = nullUTCTime{}
		return nil
	}
	value, ok := source.(time.Time)
	if !ok {
		return fmt.Errorf("kanban-store db: a time column held %T, not a time", source)
	}
	*t = nullUTCTime{Time: value.UTC(), Valid: true}
	return nil
}

// utcDestinations swaps each time destination for its UTC reader.
func utcDestinations(destinations []any) []any {
	swapped := make([]any, len(destinations))
	for index, destination := range destinations {
		switch typed := destination.(type) {
		case *time.Time:
			swapped[index] = (*utcTime)(typed)
		case *sql.NullTime:
			swapped[index] = (*nullUTCTime)(typed)
		default:
			swapped[index] = destination
		}
	}
	return swapped
}

// resultRow is a *sql.Row whose times come back in UTC.
type resultRow struct{ row *sql.Row }

func (r resultRow) Scan(destinations ...any) error {
	return r.row.Scan(utcDestinations(destinations)...)
}

// resultRows is *sql.Rows whose times come back in UTC.
type resultRows struct{ *sql.Rows }

func (r *resultRows) Scan(destinations ...any) error {
	return r.Rows.Scan(utcDestinations(destinations)...)
}

// executor is what the pool and a transaction both offer the store's reads and
// writes.
type executor interface {
	Exec(query string, arguments ...any) (sql.Result, error)
	Query(query string, arguments ...any) (*resultRows, error)
	QueryRow(query string, arguments ...any) resultRow
}

// sqlRunner is *sql.DB or *sql.Tx.
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, arguments ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, arguments ...any) *sql.Row
}

// postgresExecutor numbers each statement's placeholders and reads its times
// in UTC.
type postgresExecutor struct{ runner sqlRunner }

func (e postgresExecutor) Exec(query string, arguments ...any) (sql.Result, error) {
	return e.runner.ExecContext(context.Background(), numberPlaceholders(query), arguments...)
}

func (e postgresExecutor) Query(query string, arguments ...any) (*resultRows, error) {
	rows, err := e.runner.QueryContext(context.Background(), numberPlaceholders(query), arguments...)
	if err != nil {
		return nil, err
	}
	return &resultRows{rows}, nil
}

func (e postgresExecutor) QueryRow(query string, arguments ...any) resultRow {
	return resultRow{e.runner.QueryRowContext(context.Background(), numberPlaceholders(query), arguments...)}
}

// pool is the module's connection pool as the store uses it.
type pool struct {
	postgresExecutor
	database *sql.DB
}

// inTransaction runs work in one transaction: committed when work returns nil,
// rolled back when it returns an error. work's error comes back unchanged.
func (p pool) inTransaction(work func(executor) error) error {
	return postgresstorage.InTransaction(context.Background(), p.database, func(tx *sql.Tx) error {
		return work(postgresExecutor{tx})
	})
}

// writeLockKey names the advisory lock writeImmediately takes.
const writeLockKey = "kanban-store.write-immediately"

// writeImmediately runs work in a transaction that first takes one advisory
// lock, which every caller of writeImmediately takes, so they run one after
// the other. That is what SQLite's BEGIN IMMEDIATE gave the standalone store:
// a writer that checks the state and then writes on it holds the lock across
// both, so no other such writer can change the state in between, and each
// statement it runs sees everything the writers before it committed (Postgres
// reads committed rows at each statement). Writes that do not take the lock
// (a card placement, a link) are not part of any check made here, as they were
// not under SQLite either, where they simply waited their turn.
//
// The lock is one for the whole store rather than one per board because two
// of the checks span boards: classification ids are numbered across every
// board, and so are decision sequences.
func (p pool) writeImmediately(work func(executor) error) error {
	return p.inTransaction(func(run executor) error {
		if _, err := run.Exec(`SELECT pg_advisory_xact_lock(hashtext(?))`, writeLockKey); err != nil {
			return fmt.Errorf("taking the store's write lock: %w", err)
		}
		return work(run)
	})
}

// isUniqueViolation reports whether err is Postgres refusing a write for the
// unique index named constraint.
func isUniqueViolation(err error, constraint string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == constraint
}
