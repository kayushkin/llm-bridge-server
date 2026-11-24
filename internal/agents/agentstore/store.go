// Package agentstore provides a unified store for agent identity, nature, and configs.
package agentstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DefaultPath returns the default database path (~/.config/agent-store/agents.db)
func DefaultPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "."
	}
	return filepath.Join(homeDir, ".config", "agent-store", "agents.db")
}

// Store is the main handle for agent storage.
type Store struct {
	db       *sql.DB
	embedder *Embedder
}

// Open opens or creates the agent store at the given path.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	s := &Store{db: db, embedder: NewEmbedder()}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) migrate() error {
	// Pre-schema renames (2026-05-11): must run BEFORE schemaSQL, otherwise
	// the CREATE TABLE IF NOT EXISTS in schemaSQL would create empty new-name
	// tables alongside the existing old-name ones, splitting data.
	//
	// Partial-state recovery: if a previous migration attempt got far enough
	// to run schemaSQL but didn't complete the renames, we'll have empty
	// new-name placeholder tables blocking the RENAME. Drop those first when
	// the old-name table still has data to migrate.
	renamePairs := []struct{ oldName, newName string }{
		{"orchestrators", "harness"},
		{"agent_orchestrators", "agent_harness"},
		{"agent_tools", "agent_harness_tools"},
	}
	for _, p := range renamePairs {
		var oldExists, newExists int
		s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, p.oldName).Scan(&oldExists)
		s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, p.newName).Scan(&newExists)
		if oldExists == 1 && newExists == 1 {
			// Both exist; drop the new-name table if it's empty (placeholder from a previously failed migration).
			var rowCount int
			if err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, p.newName)).Scan(&rowCount); err == nil && rowCount == 0 {
				s.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, p.newName))
			}
		}
	}
	preMigrations := []string{
		// Table renames (FROM old name TO new name)
		"ALTER TABLE orchestrators RENAME TO harness",
		"ALTER TABLE agent_orchestrators RENAME TO agent_harness",
		"ALTER TABLE agent_tools RENAME TO agent_harness_tools",
		// Column renames (FROM old name TO new name, run after table renames)
		"ALTER TABLE agent_harness RENAME COLUMN orchestrator_id TO harness_id",
		"ALTER TABLE agent_harness RENAME COLUMN orchestrator_agent_id TO harness_agent_id",
		"ALTER TABLE agent_harness_tools RENAME COLUMN orchestrator_id TO harness_id",
		"ALTER TABLE agent_system_prompt_refs RENAME COLUMN orchestrator_id TO harness_id",
		"ALTER TABLE file_distributions RENAME COLUMN orchestrator_id TO harness_id",
		"ALTER TABLE agent_status RENAME COLUMN orchestrator_id TO harness_id",
		// Drop the old index names; new indexes are created by schemaSQL.
		"DROP INDEX IF EXISTS idx_agent_orch_orch",
		"DROP INDEX IF EXISTS idx_file_dist_orch",
		// Add parent_agent_id BEFORE schemaSQL runs so the CREATE INDEX in schema can succeed.
		"ALTER TABLE agents ADD COLUMN parent_agent_id INTEGER REFERENCES agents(id) ON DELETE SET NULL",
	}
	for _, m := range preMigrations {
		s.db.Exec(m) // ignore errors when rename already happened, table never existed, or column already renamed
	}
	if err := s.setAsidePerFilePromptSections(); err != nil {
		return fmt.Errorf("set aside per-file prompt sections: %w", err)
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return err
	}
	// Post-schema migrations: add columns that may not exist in older databases.
	postMigrations := []string{
		"ALTER TABLE agent_harness ADD COLUMN is_default INTEGER DEFAULT 0",
		"ALTER TABLE agent_harness ADD COLUMN subagent_allow TEXT",
		"ALTER TABLE tracked_files ADD COLUMN ignored_by_rule_id INTEGER",
		"ALTER TABLE prompt_collections ADD COLUMN retired_at INTEGER NOT NULL DEFAULT 0",
	}
	for _, m := range postMigrations {
		s.db.Exec(m) // ignore "duplicate column" errors
	}
	return nil
}

