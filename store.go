// Package hookstore is the registry for bridge-managed harness hooks.
//
// Each row is a hook handler (harness, event, matcher, shell command) bound
// to a scope (global, instance, session). The llm-bridge server queries this
// store at session spawn to decide which hooks to wire into the harness's
// native hook mechanism (for Claude Code: synthesized into a settings JSON
// passed via --settings).
//
// The store itself is a pure data layer — it does not execute hooks, render
// settings JSON, or talk to harnesses. That belongs to the server.
package hookstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps the hook-store SQLite database.
type Store struct {
	db *sql.DB
}

// busyTimeoutMillisecondsWanted is how long a connection waits for a lock
// before giving up. Named so the DSN that sets it and the check that proves it
// took effect cannot drift apart.
const busyTimeoutMillisecondsWanted = 5000

// dataSourceName builds the sqlite DSN for a database path.
//
// busy_timeout and foreign_keys MUST be set here rather than with a PRAGMA
// statement after Open: they are per-connection settings, and *sql.DB is a
// pool. A one-shot db.Exec("PRAGMA foreign_keys=ON") reaches only whichever
// connection the pool happened to hand out, so every other connection keeps
// SQLite's default — off for foreign_keys, 0 for busy_timeout. Measured on
// this driver holding 8 connections open at once: 7 of the 8 never saw it.
// Putting them in the DSN applies them to every connection the pool opens.
// (journal_mode needs no such care: it is a property of the database file,
// so one Exec is permanent.)
//
// _pragma=... is modernc's syntax and it is the only one that works here. The
// mattn/go-sqlite3 spelling (_foreign_keys=on) is silently ignored by this
// driver, as is any other unrecognized key — hence the verification below.
func dataSourceName(dbPath string) string {
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)",
		dbPath, busyTimeoutMillisecondsWanted)
}

// verifyPerConnectionPragmasTookEffect proves the DSN was understood. An
// unrecognized DSN key opens cleanly and configures nothing, so without this
// a typo would leave the pool silently at SQLite's defaults.
func verifyPerConnectionPragmasTookEffect(db *sql.DB) error {
	var busyTimeoutMilliseconds int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		return fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		return fmt.Errorf("busy_timeout is %d, want %d: the DSN did not take effect",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}

	var foreignKeysEnabled bool
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeysEnabled); err != nil {
		return fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if !foreignKeysEnabled {
		return fmt.Errorf("foreign_keys is off: the DSN did not take effect")
	}
	return nil
}

// Open opens or creates a hook-store database at dbPath. The parent directory
// is created if missing. WAL is enabled for concurrent reads during writes.
func Open(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		return nil, err
	}

	// journal_mode is the one pragma that belongs here rather than in the DSN:
	// it is a property of the database file, so setting it once is permanent.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	if err := verifyPerConnectionPragmasTookEffect(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection for advanced queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS hooks (
			id          TEXT PRIMARY KEY,
			harness     TEXT NOT NULL,
			event       TEXT NOT NULL,
			matcher     TEXT NOT NULL DEFAULT '',
			command     TEXT NOT NULL,
			scope_kind  TEXT NOT NULL,
			scope_id    TEXT NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CHECK (scope_kind IN ('global', 'instance', 'session')),
			CHECK (scope_kind = 'global' OR scope_id != '')
		);
		CREATE INDEX IF NOT EXISTS idx_hooks_harness_event ON hooks(harness, event);
		CREATE INDEX IF NOT EXISTS idx_hooks_scope ON hooks(scope_kind, scope_id);
		CREATE INDEX IF NOT EXISTS idx_hooks_enabled ON hooks(enabled);
	`)
	return err
}
