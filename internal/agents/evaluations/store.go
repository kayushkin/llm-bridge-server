// Package evaluations owns judged runs of several models on a fixed set of real
// model calls: suites (a task and how to judge it), cases (frozen calls),
// runs (which models answer and which judge), answers, verdicts and checks.
//
// It replaces a Python bench that decided production model roles for
// scheduler's email filing and demo mail. Every model call goes through
// llm-bridge-server's operations API, so it is recorded, priced and held to
// an organization's budget there.
package evaluations

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them. Cases hold real mail: only this module's
// role, and the login that provisions it, can read the schema.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "evaluations", Role: "evaluations_module", Migrations: migrations}, nil
}

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
	// ErrRunsOff: this process makes no model calls for runs, so a run can be
	// created and read but not started or resumed.
	ErrRunsOff = errors.New("runs are off")
)

func invalidf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}

func conflictf(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, arguments...))
}

// Id prefixes. The prefix keeps an id from looking like a uuid, which
// noteboard already claims in dash's resolver, and says what the id names.
const (
	suiteIDPrefix   = "eval_suite"
	caseIDPrefix    = "eval_case"
	runIDPrefix     = "eval_run"
	answerIDPrefix  = "eval_answer"
	verdictIDPrefix = "eval_verdict"
)

// Store keeps the records in the module's schema. The server provisions,
// migrates and opens the module, then hands the pool to New.
type Store struct {
	db *sql.DB
	// now is the clock; tests replace it.
	now func() time.Time
}

// New returns the store over db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

func (s *Store) unixNow() int64 { return s.now().Unix() }

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	Exec(query string, arguments ...any) (sql.Result, error)
	Query(query string, arguments ...any) (*sql.Rows, error)
	QueryRow(query string, arguments ...any) *sql.Row
}

// insertWithID inserts a row whose id is derived from its sequence number:
// it takes the number from the table's identity first, so the row is written
// once, whole.
func insertWithID(tx *sql.Tx, table, prefix, columns string, arguments ...any) (string, error) {
	var seq int64
	if err := tx.QueryRow("SELECT nextval(pg_get_serial_sequence($1, 'seq'))", table).Scan(&seq); err != nil {
		return "", fmt.Errorf("numbering a row of %s: %w", table, err)
	}
	id := postgresstorage.FormatRecordID(prefix, seq)
	placeholders := make([]string, len(arguments))
	for index := range arguments {
		placeholders[index] = fmt.Sprintf("$%d", index+3)
	}
	statement := fmt.Sprintf("INSERT INTO %s (seq, id, %s) OVERRIDING SYSTEM VALUE VALUES ($1, $2, %s)", table, columns, strings.Join(placeholders, ", "))
	if _, err := tx.Exec(statement, append([]any{seq, id}, arguments...)...); err != nil {
		return "", err
	}
	return id, nil
}

// uniqueViolation is Postgres's error code for a row a unique constraint
// refuses.
const uniqueViolation = "23505"

// violatesConstraint says whether err is the unique constraint named refusing
// a row.
func violatesConstraint(err error, constraint string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == uniqueViolation && postgresError.ConstraintName == constraint
}

func (s *Store) inTransaction(work func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := work(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode %T: %v", value, err))
	}
	return string(encoded)
}

func decodeJSONColumn(raw string, target any, what string) error {
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("stored %s is not valid JSON: %w", what, err)
	}
	return nil
}

func nullableRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func rawFromNullable(value sql.NullString) json.RawMessage {
	if !value.Valid {
		return nil
	}
	return json.RawMessage(value.String)
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func intFromNullable(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	number := value.Int64
	return &number
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
