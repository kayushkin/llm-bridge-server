// Package snapshotstore captures point-in-time file contents for tool-call
// before/after diffs.
//
// A Store has two backends:
//   - SQLite: metadata rows keyed by (session_id, tool_use_id, phase,
//     file_path), recording blob SHA, size, and flags (is_binary, too_large,
//     missing). The file_path is part of the PK so a single tool call (e.g.
//     a Bash command touching N files) can record many snapshots per phase.
//   - A bare git repo: raw file contents stored as content-addressed blobs
//     via `git hash-object -w`. Git handles delta compression and dedup;
//     identical content across many tool calls costs ~no additional disk.
//
// The store is a pure data layer. Deciding when to capture (which tools,
// which events) is the caller's responsibility.
package snapshotstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps the snapshot SQLite database and the git blob sidecar.
type Store struct {
	db      *sql.DB
	gitDir  string
	maxSize int64
}

// Config configures a Store.
type Config struct {
	// DBPath is the SQLite database path. Parent dir is created if missing.
	DBPath string
	// GitDir is the bare git repo used as the blob store. Initialized on
	// first use if missing.
	GitDir string
	// MaxSize is the per-file capture cap in bytes. Files larger than this
	// are recorded with too_large=true and no blob. 0 means 1 MiB default.
	MaxSize int64
}

const defaultMaxSize = 1 << 20 // 1 MiB

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

// Open opens or creates a snapshot store.
func Open(cfg Config) (*Store, error) {
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("snapshotstore: DBPath is required")
	}
	if cfg.GitDir == "" {
		return nil, fmt.Errorf("snapshotstore: GitDir is required")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(cfg.DBPath))
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

	max := cfg.MaxSize
	if max <= 0 {
		max = defaultMaxSize
	}

	s := &Store{db: db, gitDir: cfg.GitDir, maxSize: max}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.initBlobRepo(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init blob repo: %w", err)
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

// GitDir returns the bare git directory used for blob storage.
func (s *Store) GitDir() string {
	return s.gitDir
}

// MaxSize returns the configured per-file capture cap in bytes.
func (s *Store) MaxSize() int64 {
	return s.maxSize
}

// migrate brings the schema to the current version. Steps must be idempotent
// and ordered — bumping user_version after each completed step lets fresh
// installs and existing DBs converge to the same state.
func (s *Store) migrate() error {
	// v1: initial schema with PK (session_id, tool_use_id, phase).
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS snapshots (
			session_id   TEXT NOT NULL,
			tool_use_id  TEXT NOT NULL,
			phase        TEXT NOT NULL,
			file_path    TEXT NOT NULL,
			blob_sha     TEXT NOT NULL DEFAULT '',
			size         INTEGER NOT NULL DEFAULT 0,
			is_binary    INTEGER NOT NULL DEFAULT 0,
			too_large    INTEGER NOT NULL DEFAULT 0,
			missing      INTEGER NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, tool_use_id, phase),
			CHECK (phase IN ('before','after'))
		);
		CREATE INDEX IF NOT EXISTS idx_snapshots_created ON snapshots(created_at);
		CREATE INDEX IF NOT EXISTS idx_snapshots_sha ON snapshots(blob_sha);
	`); err != nil {
		return err
	}

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	// v2: widen PK to include file_path so one tool call can record snapshots
	// of multiple files (e.g. a Bash command that touches N paths).
	if version < 2 {
		if err := s.migrateV2(); err != nil {
			return fmt.Errorf("migrate v2: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = 2`); err != nil {
			return fmt.Errorf("bump user_version to 2: %w", err)
		}
	}

	return nil
}

// migrateV2 rebuilds the snapshots table with file_path included in the PK.
// SQLite has no in-place way to alter a primary key, so we do the standard
// create-new / copy / drop / rename dance inside a transaction.
func (s *Store) migrateV2() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE snapshots_v2 (
			session_id   TEXT NOT NULL,
			tool_use_id  TEXT NOT NULL,
			phase        TEXT NOT NULL,
			file_path    TEXT NOT NULL,
			blob_sha     TEXT NOT NULL DEFAULT '',
			size         INTEGER NOT NULL DEFAULT 0,
			is_binary    INTEGER NOT NULL DEFAULT 0,
			too_large    INTEGER NOT NULL DEFAULT 0,
			missing      INTEGER NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, tool_use_id, phase, file_path),
			CHECK (phase IN ('before','after'))
		)`,
		`INSERT INTO snapshots_v2
			SELECT session_id, tool_use_id, phase, file_path, blob_sha, size,
			       is_binary, too_large, missing, created_at
			FROM snapshots`,
		`DROP TABLE snapshots`,
		`ALTER TABLE snapshots_v2 RENAME TO snapshots`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_created ON snapshots(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_sha ON snapshots(blob_sha)`,
	}
	for _, sqlStmt := range stmts {
		if _, err := tx.Exec(sqlStmt); err != nil {
			return fmt.Errorf("exec %q: %w", sqlStmt, err)
		}
	}
	return tx.Commit()
}
