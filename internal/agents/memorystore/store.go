package memorystore

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
	return postgresstorage.Module{Schema: "memories", Role: "memories_module", Migrations: migrations}, nil
}

// New returns the store over db, the module's pool from postgresstorage.Open.
// The server provisions, migrates and opens the module, then hands the pool
// here.
func New(db *sql.DB) *Store {
	return &Store{
		db:       db,
		embedder: NewEmbedder(),
	}
}
