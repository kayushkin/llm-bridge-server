package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeToolStore returns a server that always returns the given JSON body for
// POST /provision and 404 otherwise.
func fakeToolStore(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, ok := req["tools"]; !ok {
			http.Error(w, "missing tools", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newServerWithToolStoreURL(url string) *Server {
	return &Server{cfg: &config.Config{ToolStoreURL: url}}
}

func TestInjectMCPConfigNoOpIfKeyAbsent(t *testing.T) {
	srv := fakeToolStore(t, `{"mcpServers":{}}`)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"settings":"{}"}`),
	}
	before := string(sess.HarnessConfig)
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(sess.HarnessConfig) != before {
		t.Fatalf("HarnessConfig was mutated: before=%s after=%s", before, sess.HarnessConfig)
	}
}

func TestInjectMCPConfigHappyPath(t *testing.T) {
	body := `{"mcpServers":{"brave-search":{"command":"npx","args":["-y","x"],"env":{"BRAVE_API_KEY":"k"}}}}`
	srv := fakeToolStore(t, body)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		SessionID:     "test-bid",
		HarnessConfig: json.RawMessage(`{"tool_store_tools":["brave-search"]}`),
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("parse mutated config: %v", err)
	}
	if _, has := cfg["tool_store_tools"]; has {
		t.Fatal("tool_store_tools should be removed after provision")
	}
	pathJSON, has := cfg["mcp_config"]
	if !has {
		t.Fatal("mcp_config not set")
	}
	var path string
	if err := json.Unmarshal(pathJSON, &path); err != nil {
		t.Fatalf("mcp_config not a string: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmpfile: %v", err)
	}
	if !strings.Contains(string(got), "brave-search") || !strings.Contains(string(got), "BRAVE_API_KEY") {
		t.Fatalf("tmpfile body unexpected: %s", got)
	}
}

func TestInjectMCPConfigEmptyToolListIsNoOp(t *testing.T) {
	srv := fakeToolStore(t, `{"mcpServers":{}}`)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"tool_store_tools":[],"x":"y"}`),
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(sess.HarnessConfig, &cfg)
	if _, has := cfg["tool_store_tools"]; has {
		t.Fatal("empty list should be cleaned up")
	}
	if _, has := cfg["mcp_config"]; has {
		t.Fatal("no mcp_config should be set for empty tool list")
	}
}

func TestInjectMCPConfigConflictingMCPConfigErrors(t *testing.T) {
	srv := fakeToolStore(t, `{"mcpServers":{}}`)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"tool_store_tools":["x"],"mcp_config":"/some/path"}`),
	}
	err := s.injectMCPConfig(sess)
	if err == nil {
		t.Fatal("expected error when both fields present")
	}
}

func TestInjectMCPConfigNonClaudeCodeHarnessSkipped(t *testing.T) {
	srv := fakeToolStore(t, `{"mcpServers":{}}`)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"tool_store_tools":["x"]}`),
	}
	before := string(sess.HarnessConfig)
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(sess.HarnessConfig) != before {
		t.Fatal("non-claudecode harness should be skipped")
	}
}

func TestInjectMCPConfigUpstreamErrorPropagates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"tool not found"}`, 400)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"tool_store_tools":["nope"]}`),
	}
	err := s.injectMCPConfig(sess)
	if err == nil {
		t.Fatal("expected error from upstream 400")
	}
}

// --- instance opt-ins ----------------------------------------------------
//
// These pin the other half of the wire: what a tick on the Tools page does to
// a session spawned on that instance.

