// Package harnessstore keeps the harness instance registry, the machines that
// host them, credential bindings, runner enrollment records and harness type
// display metadata. Static configuration only: runtime state (active
// sessions, credential slots) lives in llm-bridge-server.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every time it hands back is in UTC.
package harnessstore

import (
	"database/sql"
	"embed"
	"time"

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
	return postgresstorage.Module{Schema: "harnesses", Role: "harnesses_module", Migrations: migrations}, nil
}

// Store manages harness/machine/enrollment state.
type Store struct {
	db *sql.DB
}

// New returns the store over db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB returns the module's pool, for tests that read the tables directly.
func (s *Store) DB() *sql.DB {
	return s.db
}

// inUTC hands a time read from Postgres back in UTC, as the SQLite store did;
// the driver gives timestamptz values in the process's local zone.
func inUTC(at *time.Time) {
	*at = at.UTC()
}
