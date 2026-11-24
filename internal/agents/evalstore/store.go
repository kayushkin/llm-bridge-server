// Package evalstore owns judged runs of several models on a fixed set of real
// model calls: suites (a task and how to judge it), cases (frozen calls),
// runs (which models answer and which judge), answers, verdicts and checks.
//
// It replaces a Python bench that decided production model roles for
// scheduler's email filing and demo mail. Every model call goes through
// llm-bridge-server's operations API, so it is recorded, priced and held to
// an organization's budget there.
package evalstore

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
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
	suiteIDPrefix   = "eval_suite_"
	caseIDPrefix    = "eval_case_"
	runIDPrefix     = "eval_run_"
	answerIDPrefix  = "eval_answer_"
	verdictIDPrefix = "eval_verdict_"
)

func formatID(prefix string, seq int64) string { return fmt.Sprintf("%s%06d", prefix, seq) }

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
	// now is the clock; tests replace it.
	now func() time.Time
}

const databaseFileName = "eval-store.db"

// Open creates the data directory (mode 700) and the database file (mode
// 600) if needed, opens it and applies the schema. Cases hold real prompts,
// so a directory or file anyone else can read is refused rather than used.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open: no data directory given")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := requirePrivateMode(dataDir, 0o700); err != nil {
		return nil, err
	}
	databasePath := filepath.Join(dataDir, databaseFileName)
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	file.Close()
	if err := requirePrivateMode(databasePath, 0o600); err != nil {
		return nil, err
	}
	// Transactions begin IMMEDIATE: a run records answers from several
	// goroutines, and a transaction that reads before it writes would otherwise
	// fail with "database is locked" when two upgrade at once.
	db, err := sql.Open("sqlite3", databasePath+"?_foreign_keys=on&_busy_timeout=10000&_journal_mode=WAL&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir, now: time.Now}, nil
}

// columnsAddedAfterCreation are columns schema.sql declares that a database
// created before them lacks. CREATE TABLE IF NOT EXISTS leaves an existing
// table alone, so each is added here when missing.
var columnsAddedAfterCreation = []struct{ table, column, definition string }{
	{"cases", "expected_answer", "TEXT"},
}

func addMissingColumns(db *sql.DB) error {
	for _, added := range columnsAddedAfterCreation {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", added.table, added.column).Scan(&count); err != nil {
			return fmt.Errorf("read columns of %s: %w", added.table, err)
		}
		if count > 0 {
			continue
		}
		if _, err := db.Exec("ALTER TABLE " + added.table + " ADD COLUMN " + added.column + " " + added.definition); err != nil {
			return fmt.Errorf("add column %s.%s: %w", added.table, added.column, err)
		}
	}
	return nil
}

// requirePrivateMode refuses a path that group or others can reach.
func requirePrivateMode(path string, want os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %v; it holds real prompts and must be %v (chmod %o %s)", path, info.Mode().Perm(), want, want, path)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory the database lives in.
func (s *Store) DataDir() string { return s.dataDir }

func (s *Store) unixNow() int64 { return s.now().Unix() }

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	Exec(query string, arguments ...any) (sql.Result, error)
	Query(query string, arguments ...any) (*sql.Rows, error)
	QueryRow(query string, arguments ...any) *sql.Row
}

// insertWithID inserts a row whose id is derived from its sequence: insert
// with an empty id, then set the real one. It takes a transaction because the
// empty id is unique too, so two of these may not interleave.
func insertWithID(q *sql.Tx, table, prefix, columns, placeholders string, arguments ...any) (string, error) {
	result, err := q.Exec(fmt.Sprintf("INSERT INTO %s (id, %s) VALUES ('', %s)", table, columns, placeholders), arguments...)
	if err != nil {
		return "", err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return "", err
	}
	id := formatID(prefix, seq)
	if _, err := q.Exec(fmt.Sprintf("UPDATE %s SET id = ? WHERE seq = ?", table), id, seq); err != nil {
		return "", err
	}
	return id, nil
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
