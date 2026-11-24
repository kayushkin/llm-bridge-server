package modelstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store is the model/provider registry.
type Store struct {
	db   *sql.DB
	path string
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

// DefaultPath returns the default store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "model-store", "store.db")
}

// Open opens or creates a model store.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	os.MkdirAll(filepath.Dir(path), 0755)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS providers (
			id   TEXT PRIMARY KEY,
			name TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS models (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL REFERENCES providers(id),
			name TEXT NOT NULL,
			max_tokens INTEGER DEFAULT 200000,
			input_cost REAL DEFAULT 0,
			output_cost REAL DEFAULT 0,
			enabled INTEGER DEFAULT 1,
			priority INTEGER DEFAULT 100,
			short_name TEXT DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS model_aliases (
			alias TEXT PRIMARY KEY,
			model_id TEXT NOT NULL REFERENCES models(id)
		);

		CREATE TABLE IF NOT EXISTS model_roles (
			role TEXT PRIMARY KEY,
			model_id TEXT NOT NULL REFERENCES models(id)
		);

		CREATE TABLE IF NOT EXISTS model_role_fallbacks (
			role TEXT NOT NULL REFERENCES model_roles(role),
			position INTEGER NOT NULL,
			model_id TEXT NOT NULL REFERENCES models(id),
			PRIMARY KEY (role, position),
			UNIQUE (role, model_id)
		);
	`)
	if err != nil {
		return err
	}

	// Add columns to existing databases (safe to run multiple times)
	s.db.Exec(`ALTER TABLE models ADD COLUMN enabled INTEGER DEFAULT 1`)
	s.db.Exec(`ALTER TABLE models ADD COLUMN priority INTEGER DEFAULT 100`)
	s.db.Exec(`ALTER TABLE models ADD COLUMN short_name TEXT DEFAULT ''`)

	return nil
}

// ProviderFor returns the provider ID for a model.
// This is useful for auth-store to resolve credentials by provider.
func (s *Store) ProviderFor(modelID string) (string, error) {
	m, err := s.ResolveModel(modelID)
	if err != nil {
		return "", err
	}
	return m.Provider, nil
}