// fakeInstanceToolStore answers POST /provision for instance_id requests,
// recording the instance it was asked about.
func fakeInstanceToolStore(t *testing.T, byInstance map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		id, _ := req["instance_id"].(string)
		if id == "" {
			http.Error(w, "missing instance_id", 400)
			return
		}
		asked = append(asked, id)
		body, ok := byInstance[id]
		if !ok {
			body = `{"mcpServers":{}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &asked
}

func mcpConfigPath(t *testing.T, sess *store.Session) string {
	t.Helper()
	if len(sess.HarnessConfig) == 0 {
		return ""
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	raw, has := cfg["mcp_config"]
	if !has {
		return ""
	}
	var path string
	if err := json.Unmarshal(raw, &path); err != nil {
		t.Fatalf("mcp_config not a string: %v", err)
	}
	return path
}

func TestInjectMCPConfigProvisionsTheInstanceOptIns(t *testing.T) {
	body := `{"mcpServers":{"brave-search":{"command":"npx","args":["-y","x"]}}}`
	srv, asked := fakeInstanceToolStore(t, map[string]string{"inst-1": body})
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:    msg.HarnessClaudeCode,
		SessionID:  "test-bid",
		InstanceID: "inst-1",
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "inst-1" {
		t.Fatalf("tool-store was asked about %v, want [inst-1]", *asked)
	}
	path := mcpConfigPath(t, sess)
	if path == "" {
		t.Fatal("instance opt-ins did not reach the session")
	}
	t.Cleanup(func() { os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmpfile: %v", err)
	}
	if !strings.Contains(string(got), "brave-search") {
		t.Fatalf("tmpfile body unexpected: %s", got)
	}
}

// The state of every instance on this box today: no rows in instance_tools.
// Nothing must change about such a session, including its HarnessConfig.
func TestInjectMCPConfigInstanceWithNoOptInsChangesNothing(t *testing.T) {
	srv, _ := fakeInstanceToolStore(t, nil)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		InstanceID:    "inst-untouched",
		HarnessConfig: json.RawMessage(`{"settings":"{}"}`),
	}
	before := string(sess.HarnessConfig)
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(sess.HarnessConfig) != before {
		t.Fatalf("HarnessConfig was mutated: before=%s after=%s", before, sess.HarnessConfig)
	}
}

// A registry outage must not stop the fleet from starting sessions. This is
// the assertion that matters most here: the opposite policy would take every
// spawn on this box down with tool-store.
func TestInjectMCPConfigStartsTheSessionWhenToolStoreIsDown(t *testing.T) {
	srv, _ := fakeInstanceToolStore(t, nil)
	url := srv.URL
	srv.Close()
	s := newServerWithToolStoreURL(url)
	sess := &store.Session{
		Harness:    msg.HarnessClaudeCode,
		InstanceID: "inst-1",
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("an unreachable tool-store must not abort the spawn, got: %v", err)
	}
	if p := mcpConfigPath(t, sess); p != "" {
		t.Fatalf("no config should have been written, got %q", p)
	}
}

// A stale opt-in row — a tool since deleted, or one whose credential no longer
// resolves — makes tool-store answer 4xx. Same reasoning: log it, start the
// session. A named request in the same state still aborts, which
// TestInjectMCPConfigUpstreamErrorPropagates pins.
func TestInjectMCPConfigStartsTheSessionWhenAnOptInIsStale(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"tool \"gone\" not found"}`, 400)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, InstanceID: "inst-1"}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("a stale opt-in must not abort the spawn, got: %v", err)
	}
}

// An explicitly empty tool_store_tools is an opt-out, and it must win over the
// instance's standing preference — otherwise there is no way for a caller to
// ask for a session with no MCP servers on an instance that has opt-ins.
func TestInjectMCPConfigEmptyNamedListOptsOutOfInstanceDefaults(t *testing.T) {
	body := `{"mcpServers":{"brave-search":{"command":"npx"}}}`
	srv, asked := fakeInstanceToolStore(t, map[string]string{"inst-1": body})
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		InstanceID:    "inst-1",
		HarnessConfig: json.RawMessage(`{"tool_store_tools":[]}`),
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store should not have been called, was asked about %v", *asked)
	}
	if p := mcpConfigPath(t, sess); p != "" {
		t.Fatalf("no config should have been written, got %q", p)
	}
}

// A caller that named its own mcp_config already answered this question.
func TestInjectMCPConfigPresetPathWinsOverInstanceDefaults(t *testing.T) {
	body := `{"mcpServers":{"brave-search":{"command":"npx"}}}`
	srv, asked := fakeInstanceToolStore(t, map[string]string{"inst-1": body})
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		InstanceID:    "inst-1",
		HarnessConfig: json.RawMessage(`{"mcp_config":"/caller/own.json"}`),
	}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store should not have been called, was asked about %v", *asked)
	}
	if p := mcpConfigPath(t, sess); p != "/caller/own.json" {
		t.Fatalf("caller's own path was overwritten: %q", p)
	}
}

// Only the Claude Code bridge consumes mcp_config, so no other harness should
// cost a round trip to tool-store.
func TestInjectMCPConfigNonClaudeCodeInstanceIsNotLookedUp(t *testing.T) {
	srv, asked := fakeInstanceToolStore(t, nil)
	s := newServerWithToolStoreURL(srv.URL)
	sess := &store.Session{Harness: msg.HarnessCodex, InstanceID: "inst-1"}
	if err := s.injectMCPConfig(sess); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(*asked) != 0 {
		t.Fatalf("tool-store should not have been called, was asked about %v", *asked)
	}
}
