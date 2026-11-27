// Package usage keeps what this server knows about subscription usage
// and API spend: snapshots of each subscription's limit windows (Anthropic's
// OAuth usage, Codex's rate limits), and what each Anthropic API key cost per
// day, with the credit bought for the account.
//
// It keeps its records in the Postgres module usage (role usage_module). The
// server provisions, migrates and opens the module, then hands the pool to
// New.
package usage

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
	return postgresstorage.Module{Schema: "usage", Role: "usage_module", Migrations: migrations}, nil
}

// Store keeps limit snapshots and spend in the module's schema.
type Store struct {
	db *sql.DB
}

// New returns the store over db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store { return &Store{db: db} }
