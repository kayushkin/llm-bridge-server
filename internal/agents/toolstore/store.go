// Package toolstore is the canonical registry for tools that can be seeded
// into harnesses (Claude Code, openclaw, jig, codex, inber, etc.) via
// llm-bridge-server. It supports four kinds of tools: external MCP servers,
// arbitrary CLI commands, Go functions registered into the tool-store binary
// at build time, and the built-in tools of agent harnesses, which the harness
// runs and tool-store only records.
package toolstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

// DefaultDataDir is ~/.config/tool-store.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "tool-store")
}

// DefaultDBPath is ~/.config/tool-store/tool-store.db.
func DefaultDBPath() string { return filepath.Join(DefaultDataDir(), "tool-store.db") }

type Store struct {
	db      *sql.DB
	dataDir string
}

// Open opens or creates the store rooted at dataDir. Pass "" for the default.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "tool-store.db")
	// _txlock=immediate takes the write lock when a transaction begins, and
	// _busy_timeout makes a second writer wait for it rather than fail with
	// "database is locked": two sessions reporting the same new harness tool at
	// once must both succeed (RecordObservedHarnessTools).
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=10000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if err := migrateToolsTableToAcceptHarnessTools(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("rebuild tools table for harness tools: %w", err)
	}
	if err := addLastSeenAtColumnToTools(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

func (s *Store) Close() error    { return s.db.Close() }
func (s *Store) DB() *sql.DB     { return s.db }
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

// validate checks that t has the fields required for its Kind. Returns a
// concrete error rather than silently fixing things up.
func validate(t *Tool) error {
	if t.Name == "" {
		return errors.New("tool: name is required")
	}
	if !t.Kind.Valid() {
		return fmt.Errorf("tool %s: kind %q must be one of %s", t.Name, t.Kind, strings.Join(KindNames(), ", "))
	}
	if t.Kind != KindHarness && (t.Harness != "" || t.HarnessToolName != "") {
		return fmt.Errorf("tool %s: harness and harness_tool_name are only for kind=harness, not kind=%s", t.Name, t.Kind)
	}
	switch t.Kind {
	case KindMCP:
		if t.MCP == nil {
			return fmt.Errorf("tool %s: mcp spec is required for kind=mcp", t.Name)
		}
		switch t.MCP.Transport {
		case "stdio":
			if t.MCP.Command == "" {
				return fmt.Errorf("tool %s: mcp.command is required for stdio transport", t.Name)
			}
		case "http", "sse":
			if t.MCP.URL == "" {
				return fmt.Errorf("tool %s: mcp.url is required for %s transport", t.Name, t.MCP.Transport)
			}
		default:
			return fmt.Errorf("tool %s: mcp.transport %q must be stdio, http, or sse", t.Name, t.MCP.Transport)
		}
	case KindCLI:
		if t.CLI == nil || t.CLI.Command == "" {
			return fmt.Errorf("tool %s: cli.command is required for kind=cli", t.Name)
		}
	case KindLocal:
		if t.Local == nil || t.Local.Symbol == "" {
			return fmt.Errorf("tool %s: local.symbol is required for kind=local", t.Name)
		}
	case KindHarness:
		if t.Harness == "" || t.HarnessToolName == "" {
			return fmt.Errorf("tool %s: harness and harness_tool_name are required for kind=harness", t.Name)
		}
		if want := HarnessToolRowName(t.Harness, t.HarnessToolName); t.Name != want {
			return fmt.Errorf("tool %s: a kind=harness tool must be named %q (<harness>.<harness_tool_name>)", t.Name, want)
		}
		if t.MCP != nil || t.CLI != nil || t.Local != nil {
			return fmt.Errorf("tool %s: a kind=harness tool carries no mcp, cli or local spec; the harness runs it", t.Name)
		}
	}
	return nil
}

// UpsertTool inserts or updates by name. Sets ID on insert. It never writes
// last_seen_at, which only RecordObservedHarnessTools sets; t.LastSeenAt is
// ignored.
func (s *Store) UpsertTool(t *Tool) (bool, error) {
	if err := validate(t); err != nil {
		return false, err
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = now()
	}
	t.UpdatedAt = now()

	envKeys := encodeStrings(t.EnvKeys)
	tags := encodeStrings(t.Tags)
	credentials := encodeStringMap(t.Credentials)
	inputSchema := string(t.InputSchema)

	var (
		mcpTransport, mcpCommand, mcpArgs, mcpURL string
		cliCommand, cliArgsTemplate, cliWorkDir   string
		cliTimeoutMs                              int
		localSymbol                               string
	)
	if t.MCP != nil {
		mcpTransport = t.MCP.Transport
		mcpCommand = t.MCP.Command
		mcpArgs = encodeStrings(t.MCP.Args)
		mcpURL = t.MCP.URL
	}
	if t.CLI != nil {
		cliCommand = t.CLI.Command
		cliArgsTemplate = encodeStrings(t.CLI.ArgsTemplate)
		cliWorkDir = t.CLI.WorkingDir
		cliTimeoutMs = t.CLI.TimeoutMs
	}
	if t.Local != nil {
		localSymbol = t.Local.Symbol
	}

	res, err := s.db.Exec(`
		UPDATE tools SET
			display_name=?, description=?, kind=?, input_schema=?, env_keys=?, credentials=?, tags=?,
			mcp_transport=?, mcp_command=?, mcp_args=?, mcp_url=?,
			cli_command=?, cli_args_template=?, cli_working_dir=?, cli_timeout_ms=?,
			local_symbol=?, harness=?, harness_tool_name=?, enabled=?, updated_at=?
		WHERE name=?
	`, t.DisplayName, t.Description, string(t.Kind), inputSchema, envKeys, credentials, tags,
		mcpTransport, mcpCommand, mcpArgs, mcpURL,
		cliCommand, cliArgsTemplate, cliWorkDir, cliTimeoutMs,
		localSymbol, t.Harness, t.HarnessToolName, boolToInt(t.Enabled), t.UpdatedAt, t.Name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, s.db.QueryRow(`SELECT id FROM tools WHERE name=?`, t.Name).Scan(&t.ID)
	}

	res, err = s.db.Exec(`
		INSERT INTO tools (
			name, display_name, description, kind, input_schema, env_keys, credentials, tags,
			mcp_transport, mcp_command, mcp_args, mcp_url,
			cli_command, cli_args_template, cli_working_dir, cli_timeout_ms,
			local_symbol, harness, harness_tool_name, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.Name, t.DisplayName, t.Description, string(t.Kind), inputSchema, envKeys, credentials, tags,
		mcpTransport, mcpCommand, mcpArgs, mcpURL,
		cliCommand, cliArgsTemplate, cliWorkDir, cliTimeoutMs,
		localSymbol, t.Harness, t.HarnessToolName, boolToInt(t.Enabled), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return false, err
	}
	t.ID, _ = res.LastInsertId()
	return true, nil
}

// toolCols qualifies every column with the table alias "tools" so the same
// SELECT list is unambiguous in plain queries (FROM tools) and in JOINs
// against tables that share column names (e.g. instance_tools.created_at).
const toolCols = `tools.id, tools.name, tools.display_name, tools.description, tools.kind,
	tools.input_schema, tools.env_keys, tools.credentials, tools.tags,
	tools.mcp_transport, tools.mcp_command, tools.mcp_args, tools.mcp_url,
	tools.cli_command, tools.cli_args_template, tools.cli_working_dir, tools.cli_timeout_ms,
	tools.local_symbol, tools.harness, tools.harness_tool_name, tools.last_seen_at,
	tools.enabled, tools.created_at, tools.updated_at`

func scanTool(row interface{ Scan(...any) error }) (*Tool, error) {
	t := &Tool{}
	var (
		kind                                      string
		inputSchema, envKeys, credentials, tags   string
		mcpTransport, mcpCommand, mcpArgs, mcpURL string
		cliCommand, cliArgsTemplate, cliWorkDir   string
		cliTimeoutMs                              int
		localSymbol                               string
		enabled                                   int
	)
	err := row.Scan(
		&t.ID, &t.Name, &t.DisplayName, &t.Description, &kind, &inputSchema, &envKeys, &credentials, &tags,
		&mcpTransport, &mcpCommand, &mcpArgs, &mcpURL,
		&cliCommand, &cliArgsTemplate, &cliWorkDir, &cliTimeoutMs,
		&localSymbol, &t.Harness, &t.HarnessToolName, &t.LastSeenAt, &enabled, &t.CreatedAt, &t.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Kind = Kind(kind)
	if inputSchema != "" {
		t.InputSchema = json.RawMessage(inputSchema)
	}
	t.EnvKeys = decodeStrings(envKeys)
	t.Credentials = decodeStringMap(credentials)
	t.Tags = decodeStrings(tags)
	t.Enabled = enabled == 1

	switch t.Kind {
	case KindMCP:
		t.MCP = &MCPSpec{
			Transport: mcpTransport,
			Command:   mcpCommand,
			Args:      decodeStrings(mcpArgs),
			URL:       mcpURL,
		}
	case KindCLI:
		t.CLI = &CLISpec{
			Command:      cliCommand,
			ArgsTemplate: decodeStrings(cliArgsTemplate),
			WorkingDir:   cliWorkDir,
			TimeoutMs:    cliTimeoutMs,
		}
	case KindLocal:
		t.Local = &LocalSpec{Symbol: localSymbol}
	}
	return t, nil
}

func (s *Store) GetTool(id int64) (*Tool, error) {
	return scanTool(s.db.QueryRow(`SELECT `+toolCols+` FROM tools WHERE id=?`, id))
}

func (s *Store) GetToolByName(name string) (*Tool, error) {
	return scanTool(s.db.QueryRow(`SELECT `+toolCols+` FROM tools WHERE name=?`, name))
}

// ListFilter narrows ListTools.
type ListFilter struct {
	Kind        Kind
	Harness     string // exact match on the harness id; harness ids are llm-bridge-server's
	EnabledOnly bool
	Tag         string // matches if tags JSON array contains this string
	Query       string // substring match on name/description
	// NotSeenSince, when set, keeps only kind=harness rows whose last_seen_at
	// is before it (unix seconds), including rows never reported (0). Nil
	// means no filter.
	NotSeenSince *int64
	Limit        int
}

// ErrNotSeenSinceNeedsHarnessKind refuses a not_seen_since filter without
// kind=harness: only harness tools are ever reported seen, so every other row
// would match.
var ErrNotSeenSinceNeedsHarnessKind = errors.New("not_seen_since filters kind=harness tools only; add kind=harness")

func (s *Store) ListTools(f ListFilter) ([]Tool, error) {
	q := `SELECT ` + toolCols + ` FROM tools WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(f.Kind))
	}
	if f.Harness != "" {
		q += ` AND harness = ?`
		args = append(args, f.Harness)
	}
	if f.EnabledOnly {
		q += ` AND enabled = 1`
	}
	if f.Tag != "" {
		// Cheap LIKE match on the JSON array; entries are quoted strings.
		q += ` AND tags LIKE ?`
		args = append(args, `%"`+f.Tag+`"%`)
	}
	if f.NotSeenSince != nil {
		if f.Kind != KindHarness {
			return nil, ErrNotSeenSinceNeedsHarnessKind
		}
		q += ` AND last_seen_at < ?`
		args = append(args, *f.NotSeenSince)
	}
	if f.Query != "" {
		like := "%" + f.Query + "%"
		q += ` AND (name LIKE ? OR description LIKE ?)`
		args = append(args, like, like)
	}
	q += ` ORDER BY name`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTool(id int64) error {
	res, err := s.db.Exec(`DELETE FROM tools WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE tools SET enabled=?, updated_at=? WHERE id=?`,
		boolToInt(enabled), now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================================
// instance_tools (per-instance opt-in)
// ============================================

// ErrGloballyDisabled is returned when a caller tries to opt an instance into
// a tool whose global enabled flag is off. Global is master.
var ErrGloballyDisabled = errors.New("tool is globally disabled")

// EnableForInstance opts the given instance into the named tool. The tool
// must exist and be globally enabled — otherwise ErrNotFound or
// ErrGloballyDisabled is returned with no DB mutation.
func (s *Store) EnableForInstance(instanceID, toolName string) error {
	if instanceID == "" {
		return errors.New("instance_id is required")
	}
	t, err := s.GetToolByName(toolName)
	if err != nil {
		return err
	}
	if !t.Enabled {
		return ErrGloballyDisabled
	}
	_, err = s.db.Exec(`
		INSERT INTO instance_tools (instance_id, tool_id, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (instance_id, tool_id) DO NOTHING
	`, instanceID, t.ID, now())
	return err
}

// DisableForInstance removes the per-instance opt-in. Returns ErrNotFound if
// no such pairing exists. Idempotent at the DB level — repeated disables on
// an already-disabled pairing are detectable by the caller via the error.
func (s *Store) DisableForInstance(instanceID, toolName string) error {
	if instanceID == "" {
		return errors.New("instance_id is required")
	}
	t, err := s.GetToolByName(toolName)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`
		DELETE FROM instance_tools WHERE instance_id=? AND tool_id=?
	`, instanceID, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListInstanceTools returns every tool opted-in for this instance. Tools
// whose global enabled flag is currently off are filtered out — they're not
// available to provision regardless of the row's existence (global is
// master). Sort is alphabetical by name.
func (s *Store) ListInstanceTools(instanceID string) ([]Tool, error) {
	if instanceID == "" {
		return nil, errors.New("instance_id is required")
	}
	rows, err := s.db.Query(`
		SELECT `+toolCols+`
		FROM tools
		JOIN instance_tools ON instance_tools.tool_id = tools.id
		WHERE instance_tools.instance_id = ? AND tools.enabled = 1
		ORDER BY tools.name
	`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ListInstancesForTool returns every instance_id currently opted into the
// named tool. Useful for the management UI's "where is this enabled?" view.
func (s *Store) ListInstancesForTool(toolName string) ([]string, error) {
	t, err := s.GetToolByName(toolName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT instance_id FROM instance_tools WHERE tool_id = ? ORDER BY instance_id
	`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ============================================
// helpers
// ============================================

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodeStrings(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

func decodeStrings(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func encodeStringMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func decodeStringMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
