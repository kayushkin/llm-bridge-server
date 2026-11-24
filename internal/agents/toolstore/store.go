// Package toolstore is the canonical registry for tools that can be seeded
// into harnesses (Claude Code, openclaw, jig, codex, inber, etc.) via
// llm-bridge-server. It supports four kinds of tools: external MCP servers,
// arbitrary CLI commands, Go functions compiled into this server, and the
// built-in tools of agent harnesses, which the harness runs and tool-store
// only records.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New.
package toolstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var ErrNotFound = errors.New("not found")

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "tools", Role: "tools_module", Migrations: migrations}, nil
}

// Store is the registry, on the module's connection pool.
type Store struct {
	db *sql.DB
}

// New returns the store on db, the module's pool from postgresstorage.Open.
// The caller owns the pool and closes it.
func New(db *sql.DB) *Store { return &Store{db: db} }

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
func (s *Store) UpsertTool(ctx context.Context, t *Tool) (bool, error) {
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

	err := s.db.QueryRowContext(ctx, `
		UPDATE tools SET
			display_name=$1, description=$2, kind=$3, input_schema=$4, env_keys=$5, credentials=$6, tags=$7,
			mcp_transport=$8, mcp_command=$9, mcp_args=$10, mcp_url=$11,
			cli_command=$12, cli_args_template=$13, cli_working_dir=$14, cli_timeout_ms=$15,
			local_symbol=$16, harness=$17, harness_tool_name=$18, enabled=$19, updated_at=$20
		WHERE name=$21
		RETURNING id
	`, t.DisplayName, t.Description, string(t.Kind), inputSchema, envKeys, credentials, tags,
		mcpTransport, mcpCommand, mcpArgs, mcpURL,
		cliCommand, cliArgsTemplate, cliWorkDir, cliTimeoutMs,
		localSymbol, t.Harness, t.HarnessToolName, t.Enabled, t.UpdatedAt, t.Name).Scan(&t.ID)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO tools (
			name, display_name, description, kind, input_schema, env_keys, credentials, tags,
			mcp_transport, mcp_command, mcp_args, mcp_url,
			cli_command, cli_args_template, cli_working_dir, cli_timeout_ms,
			local_symbol, harness, harness_tool_name, enabled, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		RETURNING id
	`, t.Name, t.DisplayName, t.Description, string(t.Kind), inputSchema, envKeys, credentials, tags,
		mcpTransport, mcpCommand, mcpArgs, mcpURL,
		cliCommand, cliArgsTemplate, cliWorkDir, cliTimeoutMs,
		localSymbol, t.Harness, t.HarnessToolName, t.Enabled, t.CreatedAt, t.UpdatedAt).Scan(&t.ID)
	if err != nil {
		return false, err
	}
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
	)
	err := row.Scan(
		&t.ID, &t.Name, &t.DisplayName, &t.Description, &kind, &inputSchema, &envKeys, &credentials, &tags,
		&mcpTransport, &mcpCommand, &mcpArgs, &mcpURL,
		&cliCommand, &cliArgsTemplate, &cliWorkDir, &cliTimeoutMs,
		&localSymbol, &t.Harness, &t.HarnessToolName, &t.LastSeenAt, &t.Enabled, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
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

func (s *Store) GetTool(ctx context.Context, id int64) (*Tool, error) {
	return scanTool(s.db.QueryRowContext(ctx, `SELECT `+toolCols+` FROM tools WHERE id=$1`, id))
}

func (s *Store) GetToolByName(ctx context.Context, name string) (*Tool, error) {
	return scanTool(s.db.QueryRowContext(ctx, `SELECT `+toolCols+` FROM tools WHERE name=$1`, name))
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

// ListTools answers in name order by byte value (COLLATE "C"), the order the
// SQLite store kept, whatever collation the database was made with. Tag and
// Query match case-insensitively (ILIKE), as SQLite's LIKE did for ASCII.
func (s *Store) ListTools(ctx context.Context, f ListFilter) ([]Tool, error) {
	q := `SELECT ` + toolCols + ` FROM tools WHERE TRUE`
	var args []any
	next := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Kind != "" {
		q += ` AND kind = ` + next(string(f.Kind))
	}
	if f.Harness != "" {
		q += ` AND harness = ` + next(f.Harness)
	}
	if f.EnabledOnly {
		q += ` AND enabled`
	}
	if f.Tag != "" {
		// Cheap match on the JSON array; entries are quoted strings.
		q += ` AND tags ILIKE ` + next(`%"`+f.Tag+`"%`)
	}
	if f.NotSeenSince != nil {
		if f.Kind != KindHarness {
			return nil, ErrNotSeenSinceNeedsHarnessKind
		}
		q += ` AND last_seen_at < ` + next(*f.NotSeenSince)
	}
	if f.Query != "" {
		like := next("%" + f.Query + "%")
		q += ` AND (name ILIKE ` + like + ` OR description ILIKE ` + like + `)`
	}
	q += ` ORDER BY name COLLATE "C"`
	if f.Limit > 0 {
		q += ` LIMIT ` + next(f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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

func (s *Store) DeleteTool(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tools WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tools SET enabled=$1, updated_at=$2 WHERE id=$3`,
		enabled, now(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
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
func (s *Store) EnableForInstance(ctx context.Context, instanceID, toolName string) error {
	if instanceID == "" {
		return errors.New("instance_id is required")
	}
	t, err := s.GetToolByName(ctx, toolName)
	if err != nil {
		return err
	}
	if !t.Enabled {
		return ErrGloballyDisabled
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO instance_tools (instance_id, tool_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, tool_id) DO NOTHING
	`, instanceID, t.ID, now())
	return err
}

// DisableForInstance removes the per-instance opt-in. Returns ErrNotFound if
// no such pairing exists. Idempotent at the DB level — repeated disables on
// an already-disabled pairing are detectable by the caller via the error.
func (s *Store) DisableForInstance(ctx context.Context, instanceID, toolName string) error {
	if instanceID == "" {
		return errors.New("instance_id is required")
	}
	t, err := s.GetToolByName(ctx, toolName)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM instance_tools WHERE instance_id=$1 AND tool_id=$2
	`, instanceID, t.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListInstanceTools returns every tool opted-in for this instance. Tools
// whose global enabled flag is currently off are filtered out — they're not
// available to provision regardless of the row's existence (global is
// master). Sort is by name, by byte value.
func (s *Store) ListInstanceTools(ctx context.Context, instanceID string) ([]Tool, error) {
	if instanceID == "" {
		return nil, errors.New("instance_id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+toolCols+`
		FROM tools
		JOIN instance_tools ON instance_tools.tool_id = tools.id
		WHERE instance_tools.instance_id = $1 AND tools.enabled
		ORDER BY tools.name COLLATE "C"
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
func (s *Store) ListInstancesForTool(ctx context.Context, toolName string) ([]string, error) {
	t, err := s.GetToolByName(ctx, toolName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT instance_id FROM instance_tools WHERE tool_id = $1 ORDER BY instance_id COLLATE "C"
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
