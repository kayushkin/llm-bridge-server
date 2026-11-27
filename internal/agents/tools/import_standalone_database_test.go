package tools

import (
	"database/sql"
	_ "embed"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// writeStandaloneDatabase makes a SQLite file the way the standalone store
// made one, runs statements in it, and returns its path.
func writeStandaloneDatabase(t *testing.T, schema string, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool-store.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range append([]string{schema}, statements...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

var standaloneRows = []string{
	`INSERT INTO tools (id, name, display_name, description, kind, env_keys, credentials, tags, mcp_transport, mcp_command, mcp_args, enabled, created_at, updated_at)
	 VALUES (1, 'brave-search', 'Brave Search', 'Web search.', 'mcp', '["BRAVE_API_KEY"]', '{"BRAVE_API_KEY":"brave"}', '["search","web"]', 'stdio', 'npx', '["-y","server-brave-search"]', 1, 100, 200)`,
	`INSERT INTO tools (id, name, description, kind, input_schema, local_symbol, enabled, created_at, updated_at)
	 VALUES (2, 'shell_commands', 'Run a command.', 'local', '{"type":"object"}', 'shell_commands', 0, 101, 201)`,
	`INSERT INTO tools (id, name, description, kind, cli_command, cli_args_template, cli_working_dir, cli_timeout_ms, enabled, created_at, updated_at)
	 VALUES (4, 'echo', 'Echo.', 'cli', 'echo', '["{{text}}"]', '/tmp', 500, 1, 102, 202)`,
	`INSERT INTO tools (id, name, description, kind, tags, harness, harness_tool_name, last_seen_at, enabled, created_at, updated_at)
	 VALUES (5, 'claude_code.Bash', 'Run a shell command.', 'harness', '["effects","runs-commands"]', 'claude_code', 'Bash', 300, 1, 103, 203)`,
	// Tool 6 was made and deleted, so AUTOINCREMENT remembers 6 and never
	// hands it out again.
	`INSERT INTO tools (id, name, kind, local_symbol, created_at, updated_at) VALUES (6, 'gone', 'local', 'gone', 1, 1)`,
	`DELETE FROM tools WHERE id = 6`,
	`INSERT INTO instance_tools (instance_id, tool_id, created_at) VALUES ('inst-b', 1, 400), ('inst-a', 1, 401), ('inst-a', 4, 402)`,
}

func TestImportCopiesEveryToolAndOptInKeepingIDs(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema, standaloneRows...)
	s := newTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, path)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImportCounts{Tools: 4, InstanceTools: 3, LastToolID: 6}); counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	brave, err := s.GetTool(testContext, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantBrave := &Tool{ID: 1, Name: "brave-search", DisplayName: "Brave Search", Description: "Web search.", Kind: KindMCP,
		EnvKeys: []string{"BRAVE_API_KEY"}, Credentials: map[string]string{"BRAVE_API_KEY": "brave"}, Tags: []string{"search", "web"},
		MCP: &MCPSpec{Transport: "stdio", Command: "npx", Args: []string{"-y", "server-brave-search"}}, Enabled: true, CreatedAt: 100, UpdatedAt: 200}
	if !reflect.DeepEqual(brave, wantBrave) {
		t.Fatalf("tool 1 = %+v, want %+v", brave, wantBrave)
	}
	echo, err := s.GetTool(testContext, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(echo.CLI, &CLISpec{Command: "echo", ArgsTemplate: []string{"{{text}}"}, WorkingDir: "/tmp", TimeoutMs: 500}) || !echo.Enabled {
		t.Fatalf("tool 4 = %+v %+v", echo, echo.CLI)
	}
	bash, err := s.GetTool(testContext, 5)
	if err != nil {
		t.Fatal(err)
	}
	if bash.Harness != "claude_code" || bash.HarnessToolName != "Bash" || bash.LastSeenAt != 300 || !reflect.DeepEqual(bash.Tags, []string{"effects", "runs-commands"}) {
		t.Fatalf("tool 5 = %+v", bash)
	}
	shell, err := s.GetTool(testContext, 2)
	if err != nil {
		t.Fatal(err)
	}
	if shell.Enabled || string(shell.InputSchema) != `{"type":"object"}` {
		t.Fatalf("tool 2 = %+v", shell)
	}

	instances, err := s.ListInstancesForTool(testContext, "brave-search")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(instances, []string{"inst-a", "inst-b"}) {
		t.Fatalf("instances of brave-search = %v", instances)
	}
	optedIn, err := s.ListInstanceTools(testContext, "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(optedIn) != 2 || optedIn[0].ID != 1 || optedIn[1].ID != 4 {
		t.Fatalf("inst-a opt-ins = %+v", optedIn)
	}

	// The next tool takes the number after the last one the standalone store
	// ever handed out, not after the last one left.
	next := &Tool{Name: "new", Kind: KindLocal, Local: &LocalSpec{Symbol: "new"}}
	if _, err := s.UpsertTool(testContext, next); err != nil {
		t.Fatal(err)
	}
	if next.ID != 7 {
		t.Fatalf("the next tool got id %d, want 7", next.ID)
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema, standaloneRows...)
	s := newTestStore(t)
	if _, err := s.UpsertTool(testContext, &Tool{Name: "here", Kind: KindLocal, Local: &LocalSpec{Symbol: "here"}}); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(testContext, s.db, path)
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("import into a module with rows: %v", err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	for name, file := range map[string]struct {
		schema     string
		statements []string
		wantWords  string
	}{
		"a column it does not know":    {standaloneSchema, []string{`ALTER TABLE tools ADD COLUMN surprise TEXT NOT NULL DEFAULT ''`}, "surprise"},
		"made before the harness kind": {schemaWithoutHarnessColumns, nil, "before the harness kind"},
		"made before last_seen_at":     {standaloneSchema, []string{`ALTER TABLE tools DROP COLUMN last_seen_at`}, "before last_seen_at"},
		"an opt-in naming a tool the file does not hold": {standaloneSchema, []string{
			`PRAGMA foreign_keys = OFF`,
			`INSERT INTO instance_tools (instance_id, tool_id, created_at) VALUES ('inst-a', 99, 1)`,
		}, "tool 99"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeStandaloneDatabase(t, file.schema, file.statements...)
			s := newTestStore(t)
			_, err := ImportStandaloneDatabase(testContext, s.db, path)
			if err == nil || !strings.Contains(err.Error(), file.wantWords) {
				t.Fatalf("import: %v, want an error naming %q", err, file.wantWords)
			}
			tools, listErr := s.ListTools(testContext, ListFilter{})
			if listErr != nil || len(tools) != 0 {
				t.Fatalf("a refused import left %d tools (%v)", len(tools), listErr)
			}
		})
	}
}

// schemaWithoutHarnessColumns is the tools table as it was before the harness
// kind: no harness, harness_tool_name or last_seen_at, and no CHECK on them.
const schemaWithoutHarnessColumns = `CREATE TABLE tools (
    id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
    input_schema TEXT NOT NULL DEFAULT '', env_keys TEXT NOT NULL DEFAULT '', credentials TEXT NOT NULL DEFAULT '', tags TEXT NOT NULL DEFAULT '',
    mcp_transport TEXT NOT NULL DEFAULT '', mcp_command TEXT NOT NULL DEFAULT '', mcp_args TEXT NOT NULL DEFAULT '', mcp_url TEXT NOT NULL DEFAULT '',
    cli_command TEXT NOT NULL DEFAULT '', cli_args_template TEXT NOT NULL DEFAULT '', cli_working_dir TEXT NOT NULL DEFAULT '', cli_timeout_ms INTEGER NOT NULL DEFAULT 0,
    local_symbol TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
    CHECK (kind IN ('mcp', 'cli', 'local')));
CREATE TABLE instance_tools (instance_id TEXT NOT NULL, tool_id INTEGER NOT NULL REFERENCES tools(id) ON DELETE CASCADE, created_at INTEGER NOT NULL, PRIMARY KEY (instance_id, tool_id));`
