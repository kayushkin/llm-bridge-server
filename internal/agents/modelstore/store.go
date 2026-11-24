package modelstore

import (
	"database/sql"
	"embed"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is the model/provider registry, kept in the Postgres module models.
type Store struct {
	db *sql.DB
}

// Provider represents a model provider (Anthropic, OpenAI, etc).
type Provider struct {
	ID   string `json:"id"`   // e.g. "anthropic", "openai", "google", "ollama"
	Name string `json:"name"` // display name
}

// Model represents an available model.
type Model struct {
	ID       string `json:"id"`       // e.g. "claude-sonnet-4-5-20250929"
	Provider string `json:"provider"` // provider ID
	Name     string `json:"name"`     // display name
	// ShortName is the shortest nickname that still tells this model apart from
	// its siblings — "opus-4.6" rather than "Claude Opus 4.6". It exists for the
	// dense places in a UI, like a model picker crammed into an already crowded
	// top bar, where the full name simply does not fit and would be truncated
	// into something ambiguous. Name stays the canonical display name; this is a
	// second, terser label for those spots, never a replacement.
	ShortName  string   `json:"short_name"`
	Aliases    []string `json:"aliases"`     // e.g. ["sonnet", "claude-sonnet"]
	MaxTokens  int      `json:"max_tokens"`  // context window
	InputCost  float64  `json:"input_cost"`  // per million tokens
	OutputCost float64  `json:"output_cost"` // per million tokens
	Enabled    bool     `json:"enabled"`     // whether this model is available for use
	Priority   int      `json:"priority"`    // failover priority (lower = preferred, 0 = highest)
}

// ModelStatus combines model info with health data for dashboard display.
type ModelStatus struct {
	Model
	Health *ModelHealth `json:"health"`
}

// Module is the Postgres schema and role the registry keeps its records in,
// with the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "models", Role: "models_module", Migrations: migrations}, nil
}

// New returns the registry over db, the module's pool from
// postgresstorage.Open. The server provisions, migrates and opens the module;
// whoever opened the pool closes it.
func New(db *sql.DB) *Store { return &Store{db: db} }

// ProviderFor returns the provider ID for a model.
// This is useful for auth-store to resolve credentials by provider.
func (s *Store) ProviderFor(modelID string) (string, error) {
	m, err := s.ResolveModel(modelID)
	if err != nil {
		return "", err
	}
	return m.Provider, nil
}
