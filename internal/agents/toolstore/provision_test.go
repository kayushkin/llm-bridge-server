package toolstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func openProvTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ts"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProvisionStdioWithCreds(t *testing.T) {
	s := openProvTest(t)
	if _, err := s.UpsertTool(&Tool{
		Name:        "brave-search",
		Kind:        KindMCP,
		EnvKeys:     []string{"BRAVE_API_KEY"},
		Credentials: map[string]string{"BRAVE_API_KEY": "brave"},
		MCP: &MCPSpec{
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"-y", "@modelcontextprotocol/server-brave-search"},
		},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resolve := func(_ context.Context, provider string) (string, error) {
		if provider != "brave" {
			t.Fatalf("unexpected provider %q", provider)
		}
		return "secret-key", nil
	}
	resp, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave-search"}}, resolve)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, ok := resp.MCPServers["brave-search"]
	if !ok {
		t.Fatal("missing brave-search in response")
	}
	if cfg.Command != "npx" || len(cfg.Args) != 2 {
		t.Fatalf("config mismatch: %+v", cfg)
	}
	if cfg.Env["BRAVE_API_KEY"] != "secret-key" {
		t.Fatalf("env mismatch: %v", cfg.Env)
	}
}

func TestProvisionMissingMapping(t *testing.T) {
	s := openProvTest(t)
	// EnvKeys present but Credentials map missing the key
	_, _ = s.UpsertTool(&Tool{
		Name:    "brave",
		Kind:    KindMCP,
		EnvKeys: []string{"BRAVE_API_KEY"},
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: true,
	})
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}}, func(context.Context, string) (string, error) {
		return "x", nil
	})
	if err == nil {
		t.Fatal("expected error for missing credentials mapping")
	}
}

func TestProvisionResolverError(t *testing.T) {
	s := openProvTest(t)
	_, _ = s.UpsertTool(&Tool{
		Name:        "brave",
		Kind:        KindMCP,
		EnvKeys:     []string{"BRAVE_API_KEY"},
		Credentials: map[string]string{"BRAVE_API_KEY": "brave"},
		MCP:         &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled:     true,
	})
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"brave"}}, func(context.Context, string) (string, error) {
		return "", errors.New("not found")
	})
	if err == nil {
		t.Fatal("expected error when resolver fails")
	}
}

func TestProvisionDisabled(t *testing.T) {
	s := openProvTest(t)
	_, _ = s.UpsertTool(&Tool{
		Name:    "x",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: false,
	})
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"x"}}, nil)
	if err == nil {
		t.Fatal("expected error for disabled tool")
	}
}

func TestProvisionRejectsNonMCP(t *testing.T) {
	s := openProvTest(t)
	_, _ = s.UpsertTool(&Tool{
		Name:    "shell",
		Kind:    KindLocal,
		Local:   &LocalSpec{Symbol: "shell"},
		Enabled: true,
	})
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"shell"}}, nil)
	if err == nil {
		t.Fatal("expected error for non-mcp tool")
	}
}

func TestProvisionHTTP(t *testing.T) {
	s := openProvTest(t)
	_, _ = s.UpsertTool(&Tool{
		Name:    "remote-mcp",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "http", URL: "https://example.com/mcp"},
		Enabled: true,
	})
	resp, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"remote-mcp"}}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg := resp.MCPServers["remote-mcp"]
	if cfg.URL != "https://example.com/mcp" || cfg.Type != "http" {
		t.Fatalf("config: %+v", cfg)
	}
	if cfg.Command != "" {
		t.Fatal("stdio command should be empty for http transport")
	}
}

func TestProvisionEmptyToolsRejected(t *testing.T) {
	s := openProvTest(t)
	_, err := Provision(context.Background(), s, ProvisionRequest{}, nil)
	if err == nil {
		t.Fatal("expected error for empty tools list")
	}
}

// --- provisioning by instance -------------------------------------------
//
// These pin the second half of the wire: the rows the Tools page writes to
// instance_tools now reach a provisioned session.

