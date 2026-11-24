package toolstore

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ts"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertGetMCP(t *testing.T) {
	s := openTest(t)
	in := &Tool{
		Name:        "brave-search",
		DisplayName: "Brave Search",
		Description: "Web search via Brave Search API.",
		Kind:        KindMCP,
		EnvKeys:     []string{"BRAVE_API_KEY"},
		Tags:        []string{"search", "web"},
		MCP: &MCPSpec{
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-brave-search"},
		},
		Enabled: true,
	}
	inserted, err := s.UpsertTool(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Fatal("expected insert on first upsert")
	}
	if in.ID == 0 {
		t.Fatal("ID not set after insert")
	}

	got, err := s.GetToolByName("brave-search")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MCP == nil || got.MCP.Command != "npx" {
		t.Fatalf("MCP spec not roundtripped: %+v", got.MCP)
	}
	if len(got.EnvKeys) != 1 || got.EnvKeys[0] != "BRAVE_API_KEY" {
		t.Fatalf("EnvKeys mismatch: %v", got.EnvKeys)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("Tags mismatch: %v", got.Tags)
	}

	// Update path
	in.Description = "updated"
	inserted, err = s.UpsertTool(in)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if inserted {
		t.Fatal("expected update on second upsert")
	}
	got, _ = s.GetToolByName("brave-search")
	if got.Description != "updated" {
		t.Fatalf("update did not stick: %q", got.Description)
	}
}

func TestUpsertCLIWithSchema(t *testing.T) {
	s := openTest(t)
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	in := &Tool{
		Name:        "ls",
		Description: "list files",
		Kind:        KindCLI,
		InputSchema: schema,
		CLI: &CLISpec{
			Command:      "ls",
			ArgsTemplate: []string{"-la", "{{path}}"},
			TimeoutMs:    5000,
		},
		Enabled: true,
	}
	if _, err := s.UpsertTool(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetTool(in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CLI == nil || got.CLI.Command != "ls" || got.CLI.TimeoutMs != 5000 {
		t.Fatalf("CLI spec mismatch: %+v", got.CLI)
	}
	if string(got.InputSchema) != string(schema) {
		t.Fatalf("InputSchema mismatch:\nwant %s\n got %s", schema, got.InputSchema)
	}
}

func TestUpsertLocal(t *testing.T) {
	s := openTest(t)
	in := &Tool{
		Name:        "shell",
		Description: "Run bash commands.",
		Kind:        KindLocal,
		Local:       &LocalSpec{Symbol: "agentkit/tools.Shell"},
		Enabled:     true,
	}
	if _, err := s.UpsertTool(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetToolByName("shell")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Local == nil || got.Local.Symbol != "agentkit/tools.Shell" {
		t.Fatalf("Local spec mismatch: %+v", got.Local)
	}
}

// The kind check is validate's, not the database's. schema.sql also carries
// CHECK (kind IN ('mcp', 'cli', 'local')), so a test that asserts only "the
// upsert failed" stays green with validate's own check deleted — the driver
// answers instead, several layers later and with a constraint error the caller
// cannot act on. Naming validate's wording is what tells the two apart.
func TestValidationRejectsBadKind(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertTool(&Tool{Name: "x", Kind: "bogus"})
	if err == nil {
		t.Fatal("expected validation error for unknown kind")
	}
	want := `kind "bogus" must be one of mcp, cli, local`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want validate's own refusal naming %q, got %q", want, err.Error())
	}
}

// Each row of this table is a separate producer of validate's error. Asserting
// only that an error came back cannot tell them apart, so each case names the
// field its own refusal is about.
func TestValidationRejectsMissingFields(t *testing.T) {
	s := openTest(t)
	cases := []struct {
		desc string
		t    *Tool
		want string
	}{
		{"mcp without spec", &Tool{Name: "a", Kind: KindMCP}, "mcp spec is required for kind=mcp"},
		{"mcp stdio without command", &Tool{Name: "b", Kind: KindMCP, MCP: &MCPSpec{Transport: "stdio"}}, "mcp.command is required for stdio transport"},
		{"mcp http without url", &Tool{Name: "c", Kind: KindMCP, MCP: &MCPSpec{Transport: "http"}}, "mcp.url is required for http transport"},
		{"mcp with an unknown transport", &Tool{Name: "f", Kind: KindMCP, MCP: &MCPSpec{Transport: "grpc", Command: "x"}}, `mcp.transport "grpc" must be stdio, http, or sse`},
		{"cli without command", &Tool{Name: "d", Kind: KindCLI, CLI: &CLISpec{}}, "cli.command is required for kind=cli"},
		{"local without symbol", &Tool{Name: "e", Kind: KindLocal, Local: &LocalSpec{}}, "local.symbol is required for kind=local"},
		{"missing name", &Tool{Kind: KindLocal, Local: &LocalSpec{Symbol: "x"}}, "tool: name is required"},
	}
	for _, c := range cases {
		_, err := s.UpsertTool(c.t)
		if err == nil {
			t.Errorf("%s: expected validation error", c.desc)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error naming %q, got %q", c.desc, c.want, err.Error())
		}
	}
}

func TestListFilters(t *testing.T) {
	s := openTest(t)
	must := func(t *testing.T, in *Tool) {
		t.Helper()
		if _, err := s.UpsertTool(in); err != nil {
			t.Fatalf("upsert %s: %v", in.Name, err)
		}
	}
	must(t, &Tool{Name: "shell", Kind: KindLocal, Local: &LocalSpec{Symbol: "x.Shell"}, Tags: []string{"core"}, Enabled: true})
	must(t, &Tool{Name: "grep", Kind: KindLocal, Local: &LocalSpec{Symbol: "x.Grep"}, Tags: []string{"core", "search"}, Enabled: true})
	must(t, &Tool{Name: "brave-search", Kind: KindMCP, MCP: &MCPSpec{Transport: "stdio", Command: "npx"}, Tags: []string{"search"}, Enabled: false})

	all, err := s.ListTools(ListFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListTools all: %v len=%d", err, len(all))
	}

	enabled, _ := s.ListTools(ListFilter{EnabledOnly: true})
	if len(enabled) != 2 {
		t.Fatalf("EnabledOnly: want 2 got %d", len(enabled))
	}

	mcp, _ := s.ListTools(ListFilter{Kind: KindMCP})
	if len(mcp) != 1 || mcp[0].Name != "brave-search" {
		t.Fatalf("Kind=mcp: %+v", mcp)
	}

	tagged, _ := s.ListTools(ListFilter{Tag: "search"})
	if len(tagged) != 2 {
		t.Fatalf("Tag=search: want 2 got %d", len(tagged))
	}

	q, _ := s.ListTools(ListFilter{Query: "brave"})
	if len(q) != 1 {
		t.Fatalf("Query=brave: want 1 got %d", len(q))
	}
}

func TestDeleteAndSetEnabled(t *testing.T) {
	s := openTest(t)
	in := &Tool{Name: "shell", Kind: KindLocal, Local: &LocalSpec{Symbol: "x.Shell"}, Enabled: true}
	if _, err := s.UpsertTool(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetEnabled(in.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := s.GetTool(in.ID)
	if got.Enabled {
		t.Fatal("expected disabled")
	}
	if err := s.DeleteTool(in.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetTool(in.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := s.DeleteTool(in.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
}
