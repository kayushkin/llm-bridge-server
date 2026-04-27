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
		BridgeID:      "test-bid",
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
