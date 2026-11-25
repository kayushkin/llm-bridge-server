// Package snapshotstore captures point-in-time file contents for tool-call
// before/after diffs.
//
// A Store has two backends:
//   - Postgres: metadata rows keyed by (session_id, tool_use_id, phase,
//     file_path), recording blob SHA, size, and flags (is_binary, too_large,
//     missing), in the schema of Module. The file_path is part of the key so
//     a single tool call (e.g. a Bash command touching N files) can record
//     many snapshots per phase.
//   - A bare git repo on disk: raw file contents stored as content-addressed
//     blobs via `git hash-object -w`. Git handles delta compression and
//     dedup; identical content across many tool calls costs ~no additional
//     disk.
//
// The store is a pure data layer. Deciding when to capture (which tools,
// which events) is the caller's responsibility.
package snapshotstore

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its rows in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "snapshots", Role: "snapshots_module", Migrations: migrations}, nil
}

// Store wraps the snapshot rows and the git blob sidecar.
type Store struct {
	db      *sql.DB
	gitDir  string
	maxSize int64
}

// Config configures a Store.
type Config struct {
	// GitDir is the bare git repo used as the blob store. Initialized on
	// first use if missing.
	GitDir string
	// MaxSize is the per-file capture cap in bytes. Files larger than this
	// are recorded with too_large=true and no blob. 0 means 1 MiB default.
	MaxSize int64
}

const defaultMaxSize = 1 << 20 // 1 MiB

// New returns the store over db, the module's pool from postgresstorage.Open,
// with its blobs in cfg.GitDir, which it initializes if missing.
func New(db *sql.DB, cfg Config) (*Store, error) {
	if cfg.GitDir == "" {
		return nil, fmt.Errorf("snapshotstore: GitDir is required")
	}
	max := cfg.MaxSize
	if max <= 0 {
		max = defaultMaxSize
	}
	s := &Store{db: db, gitDir: cfg.GitDir, maxSize: max}
	if err := s.initBlobRepo(); err != nil {
		return nil, fmt.Errorf("init blob repo: %w", err)
	}
	return s, nil
}

// DB returns the module's pool, for tests that read the table directly.
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
