package toolstore

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// toolsTableBeforeHarnessKind is the tools table as schema.sql made it before
// the harness kind, without the credentials column that a later ALTER TABLE
// appended. The live database had exactly this plus that column.
const toolsTableBeforeHarnessKind = `CREATE TABLE tools (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT UNIQUE NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,                 -- 'mcp' | 'cli' | 'local'

    input_schema    TEXT NOT NULL DEFAULT '',      -- JSON object, optional for mcp
    env_keys        TEXT NOT NULL DEFAULT '',      -- JSON array of required env var names
    tags            TEXT NOT NULL DEFAULT '',      -- JSON array

    -- mcp
    mcp_transport   TEXT NOT NULL DEFAULT '',      -- 'stdio' | 'http' | 'sse'
    mcp_command     TEXT NOT NULL DEFAULT '',
    mcp_args        TEXT NOT NULL DEFAULT '',      -- JSON array
    mcp_url         TEXT NOT NULL DEFAULT '',

    -- cli
    cli_command       TEXT NOT NULL DEFAULT '',
    cli_args_template TEXT NOT NULL DEFAULT '',    -- JSON array; supports {{var}} substitution
    cli_working_dir   TEXT NOT NULL DEFAULT '',
    cli_timeout_ms    INTEGER NOT NULL DEFAULT 0,

    -- local
    local_symbol    TEXT NOT NULL DEFAULT '',      -- e.g. "agentkit/tools.Shell"

    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,

    CHECK (kind IN ('mcp', 'cli', 'local'))
);
`

const instanceToolsTableBeforeHarnessKind = `CREATE TABLE instance_tools (
    instance_id  TEXT    NOT NULL,
    tool_id      INTEGER NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (instance_id, tool_id)
);
CREATE INDEX idx_tools_kind    ON tools(kind);
CREATE INDEX idx_tools_enabled ON tools(enabled);`

// writeDatabaseBeforeHarnessKind makes a tool-store.db in dataDir the way an
// older binary left it: four tools with a gap in the ids, the highest issued
// id deleted (so the AUTOINCREMENT mark is above every row), and two
// instance opt-ins.
func writeDatabaseBeforeHarnessKind(t *testing.T, dataDir string, withCredentialsColumn bool) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "tool-store.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{toolsTableBeforeHarnessKind, instanceToolsTableBeforeHarnessKind}
	if withCredentialsColumn {
		statements = append(statements, `ALTER TABLE tools ADD COLUMN credentials TEXT NOT NULL DEFAULT ''`)
	}
	statements = append(statements,
		`INSERT INTO tools (id, name, description, kind, tags, mcp_transport, mcp_command, mcp_args, enabled, created_at, updated_at)
		 VALUES (3, 'playwright', 'browser', 'mcp', '["browser"]', 'stdio', 'npx', '["-y","@playwright/mcp"]', 1, 100, 200)`,
		`INSERT INTO tools (id, name, description, kind, cli_command, cli_args_template, cli_timeout_ms, enabled, created_at, updated_at)
		 VALUES (7, 'ls', 'list', 'cli', 'ls', '["-la","{{path}}"]', 5000, 0, 101, 201)`,
		`INSERT INTO tools (id, name, description, kind, local_symbol, enabled, created_at, updated_at)
		 VALUES (8, 'end_turn', 'end', 'local', 'end_turn', 1, 102, 202)`,
		`INSERT INTO tools (id, name, description, kind, local_symbol, enabled, created_at, updated_at)
		 VALUES (12, 'doomed', 'deleted', 'local', 'doomed', 1, 103, 203)`,
		`DELETE FROM tools WHERE id = 12`,
		`INSERT INTO instance_tools (instance_id, tool_id, created_at) VALUES ('inst-a', 3, 300), ('inst-a', 8, 301), ('inst-b', 7, 302)`,
	)
	if withCredentialsColumn {
		statements = append(statements, `UPDATE tools SET env_keys = '["K"]', credentials = '{"K":"prov"}' WHERE id = 3`)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The fixture must really refuse the new kind, or the test proves nothing.
	if _, err := db.Exec(`INSERT INTO tools (name, kind, created_at, updated_at) VALUES ('x', 'harness', 1, 1)`); err == nil {
		t.Fatal("the old-schema fixture accepted kind=harness")
	}
}

func dumpRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		var fields []string
		for i, column := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			fields = append(fields, column+"="+strings.TrimSpace(strings.ReplaceAll(fmtAny(values[i]), "\n", " ")))
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out
}

func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const oldToolColumns = `id, name, display_name, description, kind, input_schema, env_keys, tags,
	mcp_transport, mcp_command, mcp_args, mcp_url, cli_command, cli_args_template, cli_working_dir,
	cli_timeout_ms, local_symbol, enabled, created_at, updated_at`