// setAsidePerFilePromptSections moves the first prompt_sections table out of
// the way before schemaSQL creates its replacement. That table tied every
// section to a file through applies_to ('claude' | 'agents'); the replacement
// names no file. The old rows are renamed, not dropped: they were imported by
// a splitter that broke on code fences, so nothing reads them, but a rename
// costs nothing and a drop cannot be undone.
func (s *Store) setAsidePerFilePromptSections() error {
	var hasAppliesTo int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('prompt_sections') WHERE name='applies_to'`).Scan(&hasAppliesTo)
	if err != nil {
		return err
	}
	if hasAppliesTo == 0 {
		return nil
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_prompt_sections_collection`); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE prompt_sections RENAME TO prompt_sections_legacy_per_file`)
	return err
}

func now() int64 { return time.Now().Unix() }

// ============================================
// AGENTS
// ============================================

// Agent is the wire shape of /agents. The json tags are load-bearing, not
// cosmetic: without them encoding/json falls back to the Go field names, and
// while it matches those case-insensitively it does NOT ignore underscores --
// so a snake_case "display_name" would not reach DisplayName and would be
// dropped on write with a cheerful 201. That is exactly what happened here, and
// it destroyed the display name of every agent that bridge-ui's Agents page
// ever wrote. snake_case is the convention the rest of this store already
// speaks (AgentStatus, FullAgentConfig, and the ?expanded=true response).
type Agent struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Emoji       string `json:"emoji"`
	Projects    string `json:"projects"` // comma-separated
	Description string `json:"description"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) UpsertAgent(a *Agent) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = now()
	}
	a.UpdatedAt = now()
	if a.ID > 0 {
		_, err := s.db.Exec(`
			UPDATE agents SET slug=?, display_name=?, emoji=?, projects=?, description=?, role=?, enabled=?, updated_at=?
			WHERE id=?`,
			a.Slug, a.DisplayName, a.Emoji, a.Projects, a.Description, a.Role, boolToInt(a.Enabled), a.UpdatedAt, a.ID)
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO agents (slug, display_name, emoji, projects, description, role, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			display_name=excluded.display_name, emoji=excluded.emoji, projects=excluded.projects,
			description=excluded.description, role=excluded.role, enabled=excluded.enabled,
			updated_at=excluded.updated_at
	`, a.Slug, a.DisplayName, a.Emoji, a.Projects, a.Description, a.Role, boolToInt(a.Enabled), a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return err
	}
	// Always fetch the canonical ID — LastInsertId is unreliable with ON CONFLICT
	return s.db.QueryRow(`SELECT id FROM agents WHERE slug=?`, a.Slug).Scan(&a.ID)
}

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	a := &Agent{}
	var enabled int
	var emoji, projects, description, role *string
	err := row.Scan(&a.ID, &a.Slug, &a.DisplayName, &emoji, &projects, &description, &role, &enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	if emoji != nil { a.Emoji = *emoji }
	if projects != nil { a.Projects = *projects }
	if description != nil { a.Description = *description }
	if role != nil { a.Role = *role }
	return a, nil
}

const agentCols = `id, slug, display_name, emoji, projects, description, role, enabled, created_at, updated_at`

func (s *Store) GetAgent(id int64) (*Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id=?`, id))
}

