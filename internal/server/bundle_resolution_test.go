package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func fakeRepoStore(t *testing.T, tags []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /detect", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["path"] == "" {
			http.Error(w, "missing path", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fakeBundleStore(t *testing.T, tools, skills []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /resolve", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["repo_tags"]; !ok {
			http.Error(w, "missing repo_tags", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bundles": []string{"base", "react"},
			"skills":  skills,
			"tools":   tools,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newBundleServer(repoURL, bundleURL string) *Server {
	return &Server{cfg: &config.Config{RepoStoreURL: repoURL, BundleStoreURL: bundleURL}}
}

func toolStoreTools(t *testing.T, hc json.RawMessage) []string {
	t.Helper()
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(hc, &cfg); err != nil {
		t.Fatalf("parse HarnessConfig: %v", err)
	}
	raw, ok := cfg["tool_store_tools"]
	if !ok {
		return nil
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("parse tool_store_tools: %v", err)
	}
	return tools
}

func TestBundleResolutionNoOpWithoutAutoBundle(t *testing.T) {
	s := newBundleServer(fakeRepoStore(t, []string{"react"}).URL, fakeBundleStore(t, []string{"playwright"}, nil).URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, HarnessConfig: json.RawMessage(`{"work_dir":"/x"}`)}
	before := string(sess.HarnessConfig)
	if err := s.injectBundleResolution(sess); err != nil {
		t.Fatal(err)
	}
	if string(sess.HarnessConfig) != before {
		t.Fatalf("mutated despite no auto_bundle: %s", sess.HarnessConfig)
	}
}

func TestBundleResolutionHappyPath(t *testing.T) {
	s := newBundleServer(fakeRepoStore(t, []string{"react", "go"}).URL, fakeBundleStore(t, []string{"playwright"}, []string{"browser-automation"}).URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		SessionID:     "bid-1",
		HarnessConfig: json.RawMessage(`{"auto_bundle":true,"work_dir":"/home/x/dash"}`),
	}
	if err := s.injectBundleResolution(sess); err != nil {
		t.Fatal(err)
	}
	got := toolStoreTools(t, sess.HarnessConfig)
	if len(got) != 1 || got[0] != "playwright" {
		t.Fatalf("expected [playwright], got %v", got)
	}
}

func TestBundleResolutionUnionsExistingTools(t *testing.T) {
	s := newBundleServer(fakeRepoStore(t, []string{"react"}).URL, fakeBundleStore(t, []string{"playwright", "chrome-devtools"}, nil).URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"auto_bundle":true,"work_dir":"/x","tool_store_tools":["brave-search","playwright"]}`),
	}
	if err := s.injectBundleResolution(sess); err != nil {
		t.Fatal(err)
	}
	got := toolStoreTools(t, sess.HarnessConfig)
	// union: brave-search (existing) + playwright (dedup) + chrome-devtools (new)
	if len(got) != 3 {
		t.Fatalf("expected 3 unioned tools, got %v", got)
	}
}

func TestBundleResolutionSkipsWithoutWorkDir(t *testing.T) {
	s := newBundleServer(fakeRepoStore(t, []string{"react"}).URL, fakeBundleStore(t, []string{"playwright"}, nil).URL)
	sess := &store.Session{Harness: msg.HarnessClaudeCode, HarnessConfig: json.RawMessage(`{"auto_bundle":true}`)}
	if err := s.injectBundleResolution(sess); err != nil {
		t.Fatal(err)
	}
	if toolStoreTools(t, sess.HarnessConfig) != nil {
		t.Fatalf("should not have injected tools without work_dir: %s", sess.HarnessConfig)
	}
}

func TestBundleResolutionSkipsWhenMCPConfigPresent(t *testing.T) {
	s := newBundleServer(fakeRepoStore(t, []string{"react"}).URL, fakeBundleStore(t, []string{"playwright"}, nil).URL)
	sess := &store.Session{
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"auto_bundle":true,"work_dir":"/x","mcp_config":"/tmp/x.json"}`),
	}
	if err := s.injectBundleResolution(sess); err != nil {
		t.Fatal(err)
	}
	if toolStoreTools(t, sess.HarnessConfig) != nil {
		t.Fatalf("should not inject when mcp_config already set: %s", sess.HarnessConfig)
	}
}
