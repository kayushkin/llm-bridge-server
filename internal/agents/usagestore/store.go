package usagestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Store tracks token usage per agent/orchestrator/model.
type Store struct {
	db   *sql.DB
	path string
}

// UsageRecord tracks token usage.
type UsageRecord struct {
	Date         string  `json:"date"`         // YYYY-MM-DD
	Agent        string  `json:"agent"`        // agent name
	Orchestrator string  `json:"orchestrator"` // "inber", "claude-code", etc.
	Model        string  `json:"model"`        // model ID
	Provider     string  `json:"provider"`
	Session      string  `json:"session,omitempty"` // session ID
	Harness      string  `json:"harness,omitempty"` // harness type (claude_code, codex, etc.)
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Requests     int     `json:"requests"`
	CostUSD      float64 `json:"cost_usd"`
}

// Stats holds aggregated usage statistics.
type Stats struct {
	TotalCostUSD  float64  `json:"total_cost_usd"`
	TotalInput    int64    `json:"total_input_tokens"`
	TotalOutput   int64    `json:"total_output_tokens"`
	TotalRequests int      `json:"total_requests"`
	Models        []string `json:"models"`
	Agents        []string `json:"agents,omitempty"`
	Harnesses     []string `json:"harnesses,omitempty"`
	Sessions      []string `json:"sessions,omitempty"`
}

// QueryFilter defines filters for querying usage records and stats.
type QueryFilter struct {
	Agent        string
	Orchestrator string
	Model        string
	Session      string
	Harness      string
	DateFrom     string // YYYY-MM-DD
	DateTo       string // YYYY-MM-DD
}

// DefaultPath returns the default store path.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "usage-store", "usage.db")
}

// Open opens or creates a usage store.
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
	if err := s.migrateLimits(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate limits: %w", err)
	}
	if err := s.migrateSpend(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate spend: %w", err)
	}

	return s, nil
}

// DB exposes the underlying *sql.DB for read-only callers (e.g. token aggregation handlers).
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the on-disk path of this store.
func (s *Store) Path() string { return s.path }

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS usage (
			date          TEXT NOT NULL,
			agent         TEXT NOT NULL,
			orchestrator  TEXT NOT NULL DEFAULT 'inber',
			model         TEXT NOT NULL,
			provider      TEXT NOT NULL DEFAULT '',
			session       TEXT NOT NULL DEFAULT '',
			harness       TEXT NOT NULL DEFAULT '',
			input_tokens  INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			requests      INTEGER DEFAULT 0,
			cost_usd      REAL DEFAULT 0,
			PRIMARY KEY (date, agent, orchestrator, model, session)
		);
		CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session);
		CREATE INDEX IF NOT EXISTS idx_usage_harness ON usage(harness);
		CREATE INDEX IF NOT EXISTS idx_usage_agent ON usage(agent);
	`)
	if err != nil {
		return err
	}

	return s.migrateAddSessionHarness()
}

// migrateAddSessionHarness adds session and harness columns if missing.
func (s *Store) migrateAddSessionHarness() error {
	var tableSql string
	s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='usage'`).Scan(&tableSql)

	if strings.Contains(tableSql, "session") {
		return nil
	}

	// Rebuild table with new columns and new primary key.
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS usage_new (
			date          TEXT NOT NULL,
			agent         TEXT NOT NULL,
			orchestrator  TEXT NOT NULL DEFAULT 'inber',
			model         TEXT NOT NULL,
			provider      TEXT NOT NULL DEFAULT '',
			session       TEXT NOT NULL DEFAULT '',
			harness       TEXT NOT NULL DEFAULT '',
			input_tokens  INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			requests      INTEGER DEFAULT 0,
			cost_usd      REAL DEFAULT 0,
			PRIMARY KEY (date, agent, orchestrator, model, session)
		);
		INSERT OR IGNORE INTO usage_new (date, agent, orchestrator, model, provider, session, harness, input_tokens, output_tokens, requests, cost_usd)
			SELECT date, agent, COALESCE(orchestrator, 'inber'), model, provider, '', '', input_tokens, output_tokens, requests, cost_usd FROM usage;
		DROP TABLE usage;
		ALTER TABLE usage_new RENAME TO usage;
		CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session);
		CREATE INDEX IF NOT EXISTS idx_usage_harness ON usage(harness);
		CREATE INDEX IF NOT EXISTS idx_usage_agent ON usage(agent);
	`)
	return err
}
