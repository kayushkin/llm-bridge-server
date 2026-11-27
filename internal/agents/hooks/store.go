// Package hooks is the registry for bridge-managed harness hooks.
//
// Each row is a hook handler (harness, event, matcher, shell command) bound
// to a scope (global, instance, session). The llm-bridge server queries this
// store at session spawn to decide which hooks to wire into the harness's
// native hook mechanism (for Claude Code: synthesized into a settings JSON
// passed via --settings).
//
// The store itself is a pure data layer — it does not execute hooks, render
// settings JSON, or talk to harnesses. That belongs to the server.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every time it hands back is in UTC.
package hooks

import (
	"database/sql"
	"embed"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "hooks", Role: "hooks_module", Migrations: migrations}, nil
}

// Store is the hook-store module.
type Store struct {
	db *sql.DB
}

// New returns the store over db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB returns the module's pool, for tests that read the table directly.
func (s *Store) DB() *sql.DB {
	return s.db
}
