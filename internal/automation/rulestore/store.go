// Package rulestore holds rules that react to changes in other services and
// runs them: "when a session from llm-bridge-server changes, if these
// conditions hold, do these effects in the service that owns them".
//
// It owns the rules, their revisions, who activated them, and every decision
// a rule made. It owns no subject: sessions stay in llm-bridge-server and
// cards in kanban-store, and an effect is a call to the owner. It keeps no
// clock either: the scheduler keeps cron, and the sweep here only re-checks
// conditions that depend on how much time has passed.
//
// Today every rule runs in shadow mode: a match records the exact call its
// effects would make, and nothing is called. Live mode comes after shadow runs
// have been compared with the binaries the rules replace.
package rulestore

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// Store owns the database.
type Store struct {
	db      *sql.DB
	dataDir string
}

// Open creates the data directory if needed, opens the database and applies
// the schema, which is create-only, so this is safe on every boot.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open: no data directory")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "rule-store.db")+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One writer at a time; SQLite serializes writes anyway, and a single
	// connection makes "read the max seq, insert seq+1" safe without retries.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

// nextSeq returns the next sequence number of a table, inside tx.
func nextSeq(tx *sql.Tx, table string) (int64, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM ` + table).Scan(&maxSeq); err != nil {
		return 0, err
	}
	return maxSeq.Int64 + 1, nil
}

func formatID(prefix string, seq int64) string { return fmt.Sprintf("%s_%06d", prefix, seq) }

// Counts is what GET /health reports.
type Counts struct {
	Rules       int `json:"rules"`
	ActiveRules int `json:"active_rules"`
	Subjects    int `json:"subjects"`
	Runs        int `json:"runs"`
}

// Counts reads the row counts.
func (s *Store) Counts() (Counts, error) {
	var c Counts
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM rules),
		(SELECT COUNT(*) FROM rules WHERE state = 'active'),
		(SELECT COUNT(*) FROM subjects),
		(SELECT COUNT(*) FROM rule_runs)`).Scan(&c.Rules, &c.ActiveRules, &c.Subjects, &c.Runs)
	return c, err
}