func TestOpenRebuildsAToolsTableMadeBeforeTheHarnessKind(t *testing.T) {
	for _, withCredentialsColumn := range []bool{true, false} {
		t.Run(map[bool]string{true: "with credentials column", false: "before credentials column"}[withCredentialsColumn], func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "ts")
			writeDatabaseBeforeHarnessKind(t, dataDir, withCredentialsColumn)

			before, err := sql.Open("sqlite3", filepath.Join(dataDir, "tool-store.db"))
			if err != nil {
				t.Fatal(err)
			}
			toolsBefore := dumpRows(t, before, `SELECT `+oldToolColumns+` FROM tools ORDER BY id`)
			optInsBefore := dumpRows(t, before, `SELECT * FROM instance_tools ORDER BY instance_id, tool_id`)
			before.Close()

			s, err := Open(dataDir)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()

			if got := dumpRows(t, s.DB(), `SELECT `+oldToolColumns+` FROM tools ORDER BY id`); !reflect.DeepEqual(got, toolsBefore) {
				t.Fatalf("tools rows changed:\nbefore %v\nafter  %v", toolsBefore, got)
			}
			if got := dumpRows(t, s.DB(), `SELECT * FROM instance_tools ORDER BY instance_id, tool_id`); !reflect.DeepEqual(got, optInsBefore) {
				t.Fatalf("instance_tools rows changed:\nbefore %v\nafter  %v", optInsBefore, got)
			}
			playwright, err := s.GetToolByName("playwright")
			if err != nil {
				t.Fatal(err)
			}
			if withCredentialsColumn && playwright.Credentials["K"] != "prov" {
				t.Fatalf("credentials lost: %+v", playwright.Credentials)
			}
			if playwright.Harness != "" || playwright.HarnessToolName != "" {
				t.Fatalf("an old row gained harness fields: %+v", playwright)
			}

			// The rebuilt table takes the new kind, and ids keep counting from
			// above the deleted id 12.
			harnessTool := &Tool{Name: "claude_code.Read", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Read", Enabled: true}
			if _, err := s.UpsertTool(harnessTool); err != nil {
				t.Fatalf("insert harness tool into rebuilt table: %v", err)
			}
			if harnessTool.ID <= 12 {
				t.Fatalf("new id %d reuses an id at or below the deleted 12", harnessTool.ID)
			}

			// Foreign keys are on again and still cascade from the rebuilt table.
			var foreignKeys int
			if err := s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign_keys = %d, %v", foreignKeys, err)
			}
			if err := s.DeleteTool(3); err != nil {
				t.Fatal(err)
			}
			var remaining int
			s.DB().QueryRow(`SELECT count(*) FROM instance_tools WHERE tool_id = 3`).Scan(&remaining)
			if remaining != 0 {
				t.Fatalf("deleting tool 3 left %d opt-ins; the cascade is gone", remaining)
			}
			var indexCount int
			s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_tools_kind', 'idx_tools_enabled', 'idx_tools_harness')`).Scan(&indexCount)
			if indexCount != 3 {
				t.Fatalf("expected the three tools indexes, found %d", indexCount)
			}
		})
	}
}

func TestOpeningARebuiltDatabaseAgainChangesNothing(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "ts")
	writeDatabaseBeforeHarnessKind(t, dataDir, true)
	s, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var tableSQL string
	s.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'tools'`).Scan(&tableSQL)
	toolsAfterFirst := dumpRows(t, s.DB(), `SELECT * FROM tools ORDER BY id`)
	s.Close()

	s, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var tableSQLAgain string
	s.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'tools'`).Scan(&tableSQLAgain)
	if tableSQLAgain != tableSQL {
		t.Fatal("the second open rebuilt the table again")
	}
	if got := dumpRows(t, s.DB(), `SELECT * FROM tools ORDER BY id`); !reflect.DeepEqual(got, toolsAfterFirst) {
		t.Fatalf("second open changed rows")
	}
}

func TestTheSchemaKindCheckListsExactlyTheKinds(t *testing.T) {
	match := regexp.MustCompile(`CHECK \(kind IN \(([^)]*)\)\)`).FindStringSubmatch(schemaSQL)
	if match == nil {
		t.Fatal("schema.sql has no CHECK (kind IN (...))")
	}
	var inSchema []string
	for _, quoted := range strings.Split(match[1], ",") {
		inSchema = append(inSchema, strings.Trim(strings.TrimSpace(quoted), "'"))
	}
	inGo := KindNames()
	sort.Strings(inSchema)
	sort.Strings(inGo)
	if !reflect.DeepEqual(inSchema, inGo) {
		t.Fatalf("schema.sql CHECK lists %v, Kinds lists %v", inSchema, inGo)
	}
}