// seedInstanceOptIns writes one MCP tool, one CLI tool and one globally
// disabled MCP tool, and opts the given instance into the first two.
func seedInstanceOptIns(t *testing.T, s *Store, instanceID string) {
	t.Helper()
	if _, err := s.UpsertTool(&Tool{
		Name:    "remote-mcp",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "http", URL: "https://mcp.example.com"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert mcp: %v", err)
	}
	if _, err := s.UpsertTool(&Tool{
		Name:    "ripgrep",
		Kind:    KindCLI,
		CLI:     &CLISpec{Command: "rg"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert cli: %v", err)
	}
	for _, name := range []string{"remote-mcp", "ripgrep"} {
		if err := s.EnableForInstance(instanceID, name); err != nil {
			t.Fatalf("opt %s in: %v", name, err)
		}
	}
}

func TestProvisionByInstanceReadsTheOptInList(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")

	resp, err := Provision(context.Background(), s, ProvisionRequest{InstanceID: "inst-1"}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, ok := resp.MCPServers["remote-mcp"]; !ok {
		t.Fatalf("instance opt-in did not reach the config: %+v", resp.MCPServers)
	}
	// The CLI tool is opted in for this instance and is not an MCP server;
	// selecting the MCP subset is what keeps it from wedging the whole call.
	if _, ok := resp.MCPServers["ripgrep"]; ok {
		t.Fatal("a CLI tool must not appear under mcpServers")
	}
	if len(resp.MCPServers) != 1 {
		t.Fatalf("want exactly the one MCP tool, got %+v", resp.MCPServers)
	}
}

func TestProvisionByInstanceIgnoresAToolTheOwnerDisabledGlobally(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")
	tool, err := s.GetToolByName("remote-mcp")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := s.SetEnabled(tool.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	resp, err := Provision(context.Background(), s, ProvisionRequest{InstanceID: "inst-1"}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(resp.MCPServers) != 0 {
		t.Fatalf("globally disabled tool leaked into the config: %+v", resp.MCPServers)
	}
}

// An instance nobody has ticked anything for is the state of every instance on
// this box today. It must provision nothing and say so without an error, so a
// caller can tell "no opt-ins" apart from "the lookup broke".
func TestProvisionByInstanceWithNoOptInsIsEmptyNotAnError(t *testing.T) {
	s := openProvTest(t)
	resp, err := Provision(context.Background(), s, ProvisionRequest{InstanceID: "inst-nobody-touched"}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(resp.MCPServers) != 0 {
		t.Fatalf("want no servers, got %+v", resp.MCPServers)
	}
}

func TestProvisionRejectsBothToolsAndInstance(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")
	_, err := Provision(context.Background(), s, ProvisionRequest{
		Tools:      []string{"remote-mcp"},
		InstanceID: "inst-1",
	}, nil)
	if err == nil {
		t.Fatal("naming both a tool list and an instance must be an error, not a merge")
	}
}

// The explicit-names path keeps its old contract: ask for a CLI tool by name
// and you get an error, because you asked for something this endpoint cannot
// return. Only the instance path selects a subset.
func TestProvisionByNameStillRejectsANonMCPTool(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")
	_, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"ripgrep"}}, nil)
	if err == nil {
		t.Fatal("naming a CLI tool outright must still be an error")
	}
}

// --- provisioning by id ---------------------------------------------------
//
// llm-bridge-server intersects a principal's grants with an instance's opt-ins
// and holds tool-store ids on both sides, so it asks by id. Same rules as by
// name: every id must exist, be enabled, and be an MCP tool.

func TestProvisionByIDBuildsTheSameConfigAsByName(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")
	remote, err := s.GetToolByName("remote-mcp")
	if err != nil {
		t.Fatal(err)
	}
	byID, err := Provision(context.Background(), s, ProvisionRequest{ToolIDs: []int64{remote.ID}}, nil)
	if err != nil {
		t.Fatalf("provision by id: %v", err)
	}
	byName, err := Provision(context.Background(), s, ProvisionRequest{Tools: []string{"remote-mcp"}}, nil)
	if err != nil {
		t.Fatalf("provision by name: %v", err)
	}
	if len(byID.MCPServers) != 1 || !reflect.DeepEqual(byID.MCPServers["remote-mcp"], byName.MCPServers["remote-mcp"]) {
		t.Fatalf("by id %+v != by name %+v", byID.MCPServers, byName.MCPServers)
	}
}

func TestProvisionByIDRefusesAMissingIdAndANonMCPTool(t *testing.T) {
	s := openProvTest(t)
	seedInstanceOptIns(t, s, "inst-1")
	if _, err := Provision(context.Background(), s, ProvisionRequest{ToolIDs: []int64{999999}}, nil); err == nil || !strings.Contains(err.Error(), "tool id 999999 not found") {
		t.Fatalf("missing id: err = %v", err)
	}
	cli, err := s.GetToolByName("ripgrep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(context.Background(), s, ProvisionRequest{ToolIDs: []int64{cli.ID}}, nil); err == nil || !strings.Contains(err.Error(), "only mcp tools") {
		t.Fatalf("cli by id: err = %v", err)
	}
}

func TestProvisionRefusesMoreThanOneSource(t *testing.T) {
	s := openProvTest(t)
	for _, req := range []ProvisionRequest{
		{Tools: []string{"a"}, ToolIDs: []int64{1}},
		{ToolIDs: []int64{1}, InstanceID: "inst-1"},
		{Tools: []string{"a"}, InstanceID: "inst-1"},
	} {
		if _, err := Provision(context.Background(), s, req, nil); err == nil || !strings.Contains(err.Error(), "pick one") {
			t.Fatalf("%+v: err = %v, want a refusal to merge sources", req, err)
		}
	}
}
