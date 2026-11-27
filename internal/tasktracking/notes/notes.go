// Package notes is the module that keeps the operator's notes, ranked list
// entries and workspaces. It was noteboard until 2026-11-27, when noteboard's
// todos became issues (internal/tasktracking/issues) and the rest stayed here.
// The records live in the Postgres module Module names, and the items package
// reads and writes them.
package notes

import (
	"database/sql"
	"embed"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role notes keeps its items in, with the
// migrations that build them. Until 2026-11-27 they were named noteboard and
// noteboard_module; an install that holds them is renamed at start, and the
// todos it held move to issues (internal/tasktracking/noteboardsplit).
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "notes", Role: "notes_module", Migrations: migrations,
		FormerNames: []postgresstorage.FormerName{{Schema: "noteboard", Role: "noteboard_module"}}}, nil
}

// Holding is what notes keeps: notes, ranked list entries and workspaces.
// A todo is an issue, kept by the issues module.
var Holding = items.Holding{
	Name:  "notes",
	Types: []string{model.TypeNote, model.TypeRank, model.TypeWorkspace},
	TypesKeptElsewhere: map[string]string{
		model.TypeTodo: "a todo is an issue: write it to /issues/items",
	},
}

// New returns the notes store over db, the pool postgresstorage.Open gave
// Module. The schema must already be migrated.
func New(db *sql.DB) *items.Store { return items.New(db, Holding) }