func TestTheDatabaseRefusesAHarnessRowThatBreaksTheNamingRule(t *testing.T) {
	s := openTest(t)
	for _, statement := range []string{
		`INSERT INTO tools (name, kind, harness, harness_tool_name, created_at, updated_at) VALUES ('Read', 'harness', 'claude_code', 'Read', 1, 1)`,
		`INSERT INTO tools (name, kind, harness, harness_tool_name, created_at, updated_at) VALUES ('claude_code.', 'harness', 'claude_code', '', 1, 1)`,
		`INSERT INTO tools (name, kind, local_symbol, harness, created_at, updated_at) VALUES ('x', 'local', 'x', 'codex', 1, 1)`,
	} {
		if _, err := s.DB().Exec(statement); err == nil {
			t.Errorf("the database accepted %s", statement)
		}
	}
}

func TestValidateHarnessTools(t *testing.T) {
	s := openTest(t)
	good := &Tool{Name: "codex.shell_tool", Kind: KindHarness, Harness: "codex", HarnessToolName: "shell_tool", Tags: []string{"effects"}, Enabled: true}
	if _, err := s.UpsertTool(good); err != nil {
		t.Fatalf("valid harness tool refused: %v", err)
	}
	got, err := s.GetToolByName("codex.shell_tool")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindHarness || got.Harness != "codex" || got.HarnessToolName != "shell_tool" || got.MCP != nil || got.CLI != nil || got.Local != nil {
		t.Fatalf("harness tool did not round-trip: %+v", got)
	}
	for _, bad := range []*Tool{
		{Name: "shell_tool", Kind: KindHarness, Harness: "codex", HarnessToolName: "shell_tool"},
		{Name: "codex.", Kind: KindHarness, Harness: "codex"},
		{Name: ".Read", Kind: KindHarness, HarnessToolName: "Read"},
		{Name: "codex.x", Kind: KindHarness, Harness: "codex", HarnessToolName: "x", Local: &LocalSpec{Symbol: "x"}},
		{Name: "y", Kind: KindLocal, Local: &LocalSpec{Symbol: "y"}, Harness: "codex"},
		{Name: "z", Kind: KindCLI, CLI: &CLISpec{Command: "z"}, HarnessToolName: "z"},
	} {
		if _, err := s.UpsertTool(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestListFiltersByHarness(t *testing.T) {
	s := openTest(t)
	for _, tool := range []*Tool{
		{Name: "claude_code.Read", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Read"},
		{Name: "claude_code.Bash", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Bash"},
		{Name: "codex.web_search", Kind: KindHarness, Harness: "codex", HarnessToolName: "web_search"},
		{Name: "web_search", Kind: KindLocal, Local: &LocalSpec{Symbol: "web_search"}},
	} {
		if _, err := s.UpsertTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	names := func(f ListFilter) []string {
		tools, err := s.ListTools(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return out
	}
	if got := names(ListFilter{Kind: KindHarness, Harness: "claude_code"}); !reflect.DeepEqual(got, []string{"claude_code.Bash", "claude_code.Read"}) {
		t.Fatalf("kind=harness&harness=claude_code: %v", got)
	}
	if got := names(ListFilter{Kind: KindHarness}); len(got) != 3 {
		t.Fatalf("kind=harness: %v", got)
	}
	if got := names(ListFilter{Harness: "gemini"}); got != nil {
		t.Fatalf("an unknown harness should match nothing, got %v", got)
	}
}

func TestHTTPKindsServesTheVocabulary(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/kinds")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []KindDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Kinds) {
		t.Fatalf("GET /kinds = %+v", got)
	}
	for _, kind := range got {
		if kind.Description == "" {
			t.Fatalf("kind %s has no description", kind.Kind)
		}
	}
}

func TestHTTPHarnessToolsAreListedButNotInvokedSpecifiedOrProvisioned(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{Name: "claude_code.Bash", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Bash", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTool(&Tool{Name: "codex.shell_tool", Kind: KindHarness, Harness: "codex", HarnessToolName: "shell_tool", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/tools?kind=harness&harness=claude_code")
	if err != nil {
		t.Fatal(err)
	}
	var listed []map[string]any
	json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed) != 1 || listed[0]["name"] != "claude_code.Bash" || listed[0]["harness"] != "claude_code" || listed[0]["harness_tool_name"] != "Bash" {
		t.Fatalf("GET /tools?kind=harness&harness=claude_code = %v", listed)
	}

	for _, name := range []string{"claude_code.Bash", "codex.shell_tool"} {
		for _, call := range []struct{ method, path string }{
			{"POST", "/tools/by-name/" + name + "/invoke"},
			{"GET", "/tools/by-name/" + name + "/spec"},
		} {
			req, _ := http.NewRequest(call.method, srv.URL+call.path, strings.NewReader(`{}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]string
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != 409 || !strings.Contains(body["error"], "harness") {
				t.Fatalf("%s %s: %d %v", call.method, call.path, resp.StatusCode, body)
			}
		}
	}

	resp, err = http.Post(srv.URL+"/provision", "application/json", strings.NewReader(`{"tools":["claude_code.Bash"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(body["error"], "claude_code harness") {
		t.Fatalf("provision of a harness tool: %d %v", resp.StatusCode, body)
	}
}
