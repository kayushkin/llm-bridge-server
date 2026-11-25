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
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role rule-store keeps its records in,
// with the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "rules", Role: "rules_module", Migrations: migrations}, nil
}

// Store owns the module's records.
type Store struct {
	db *sql.DB
}

// New keeps rule-store's records in pool, the module's own connection pool:
// provisioned, migrated and opened by the caller.
func New(pool *sql.DB) *Store {
	return &Store{db: pool}
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

// nextSeq takes the next number of a table's seq identity column, inside tx.
// A number taken by a transaction that rolls back is never used again.
func nextSeq(tx *sql.Tx, table string) (int64, error) {
	var seq int64
	if err := tx.QueryRow(`SELECT nextval(pg_get_serial_sequence($1, 'seq'))`, table).Scan(&seq); err != nil {
		return 0, fmt.Errorf("numbering %s: %w", table, err)
	}
	return seq, nil
}

func formatID(prefix string, seq int64) string { return postgresstorage.FormatRecordID(prefix, seq) }

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
