package tools

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestTheSchemaKindCheckListsExactlyTheKinds(t *testing.T) {
	migration, err := migrationFiles.ReadFile("migrations/0001_create_tools_and_instance_tools.sql")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`CHECK \(kind IN \(([^)]*)\)\)`).FindStringSubmatch(string(migration))
	if match == nil {
		t.Fatal("the migration has no CHECK (kind IN (...))")
	}
	var inSchema []string
	for _, quoted := range strings.Split(match[1], ",") {
		inSchema = append(inSchema, strings.Trim(strings.TrimSpace(quoted), "'"))
	}
	inGo := KindNames()
	sort.Strings(inSchema)
	sort.Strings(inGo)
	if !reflect.DeepEqual(inSchema, inGo) {
		t.Fatalf("the migration's CHECK lists %v, Kinds lists %v", inSchema, inGo)
	}
}

func TestTheDatabaseRefusesAHarnessRowThatBreaksTheNamingRule(t *testing.T) {
	s := openTest(t)
	for _, statement := range []string{
		`INSERT INTO tools (name, kind, harness, harness_tool_name, created_at, updated_at) VALUES ('Read', 'harness', 'claude_code', 'Read', 1, 1)`,
		`INSERT INTO tools (name, kind, harness, harness_tool_name, created_at, updated_at) VALUES ('claude_code.', 'harness', 'claude_code', '', 1, 1)`,
		`INSERT INTO tools (name, kind, local_symbol, harness, created_at, updated_at) VALUES ('x', 'local', 'x', 'codex', 1, 1)`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Errorf("the database accepted %s", statement)
		}
	}
}

func TestValidateHarnessTools(t *testing.T) {
	s := openTest(t)
	good := &Tool{Name: "codex.shell_tool", Kind: KindHarness, Harness: "codex", HarnessToolName: "shell_tool", Tags: []string{"effects"}, Enabled: true}
	if _, err := s.UpsertTool(testContext, good); err != nil {
		t.Fatalf("valid harness tool refused: %v", err)
	}
	got, err := s.GetToolByName(testContext, "codex.shell_tool")
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
		if _, err := s.UpsertTool(testContext, bad); err == nil {
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
		if _, err := s.UpsertTool(testContext, tool); err != nil {
			t.Fatal(err)
		}
	}
	names := func(f ListFilter) []string {
		tools, err := s.ListTools(testContext, f)
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
	if _, err := s.UpsertTool(testContext, &Tool{Name: "claude_code.Bash", Kind: KindHarness, Harness: "claude_code", HarnessToolName: "Bash", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTool(testContext, &Tool{Name: "codex.shell_tool", Kind: KindHarness, Harness: "codex", HarnessToolName: "shell_tool", Enabled: false}); err != nil {
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
