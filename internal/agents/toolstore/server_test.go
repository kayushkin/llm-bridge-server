package toolstore

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ts"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, s, HandlerOptions{})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		s.Close()
	})
	return srv, s
}

func TestHTTPHealth(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestHTTPCRUD(t *testing.T) {
	srv, _ := newServer(t)

	// Create
	body := `{
		"name":"brave-search",
		"description":"Web search via Brave Search API",
		"kind":"mcp",
		"env_keys":["BRAVE_API_KEY"],
		"tags":["search"],
		"mcp":{"transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-brave-search"]},
		"enabled":true
	}`
	resp, err := http.Post(srv.URL+"/tools", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create status: %d", resp.StatusCode)
	}
	var created Tool
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("ID not set")
	}

	// List
	resp, _ = http.Get(srv.URL + "/tools")
	var list []Tool
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}

	// Get by id
	resp, _ = http.Get(srv.URL + "/tools/" + itoa(created.ID))
	var got Tool
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.MCP == nil || got.MCP.Command != "npx" {
		t.Fatalf("MCP not roundtripped: %+v", got.MCP)
	}

	// Get by name
	resp, _ = http.Get(srv.URL + "/tools/by-name/brave-search")
	if resp.StatusCode != 200 {
		t.Fatalf("by-name status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Disable
	resp, _ = http.Post(srv.URL+"/tools/"+itoa(created.ID)+"/disable", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("disable status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + "/tools?enabled=true")
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("expected 0 enabled, got %d", len(list))
	}

	// Re-enable
	resp, _ = http.Post(srv.URL+"/tools/"+itoa(created.ID)+"/enable", "", nil)
	resp.Body.Close()

	// Delete
	req, _ := http.NewRequest("DELETE", srv.URL+"/tools/"+itoa(created.ID), nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("delete status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 404 path
	resp, _ = http.Get(srv.URL + "/tools/" + itoa(created.ID))
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPInvokeLocal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ts"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Seed a local tool row.
	if _, err := s.UpsertTool(&Tool{
		Name:        "echo",
		Description: "test",
		Kind:        KindLocal,
		Local:       &LocalSpec{Symbol: "echo"},
		Enabled:     true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	mux := http.NewServeMux()
	RegisterHandlers(mux, s, HandlerOptions{
		InvokeLocal: func(ctx context.Context, name, input string) (string, error) {
			return name + ":" + input, nil
		},
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/tools/by-name/echo/invoke", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var got map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["output"] != `echo:{"x":1}` {
		t.Fatalf("output: %q", got["output"])
	}
}

func TestHTTPInvokeWithoutFunc(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{
		Name:    "x",
		Kind:    KindLocal,
		Local:   &LocalSpec{Symbol: "x"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resp, _ := http.Post(srv.URL+"/tools/by-name/x/invoke", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 501 {
		t.Fatalf("expected 501, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPInvokeMCP(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{
		Name:    "brave",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resp, _ := http.Post(srv.URL+"/tools/by-name/brave/invoke", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 (mcp not invokable), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPInvokeCLI(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "echo-cli",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "echo",
			ArgsTemplate: []string{"hello", "{{name}}"},
			TimeoutMs:    2000,
		},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resp, _ := http.Post(srv.URL+"/tools/by-name/echo-cli/invoke",
		"application/json", strings.NewReader(`{"name":"world"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var got map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if strings.TrimSpace(got["output"]) != "hello world" {
		t.Fatalf("output: %q", got["output"])
	}
}

func TestHTTPSpec(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{
		Name: "echo-cli",
		Kind: KindCLI,
		CLI: &CLISpec{
			Command:      "echo",
			ArgsTemplate: []string{"{{msg}}"},
		},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resp, _ := http.Get(srv.URL + "/tools/by-name/echo-cli/spec")
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var got CLISpec
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Command != "echo" || len(got.ArgsTemplate) != 1 {
		t.Fatalf("spec mismatch: %+v", got)
	}
}

func TestHTTPListLocals(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "ts"))
	t.Cleanup(func() { s.Close() })
	mux := http.NewServeMux()
	RegisterHandlers(mux, s, HandlerOptions{
		ListLocals: func() []LocalDescriptor {
			return []LocalDescriptor{
				{Name: "shell", Description: "bash"},
				{Name: "grep", Description: "rg"},
			}
		},
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, _ := http.Get(srv.URL + "/locals")
	var got []LocalDescriptor
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got) != 2 || got[0].Name != "shell" {
		t.Fatalf("locals mismatch: %+v", got)
	}
}

func TestHTTPInstanceToolsCRUD(t *testing.T) {
	srv, s := newServer(t)
	if _, err := s.UpsertTool(&Tool{
		Name:    "brave-search",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Initially empty
	resp, _ := http.Get(srv.URL + "/instances/inst-1/tools")
	var tools []Tool
	_ = json.NewDecoder(resp.Body).Decode(&tools)
	resp.Body.Close()
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}

	// Enable
	resp, _ = http.Post(srv.URL+"/instances/inst-1/tools/by-name/brave-search", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("enable status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Now visible
	resp, _ = http.Get(srv.URL + "/instances/inst-1/tools")
	_ = json.NewDecoder(resp.Body).Decode(&tools)
	resp.Body.Close()
	if len(tools) != 1 || tools[0].Name != "brave-search" {
		t.Fatalf("expected brave-search, got %v", tools)
	}

	// And the inverse view
	resp, _ = http.Get(srv.URL + "/tools/by-name/brave-search/instances")
	var ids []string
	_ = json.NewDecoder(resp.Body).Decode(&ids)
	resp.Body.Close()
	if len(ids) != 1 || ids[0] != "inst-1" {
		t.Fatalf("expected [inst-1], got %v", ids)
	}

	// Disable
	req, _ := http.NewRequest("DELETE", srv.URL+"/instances/inst-1/tools/by-name/brave-search", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("disable status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty again
	resp, _ = http.Get(srv.URL + "/instances/inst-1/tools")
	_ = json.NewDecoder(resp.Body).Decode(&tools)
	resp.Body.Close()
	if len(tools) != 0 {
		t.Fatalf("expected 0 after disable, got %d", len(tools))
	}
}

func TestHTTPInstanceToolsRejectsGloballyDisabled(t *testing.T) {
	srv, s := newServer(t)
	_, _ = s.UpsertTool(&Tool{
		Name:    "x",
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: false,
	})
	resp, _ := http.Post(srv.URL+"/instances/inst-1/tools/by-name/x", "", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 for globally-disabled tool, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPInstanceToolsMissing(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := http.Post(srv.URL+"/instances/i/tools/by-name/nope", "", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for missing tool, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPRejectsBadJSON(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := http.Post(srv.URL+"/tools", "application/json", bytes.NewReader([]byte(`{"name":`)))
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPValidatesKind(t *testing.T) {
	srv, _ := newServer(t)
	body := `{"name":"x","kind":"bogus"}`
	resp, _ := http.Post(srv.URL+"/tools", "application/json", strings.NewReader(body))
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 on bad kind, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