func (s *Store) GetAgentBySlug(slug string) (*Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE slug=?`, slug))
}

func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AgentWithHarness is an agent entry expanded per harness.
type AgentWithHarness struct {
	Slug             string
	DisplayName      string
	Emoji            string
	Projects         string
	Description      string
	Role             string
	Enabled          bool
	Harness     string // harness ID
	HarnessName string // what the harness calls this agent
	Model            string
	IsDefault        bool   // true if this is the harness's default agent
	HarnessEmoji        string // harness emoji
}

// ListAgentsExpanded returns one row per agent+harness pair.
func (s *Store) ListAgentsExpanded() ([]AgentWithHarness, error) {
	rows, err := s.db.Query(`
		SELECT a.slug, COALESCE(a.display_name,''), COALESCE(a.emoji,''), COALESCE(a.projects,''),
		       COALESCE(a.description,''), COALESCE(a.role,''), a.enabled,
		       ao.harness_id, ao.harness_agent_id, COALESCE(ao.model_primary, ''),
		       CASE WHEN o.default_agent_id = a.id THEN 1 ELSE 0 END,
		       COALESCE(o.emoji, '')
		FROM agents a
		JOIN agent_harness ao ON a.id = ao.agent_id
		LEFT JOIN harness o ON o.id = ao.harness_id
		WHERE ao.shelved = 0
		ORDER BY ao.harness_id, a.slug
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentWithHarness
	for rows.Next() {
		var a AgentWithHarness
		var enabled, isDefault int
		if err := rows.Scan(&a.Slug, &a.DisplayName, &a.Emoji, &a.Projects, &a.Description, &a.Role, &enabled,
			&a.Harness, &a.HarnessName, &a.Model, &isDefault, &a.HarnessEmoji); err != nil {
			return nil, err
		}
		a.Enabled = enabled != 0
		a.IsDefault = isDefault != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAgent(id int64) error {
	_, err := s.db.Exec("DELETE FROM agents WHERE id=?", id)
	return err
}

// ============================================
// ORCHESTRATORS
// ============================================

type Harness struct {
	ID             string
	DisplayName    string
	DefaultAgentID *int64
	ConfigPath     string
	APIEndpoint    string
	CreatedAt      int64
	UpdatedAt      int64
}

func (s *Store) UpsertHarness(o *Harness) error {
	if o.CreatedAt == 0 {
		o.CreatedAt = now()
	}
	o.UpdatedAt = now()
	_, err := s.db.Exec(`
		INSERT INTO harness (id, display_name, default_agent_id, config_path, api_endpoint, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name=excluded.display_name, default_agent_id=excluded.default_agent_id,
			config_path=excluded.config_path, api_endpoint=excluded.api_endpoint,
			updated_at=excluded.updated_at
	`, o.ID, o.DisplayName, o.DefaultAgentID, o.ConfigPath, o.APIEndpoint, o.CreatedAt, o.UpdatedAt)
	return err
}

func (s *Store) GetHarness(id string) (*Harness, error) {
	o := &Harness{ID: id}
	err := s.db.QueryRow(`
		SELECT display_name, default_agent_id, config_path, api_endpoint, created_at, updated_at
		FROM harness WHERE id=?`, id).Scan(
		&o.DisplayName, &o.DefaultAgentID, &o.ConfigPath, &o.APIEndpoint, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Store) ListHarnesses() ([]Harness, error) {
	rows, err := s.db.Query(`SELECT id, display_name, default_agent_id, config_path, api_endpoint, created_at, updated_at FROM harness ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Harness
	for rows.Next() {
		var o Harness
		if err := rows.Scan(&o.ID, &o.DisplayName, &o.DefaultAgentID, &o.ConfigPath, &o.APIEndpoint, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) DeleteHarness(id string) error {
	_, err := s.db.Exec("DELETE FROM harness WHERE id=?", id)
	return err
}

// ============================================
// AGENT ORCHESTRATORS
// ============================================

// AgentHarness is the wire shape of /agents/{slug}/harnesses. It carried the
// same untagged defect as Agent, and worse: almost every field here is
// multi-word, so a snake_case client silently lost nearly the whole config.
// See the note on Agent for the mechanism.
type AgentHarness struct {
	AgentID         int64  `json:"agent_id"`
	HarnessID       string `json:"harness_id"`
	HarnessAgentID  string `json:"harness_agent_id"`
	Enabled         bool   `json:"enabled"`
	ModelPrimary    string `json:"model_primary"`
	ModelFallbacks  string `json:"model_fallbacks"` // JSON array
	WorkspacePath   string `json:"workspace_path"`
	ThinkingBudget  int    `json:"thinking_budget"`
	ContextBudget   int    `json:"context_budget"`
	ContextTags     string `json:"context_tags"` // JSON array
	MaxTurns        int    `json:"max_turns"`
	MaxInputTokens  int    `json:"max_input_tokens"`
	MaxResponseTime int    `json:"max_response_time"` // seconds
	SystemPrompt    string `json:"system_prompt"`
	Project         string `json:"project"`
	Shelved         bool   `json:"shelved"`
	IsDefault       bool   `json:"is_default"`
	SubagentAllow   string `json:"subagent_allow"` // JSON array, e.g. '["*"]'
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

func (s *Store) UpsertAgentHarness(ao *AgentHarness) error {
	if ao.CreatedAt == 0 {
		ao.CreatedAt = now()
	}
	ao.UpdatedAt = now()
	_, err := s.db.Exec(`
		INSERT INTO agent_harness (agent_id, harness_id, harness_agent_id, enabled, model_primary, model_fallbacks, workspace_path, thinking_budget, context_budget, context_tags, max_turns, max_input_tokens, max_response_time, system_prompt, project, shelved, is_default, subagent_allow, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, harness_id) DO UPDATE SET
			harness_agent_id=excluded.harness_agent_id, enabled=excluded.enabled,
			model_primary=excluded.model_primary, model_fallbacks=excluded.model_fallbacks,
			workspace_path=excluded.workspace_path, thinking_budget=excluded.thinking_budget,
			context_budget=excluded.context_budget, context_tags=excluded.context_tags,
			max_turns=excluded.max_turns, max_input_tokens=excluded.max_input_tokens,
			max_response_time=excluded.max_response_time, system_prompt=excluded.system_prompt,
			project=excluded.project, shelved=excluded.shelved, is_default=excluded.is_default,
			subagent_allow=excluded.subagent_allow, updated_at=excluded.updated_at
	`, ao.AgentID, ao.HarnessID, ao.HarnessAgentID, boolToInt(ao.Enabled), ao.ModelPrimary, ao.ModelFallbacks, ao.WorkspacePath, ao.ThinkingBudget, ao.ContextBudget, ao.ContextTags, ao.MaxTurns, ao.MaxInputTokens, ao.MaxResponseTime, ao.SystemPrompt, ao.Project, boolToInt(ao.Shelved), boolToInt(ao.IsDefault), ao.SubagentAllow, ao.CreatedAt, ao.UpdatedAt)
	return err
}

const aoCols = `agent_id, harness_id, harness_agent_id, enabled, model_primary, model_fallbacks, workspace_path, thinking_budget, context_budget, context_tags, max_turns, max_input_tokens, max_response_time, system_prompt, project, shelved, is_default, subagent_allow, created_at, updated_at`

func scanAO(row interface{ Scan(...any) error }) (AgentHarness, error) {
	var ao AgentHarness
	var enabled, shelved, isDefault int
	var modelFallbacks, contextTags, systemPrompt, workspacePath, project, harnessAgentID, subagentAllow *string
	err := row.Scan(&ao.AgentID, &ao.HarnessID, &harnessAgentID, &enabled, &ao.ModelPrimary, &modelFallbacks, &workspacePath, &ao.ThinkingBudget, &ao.ContextBudget, &contextTags, &ao.MaxTurns, &ao.MaxInputTokens, &ao.MaxResponseTime, &systemPrompt, &project, &shelved, &isDefault, &subagentAllow, &ao.CreatedAt, &ao.UpdatedAt)
	ao.Enabled = enabled == 1
	ao.Shelved = shelved == 1
	ao.IsDefault = isDefault == 1
	if harnessAgentID != nil { ao.HarnessAgentID = *harnessAgentID }
	if modelFallbacks != nil { ao.ModelFallbacks = *modelFallbacks }
	if workspacePath != nil { ao.WorkspacePath = *workspacePath }
	if contextTags != nil { ao.ContextTags = *contextTags }
	if systemPrompt != nil { ao.SystemPrompt = *systemPrompt }
	if project != nil { ao.Project = *project }
	if subagentAllow != nil { ao.SubagentAllow = *subagentAllow }
	return ao, err
}

func (s *Store) GetAgentHarnesses(agentID int64) ([]AgentHarness, error) {
	rows, err := s.db.Query(`SELECT `+aoCols+` FROM agent_harness WHERE agent_id=? ORDER BY harness_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHarness
	for rows.Next() {
		ao, err := scanAO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ao)
	}
	return out, rows.Err()
}

func (s *Store) GetHarnessAgents(harnessID string) ([]AgentHarness, error) {
	rows, err := s.db.Query(`SELECT `+aoCols+` FROM agent_harness WHERE harness_id=? ORDER BY agent_id`, harnessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHarness
	for rows.Next() {
		ao, err := scanAO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ao)
	}
	return out, rows.Err()
}

// GetAgentByHarnessID finds an agent by its harness-specific ID.
func (s *Store) GetAgentByHarnessID(harnessID, harnessAgentID string) (*Agent, error) {
	var agentID int64
	err := s.db.QueryRow(`SELECT agent_id FROM agent_harness WHERE harness_id=? AND harness_agent_id=?`,
		harnessID, harnessAgentID).Scan(&agentID)
	if err != nil {
		return nil, err
	}
	return s.GetAgent(agentID)
}

func (s *Store) DeleteAgentHarness(agentID int64, harnessID string) error {
	_, err := s.db.Exec("DELETE FROM agent_harness WHERE agent_id=? AND harness_id=?", agentID, harnessID)
	return err
}

// ============================================
// AGENT TOOLS
// ============================================

func (s *Store) SetAgentTools(agentID int64, harnessID string, tools []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM agent_harness_tools WHERE agent_id=? AND harness_id=?", agentID, harnessID); err != nil {
		return err
	}
	for _, t := range tools {
		if _, err := tx.Exec("INSERT INTO agent_harness_tools (agent_id, harness_id, tool_name) VALUES (?, ?, ?)", agentID, harnessID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListAgentTools(agentID int64, harnessID string) ([]string, error) {
	rows, err := s.db.Query("SELECT tool_name FROM agent_harness_tools WHERE agent_id=? AND harness_id=? ORDER BY tool_name", agentID, harnessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ============================================
// FULL AGENT CONFIG (for harnesses)
// ============================================

// FullAgentConfig is everything an harness needs for one agent.
type FullAgentConfig struct {
	Slug        string   `json:"slug"`
	DisplayName string   `json:"name"`
	Role        string   `json:"role"`
	Emoji       string   `json:"emoji,omitempty"`
	Model       string   `json:"model"`
	Thinking    int      `json:"thinking,omitempty"`
	Tools       []string `json:"tools"`
	ContextTags []string `json:"context_tags,omitempty"`
	ContextBudget int    `json:"context_budget,omitempty"`
	MaxTurns      int    `json:"max_turns,omitempty"`
	MaxInputTokens int   `json:"max_input_tokens,omitempty"`
	MaxResponseTime int  `json:"max_response_time,omitempty"`
	Workspace   string   `json:"workspace,omitempty"`
	Project     string   `json:"project,omitempty"`
	Projects    string   `json:"projects,omitempty"`
	SystemPrompt string  `json:"system_prompt,omitempty"`
	Shelved     bool     `json:"shelved,omitempty"`
	// Nature content
	Soul       string `json:"soul,omitempty"`
	Principles string `json:"principles,omitempty"`
	Values     string `json:"values,omitempty"`
	UserContext string `json:"user_context,omitempty"`
}

func (s *Store) GetFullAgentConfig(slug string, harnessID string) (*FullAgentConfig, error) {
	a, err := s.GetAgentBySlug(slug)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %s", slug)
	}

	// Get harness config
	row := s.db.QueryRow(`SELECT `+aoCols+` FROM agent_harness WHERE agent_id=? AND harness_id=?`, a.ID, harnessID)
	ao, err := scanAO(row)
	if err != nil {
		return nil, fmt.Errorf("agent %s not registered with harness %s", slug, harnessID)
	}

	tools, _ := s.ListAgentTools(a.ID, harnessID)
	if tools == nil {
		tools = []string{}
	}

	var contextTags []string
	if ao.ContextTags != "" {
		json.Unmarshal([]byte(ao.ContextTags), &contextTags)
	}

	cfg := &FullAgentConfig{
		Slug:            a.Slug,
		DisplayName:     a.DisplayName,
		Role:            a.Role,
		Emoji:           a.Emoji,
		Model:           ao.ModelPrimary,
		Thinking:        ao.ThinkingBudget,
		Tools:           tools,
		ContextTags:     contextTags,
		ContextBudget:   ao.ContextBudget,
		MaxTurns:        ao.MaxTurns,
		MaxInputTokens:  ao.MaxInputTokens,
		MaxResponseTime: ao.MaxResponseTime,
		Workspace:       ao.WorkspacePath,
		Project:         ao.Project,
		Projects:        a.Projects,
		SystemPrompt:    ao.SystemPrompt,
		Shelved:         ao.Shelved,
	}

	// Load nature content
	natures, _ := s.ListAgentNature(a.ID)
	for _, n := range natures {
		switch n.Kind {
		case "identity":
			cfg.Soul = n.Content
		case "principle":
			cfg.Principles = n.Content
		case "value":
			cfg.Values = n.Content
		case "user":
			cfg.UserContext = n.Content
		}
	}

	return cfg, nil
}

// GetAllAgentConfigs returns full configs for all agents registered with an harness.
func (s *Store) GetAllAgentConfigs(harnessID string) ([]FullAgentConfig, string, error) {
	// Get default agent slug
	var defaultSlug string
	h, err := s.GetHarness(harnessID)
	if err == nil && h.DefaultAgentID != nil {
		if da, err := s.GetAgent(*h.DefaultAgentID); err == nil {
			defaultSlug = da.Slug
		}
	}

	aos, err := s.GetHarnessAgents(harnessID)
	if err != nil {
		return nil, "", err
	}

	var configs []FullAgentConfig
	for _, ao := range aos {
		a, err := s.GetAgent(ao.AgentID)
		if err != nil {
			continue
		}
		cfg, err := s.GetFullAgentConfig(a.Slug, harnessID)
		if err != nil {
			continue
		}
		configs = append(configs, *cfg)
	}
	return configs, defaultSlug, nil
}

// ============================================
// AGENT SYSTEM PROMPT REFS
// ============================================

type AgentSystemPromptRef struct {
	HarnessID    string
	HostAgentID       int64
	ReferencedAgentID int64
	PromptLocation    string
	CreatedAt         int64
}

func (s *Store) UpsertSystemPromptRef(r *AgentSystemPromptRef) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = now()
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO agent_system_prompt_refs (harness_id, host_agent_id, referenced_agent_id, prompt_location, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, r.HarnessID, r.HostAgentID, r.ReferencedAgentID, r.PromptLocation, r.CreatedAt)
	return err
}

func (s *Store) ListSystemPromptRefs(hostAgentID int64) ([]AgentSystemPromptRef, error) {
	rows, err := s.db.Query(`
		SELECT harness_id, host_agent_id, referenced_agent_id, prompt_location, created_at
		FROM agent_system_prompt_refs WHERE host_agent_id=?`, hostAgentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentSystemPromptRef
	for rows.Next() {
		var r AgentSystemPromptRef
		if err := rows.Scan(&r.HarnessID, &r.HostAgentID, &r.ReferencedAgentID, &r.PromptLocation, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSystemPromptRef(harnessID string, hostID, refID int64, location string) error {
	_, err := s.db.Exec(`DELETE FROM agent_system_prompt_refs WHERE harness_id=? AND host_agent_id=? AND referenced_agent_id=? AND prompt_location=?`,
		harnessID, hostID, refID, location)
	return err
}

// ============================================
// AGENT NATURE
// ============================================

type AgentNature struct {
	ID          int64
	AgentID     int64
	Kind        string // identity, principle, value, user, project
	Content     string
	ContentHash string
	Priority    int
	SourcePath  string
	CreatedAt   int64
	UpdatedAt   int64
}

func (s *Store) UpsertAgentNature(n *AgentNature) error {
	if n.CreatedAt == 0 {
		n.CreatedAt = now()
	}
	n.UpdatedAt = now()
	if n.ID > 0 {
		_, err := s.db.Exec(`
			UPDATE agent_nature SET agent_id=?, kind=?, content=?, content_hash=?, priority=?, source_path=?, updated_at=?
			WHERE id=?`, n.AgentID, n.Kind, n.Content, n.ContentHash, n.Priority, n.SourcePath, n.UpdatedAt, n.ID)
		return err
	}
	res, err := s.db.Exec(`
		INSERT INTO agent_nature (agent_id, kind, content, content_hash, priority, source_path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, n.AgentID, n.Kind, n.Content, n.ContentHash, n.Priority, n.SourcePath, n.CreatedAt, n.UpdatedAt)
	if err != nil {
		return err
	}
	n.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) GetAgentNature(id int64) (*AgentNature, error) {
	n := &AgentNature{}
	err := s.db.QueryRow(`
		SELECT id, agent_id, kind, content, content_hash, priority, source_path, created_at, updated_at
		FROM agent_nature WHERE id=?`, id).Scan(
		&n.ID, &n.AgentID, &n.Kind, &n.Content, &n.ContentHash, &n.Priority, &n.SourcePath, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (s *Store) ListAgentNature(agentID int64) ([]AgentNature, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, kind, content, content_hash, priority, source_path, created_at, updated_at
		FROM agent_nature WHERE agent_id=? ORDER BY priority, id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentNature
	for rows.Next() {
		var n AgentNature
		if err := rows.Scan(&n.ID, &n.AgentID, &n.Kind, &n.Content, &n.ContentHash, &n.Priority, &n.SourcePath, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAgentNature(id int64) error {
	_, err := s.db.Exec("DELETE FROM agent_nature WHERE id=?", id)
	return err
}

// ============================================
// AGENT NAME ALIASES
// ============================================

type AgentNameAlias struct {
	AgentID   int64
	Alias     string
	Context   string
	CreatedAt int64
}

func (s *Store) UpsertAlias(a *AgentNameAlias) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = now()
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_name_aliases (agent_id, alias, context, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(agent_id, alias) DO UPDATE SET context=excluded.context
	`, a.AgentID, a.Alias, a.Context, a.CreatedAt)
	return err
}

func (s *Store) GetAgentAliases(agentID int64) ([]AgentNameAlias, error) {
	rows, err := s.db.Query(`SELECT agent_id, alias, context, created_at FROM agent_name_aliases WHERE agent_id=?`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentNameAlias
	for rows.Next() {
		var a AgentNameAlias
		if err := rows.Scan(&a.AgentID, &a.Alias, &a.Context, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAlias(agentID int64, alias string) error {
	_, err := s.db.Exec("DELETE FROM agent_name_aliases WHERE agent_id=? AND alias=?", agentID, alias)
	return err
}

// ResolveAgentName looks up an agent by slug or alias and returns it.
func (s *Store) ResolveAgentName(name string) (*Agent, error) {
	// Try slug first
	a, err := s.GetAgentBySlug(name)
	if err == nil {
		return a, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	// Try alias
	var agentID int64
	err = s.db.QueryRow(`SELECT agent_id FROM agent_name_aliases WHERE alias=?`, name).Scan(&agentID)
	if err != nil {
		return nil, fmt.Errorf("agent not found: %q", name)
	}
	return s.GetAgent(agentID)
}

// ============================================
// FILE DISTRIBUTIONS
// ============================================

type FileDistribution struct {
	ID              int64
	AgentID         int64
	HarnessID  string
	FilePath        string
	ContentHash     string
	DistributedAt   int64
	SourceNatureIDs string // JSON array
}

func (s *Store) RecordDistribution(agentID int64, harnessID string, filePath string, contentHash string, sourceNatureIDs []int64) (int64, error) {
	var idsJSON string
	if len(sourceNatureIDs) > 0 {
		b, _ := json.Marshal(sourceNatureIDs)
		idsJSON = string(b)
	}
	res, err := s.db.Exec(`
		INSERT INTO file_distributions (agent_id, harness_id, file_path, content_hash, distributed_at, source_nature_ids)
		VALUES (?, ?, ?, ?, ?, ?)
	`, agentID, harnessID, filePath, contentHash, now(), idsJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetLatestDistribution(filePath string) (*FileDistribution, error) {
	d := &FileDistribution{}
	err := s.db.QueryRow(`
		SELECT id, agent_id, harness_id, file_path, content_hash, distributed_at, COALESCE(source_nature_ids,'')
		FROM file_distributions WHERE file_path=? ORDER BY distributed_at DESC LIMIT 1`, filePath).Scan(
		&d.ID, &d.AgentID, &d.HarnessID, &d.FilePath, &d.ContentHash, &d.DistributedAt, &d.SourceNatureIDs)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Store) ListDistributions(agentID int64) ([]FileDistribution, error) {
	rows, err := s.db.Query(`
		SELECT id, agent_id, harness_id, file_path, content_hash, distributed_at, COALESCE(source_nature_ids,'')
		FROM file_distributions WHERE agent_id=? ORDER BY distributed_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileDistribution
	for rows.Next() {
		var d FileDistribution
		if err := rows.Scan(&d.ID, &d.AgentID, &d.HarnessID, &d.FilePath, &d.ContentHash, &d.DistributedAt, &d.SourceNatureIDs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDistribution(id int64) error {
	_, err := s.db.Exec("DELETE FROM file_distributions WHERE id=?", id)
	return err
}

// ============================================
// FILE SCANS
// ============================================

type FileScan struct {
	ID                 int64
	FileDistributionID int64
	ScannedAt          int64
	CurrentHash        string
	Status             string // unchanged, modified, missing, new
	DiffSummary        string
	Ingested           bool
}

func (s *Store) RecordScan(distributionID int64, currentHash string, status string, diffSummary string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO file_scans (file_distribution_id, scanned_at, current_hash, status, diff_summary)
		VALUES (?, ?, ?, ?, ?)
	`, distributionID, now(), currentHash, status, diffSummary)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) MarkIngested(scanID int64) error {
	_, err := s.db.Exec(`UPDATE file_scans SET ingested=1 WHERE id=?`, scanID)
	return err
}

func (s *Store) GetPendingScans() ([]FileDistribution, error) {
	// Distributions that have never been scanned, or whose latest scan is older than their distribution
	rows, err := s.db.Query(`
		SELECT d.id, d.agent_id, d.harness_id, d.file_path, d.content_hash, d.distributed_at, COALESCE(d.source_nature_ids,'')
		FROM file_distributions d
		LEFT JOIN file_scans fs ON fs.file_distribution_id = d.id
		GROUP BY d.id
		HAVING MAX(fs.scanned_at) IS NULL OR MAX(fs.scanned_at) < d.distributed_at
		ORDER BY d.distributed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileDistribution
	for rows.Next() {
		var d FileDistribution
		if err := rows.Scan(&d.ID, &d.AgentID, &d.HarnessID, &d.FilePath, &d.ContentHash, &d.DistributedAt, &d.SourceNatureIDs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetModifiedFiles() ([]FileScan, error) {
	rows, err := s.db.Query(`
		SELECT id, file_distribution_id, scanned_at, current_hash, status, COALESCE(diff_summary,''), ingested
		FROM file_scans WHERE status='modified' AND ingested=0 ORDER BY scanned_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileScan
	for rows.Next() {
		var fs FileScan
		var ingested int
		if err := rows.Scan(&fs.ID, &fs.FileDistributionID, &fs.ScannedAt, &fs.CurrentHash, &fs.Status, &fs.DiffSummary, &ingested); err != nil {
			return nil, err
		}
		fs.Ingested = ingested == 1
		out = append(out, fs)
	}
	return out, rows.Err()
}
// ============================================
// PROJECTS
// ============================================

type Project struct {
	ID          int64
	Slug        string
	Name        string
	Path        string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
}

func (s *Store) UpsertProject(p *Project) error {
	if p.CreatedAt == 0 {
		p.CreatedAt = now()
	}
	p.UpdatedAt = now()
	if p.ID > 0 {
		_, err := s.db.Exec(`
			UPDATE projects SET slug=?, name=?, path=?, description=?, updated_at=? WHERE id=?`,
			p.Slug, p.Name, p.Path, p.Description, p.UpdatedAt, p.ID)
		return err
	}
	res, err := s.db.Exec(`
		INSERT INTO projects (slug, name, path, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			name=excluded.name, path=excluded.path, description=excluded.description, updated_at=excluded.updated_at
	`, p.Slug, p.Name, p.Path, p.Description, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if id > 0 {
		p.ID = id
	} else {
		return s.db.QueryRow(`SELECT id FROM projects WHERE slug=?`, p.Slug).Scan(&p.ID)
	}
	return nil
}

func (s *Store) GetProject(id int64) (*Project, error) {
	p := &Project{}
	err := s.db.QueryRow(`SELECT id, slug, name, path, description, created_at, updated_at FROM projects WHERE id=?`, id).Scan(
		&p.ID, &p.Slug, &p.Name, &p.Path, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) GetProjectBySlug(slug string) (*Project, error) {
	p := &Project{}
	err := s.db.QueryRow(`SELECT id, slug, name, path, description, created_at, updated_at FROM projects WHERE slug=?`, slug).Scan(
		&p.ID, &p.Slug, &p.Name, &p.Path, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query("SELECT id, slug, name, path, description, created_at, updated_at FROM projects ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Path, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec("DELETE FROM projects WHERE id=?", id)
	return err
}

// ============================================
// PROJECT NATURE
// ============================================

type ProjectNature struct {
	ProjectID int64
	NatureID  int64
	Priority  int
}

func (s *Store) UpsertProjectNature(pn *ProjectNature) error {
	_, err := s.db.Exec(`
		INSERT INTO project_nature (project_id, nature_id, priority)
		VALUES (?, ?, ?)
		ON CONFLICT(project_id, nature_id) DO UPDATE SET priority=excluded.priority
	`, pn.ProjectID, pn.NatureID, pn.Priority)
	return err
}

func (s *Store) ListProjectNature(projectID int64) ([]ProjectNature, error) {
	rows, err := s.db.Query(`SELECT project_id, nature_id, priority FROM project_nature WHERE project_id=? ORDER BY priority`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectNature
	for rows.Next() {
		var pn ProjectNature
		if err := rows.Scan(&pn.ProjectID, &pn.NatureID, &pn.Priority); err != nil {
			return nil, err
		}
		out = append(out, pn)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProjectNature(projectID, natureID int64) error {
	_, err := s.db.Exec("DELETE FROM project_nature WHERE project_id=? AND nature_id=?", projectID, natureID)
	return err
}

// ============================================
// HELPERS
// ============================================

// ============================================
// AGENT STATUS
// ============================================

// AgentStatus represents the runtime status of an agent on an harness.
type AgentStatus struct {
	AgentSlug    string  `json:"agent_slug"`
	DisplayName  string  `json:"display_name"`
	Emoji        string  `json:"emoji"`
	Harness string  `json:"harness"`
	Status       string  `json:"status"`
	Task         *string `json:"task,omitempty"`
	SessionID    *string `json:"session_id,omitempty"`
	StartedAt    *int64  `json:"started_at,omitempty"`
	UpdatedAt    int64   `json:"updated_at"`
}

// SetStatus upserts an agent status row. Sets started_at when transitioning to "working".
func (s *Store) SetStatus(agentSlug, harnessID, status, task, sessionID string) error {
	agent, err := s.ResolveAgentName(agentSlug)
	if err != nil {
		return err
	}
	n := now()
	var taskPtr, sessionPtr *string
	if task != "" {
		taskPtr = &task
	}
	if sessionID != "" {
		sessionPtr = &sessionID
	}
	_, err = s.db.Exec(`
		INSERT INTO agent_status (agent_id, harness_id, status, task, session_id, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, harness_id) DO UPDATE SET
			status=excluded.status, task=excluded.task, session_id=excluded.session_id,
			started_at=CASE WHEN agent_status.status != 'working' AND excluded.status = 'working' THEN excluded.updated_at ELSE agent_status.started_at END,
			updated_at=excluded.updated_at
	`, agent.ID, harnessID, status, taskPtr, sessionPtr, n, n)
	return err
}

// ClearStatus sets an agent's status to idle and clears task/session.
func (s *Store) ClearStatus(agentSlug, harnessID string) error {
	agent, err := s.ResolveAgentName(agentSlug)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		UPDATE agent_status SET status='idle', task=NULL, session_id=NULL, updated_at=?
		WHERE agent_id=? AND harness_id=?
	`, now(), agent.ID, harnessID)
	return err
}

// GetStatus returns the status of an agent across all harnesses.
func (s *Store) GetStatus(agentSlug string) ([]AgentStatus, error) {
	agent, err := s.GetAgentBySlug(agentSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT a.slug, a.display_name, COALESCE(a.emoji,''), s.harness_id, s.status, s.task, s.session_id, s.started_at, s.updated_at
		FROM agent_status s JOIN agents a ON a.id = s.agent_id
		WHERE s.agent_id=?`, agent.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentStatus
	for rows.Next() {
		var st AgentStatus
		if err := rows.Scan(&st.AgentSlug, &st.DisplayName, &st.Emoji, &st.Harness, &st.Status, &st.Task, &st.SessionID, &st.StartedAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListStatuses returns all agent statuses.
func (s *Store) ListStatuses() ([]AgentStatus, error) {
	rows, err := s.db.Query(`
		SELECT a.slug, a.display_name, COALESCE(a.emoji,''), s.harness_id, s.status, s.task, s.session_id, s.started_at, s.updated_at
		FROM agent_status s JOIN agents a ON a.id = s.agent_id
		ORDER BY a.slug, s.harness_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentStatus
	for rows.Next() {
		var st AgentStatus
		if err := rows.Scan(&st.AgentSlug, &st.DisplayName, &st.Emoji, &st.Harness, &st.Status, &st.Task, &st.SessionID, &st.StartedAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ResolveHarnessAgent finds the canonical agent slug given an harness-specific agent name.
func (s *Store) ResolveHarnessAgent(harnessID, harnessAgentName string) (string, error) {
	var slug string
	err := s.db.QueryRow(`
		SELECT a.slug FROM agents a
		JOIN agent_harness ao ON a.id = ao.agent_id
		WHERE ao.harness_id=? AND ao.harness_agent_id=?
	`, harnessID, harnessAgentName).Scan(&slug)
	if err != nil {
		return "", fmt.Errorf("no agent found for harness=%s agent=%s", harnessID, harnessAgentName)
	}
	return slug, nil
}

// Int64Ptr is a convenience for creating *int64 values.
func Int64Ptr(v int64) *int64 { return &v }

// ParseSourceNatureIDs parses the JSON array stored in file_distributions.source_nature_ids.
func ParseSourceNatureIDs(jsonStr string) []int64 {
	if jsonStr == "" {
		return nil
	}
	// Try JSON array first
	var ids []int64
	if err := json.Unmarshal([]byte(jsonStr), &ids); err == nil {
		return ids
	}
	// Try comma-separated as fallback
	var out []int64
	for _, s := range strings.Split(jsonStr, ",") {
		s = strings.TrimSpace(s)
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}
