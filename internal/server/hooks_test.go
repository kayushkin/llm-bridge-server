package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func testServerWithHookStore(t *testing.T) (*Server, *hookstore.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hks, err := hookstore.Open(filepath.Join(dir, "hooks.db"))
	if err != nil {
		t.Fatalf("open hook-store: %v", err)
	}
	t.Cleanup(func() { hks.Close() })

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     "http://localhost:0",
	}
	srv := New(st, nil, nil, nil, hks, nil, nil, cfg)
	return srv, hks
}

func TestCreateHook_Success(t *testing.T) {
	srv, _ := testServerWithHookStore(t)

	req := msg.Hook{
		Harness:   msg.HarnessClaudeCode,
		Event:     "PreToolUse",
		Matcher:   "Bash",
		Command:   "echo '{\"decision\":\"approve\"}'",
		ScopeKind: msg.HookScopeGlobal,
	}
	resp := doJSON(t, srv, "POST", "/hooks", req)
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	got := decodeJSON[msg.Hook](t, resp)
	if got.ID == "" || !strings.HasPrefix(got.ID, "hook_") {
		t.Errorf("id = %q, want hook_ prefix", got.ID)
	}
	if !got.Enabled {
		t.Error("new hook should default to enabled")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not populated")
	}
}

func TestCreateHook_MissingCommand(t *testing.T) {
	srv, _ := testServerWithHookStore(t)

	resp := doJSON(t, srv, "POST", "/hooks", msg.Hook{
		Harness:   msg.HarnessClaudeCode,
		Event:     "PreToolUse",
		ScopeKind: msg.HookScopeGlobal,
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateHook_InstanceScopeMissingID(t *testing.T) {
	srv, _ := testServerWithHookStore(t)

	resp := doJSON(t, srv, "POST", "/hooks", msg.Hook{
		Harness:   msg.HarnessClaudeCode,
		Event:     "PreToolUse",
		Command:   "echo",
		ScopeKind: msg.HookScopeInstance,
	})
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListHooks_Filters(t *testing.T) {
	srv, hks := testServerWithHookStore(t)

	for _, h := range []msg.Hook{
		{ID: "a", Harness: msg.HarnessClaudeCode, Event: "PreToolUse", Command: "c", ScopeKind: msg.HookScopeGlobal, Enabled: true},
		{ID: "b", Harness: msg.HarnessClaudeCode, Event: "PostToolUse", Command: "c", ScopeKind: msg.HookScopeGlobal, Enabled: true},
		{ID: "c", Harness: msg.HarnessCodex, Event: "PreToolUse", Command: "c", ScopeKind: msg.HookScopeGlobal, Enabled: false},
	} {
		if err := hks.CreateHook(&h); err != nil {
			t.Fatalf("seed %s: %v", h.ID, err)
		}
	}

	resp := doJSON(t, srv, "GET", "/hooks?harness=claude_code", nil)
	got := decodeJSON[[]msg.Hook](t, resp)
	if len(got) != 2 {
		t.Errorf("harness filter: got %d, want 2", len(got))
	}

	resp = doJSON(t, srv, "GET", "/hooks?event=PreToolUse", nil)
	got = decodeJSON[[]msg.Hook](t, resp)
	if len(got) != 2 {
		t.Errorf("event filter: got %d, want 2", len(got))
	}

	resp = doJSON(t, srv, "GET", "/hooks?enabled=false", nil)
	got = decodeJSON[[]msg.Hook](t, resp)
	if len(got) != 1 || got[0].ID != "c" {
		t.Errorf("enabled=false filter: got %+v", got)
	}
}

func TestGetHook_NotFound(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	resp := doJSON(t, srv, "GET", "/hooks/does_not_exist", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestUpdateHook_Disable(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	h := &msg.Hook{
		ID: "toggle", Harness: msg.HarnessClaudeCode, Event: "PreToolUse",
		Command: "c", ScopeKind: msg.HookScopeGlobal, Enabled: true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, srv, "PATCH", "/hooks/toggle", map[string]any{"enabled": false})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeJSON[msg.Hook](t, resp)
	if got.Enabled {
		t.Error("enabled should be false after patch")
	}

	// Rest of the fields should be preserved.
	if got.Command != "c" || got.Event != "PreToolUse" {
		t.Errorf("other fields mutated: %+v", got)
	}
}

func TestDeleteHook(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	h := &msg.Hook{
		ID: "del", Harness: msg.HarnessClaudeCode, Event: "PreToolUse",
		Command: "c", ScopeKind: msg.HookScopeGlobal, Enabled: true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatal(err)
	}

	resp := doJSON(t, srv, "DELETE", "/hooks/del", nil)
	if resp.StatusCode != 204 {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = doJSON(t, srv, "DELETE", "/hooks/del", nil)
	if resp.StatusCode != 404 {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestExecHook_EchoCommand(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	// cat: re-emit stdin as stdout. Lets us verify pass-through.
	h := &msg.Hook{
		ID: "echo", Harness: msg.HarnessClaudeCode, Event: "PreToolUse",
		Command: "cat", ScopeKind: msg.HookScopeGlobal, Enabled: true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatal(err)
	}

	payload := `{"session_id":"s1","tool_name":"Bash","hook_event_name":"PreToolUse","tool_input":{"command":"ls"}}`
	req := httptest.NewRequest("POST", "/hooks/exec/echo", bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if got["session_id"] != "s1" || got["tool_name"] != "Bash" {
		t.Errorf("stdout did not round-trip input: %+v", got)
	}
}

func TestExecHook_JSONDecisionExtracted(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	// Register a hook that ignores stdin and outputs a deny decision.
	h := &msg.Hook{
		ID: "deny", Harness: msg.HarnessClaudeCode, Event: "PreToolUse",
		Command: `cat >/dev/null; printf '{"decision":"deny","reason":"no"}'`,
		ScopeKind: msg.HookScopeGlobal, Enabled: true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/hooks/exec/deny",
		bytes.NewReader([]byte(`{"session_id":"","tool_name":""}`)))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Decision != "deny" || out.Reason != "no" {
		t.Errorf("deny response not returned: %+v", out)
	}
}

func TestExecHook_NotFound(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	resp := doJSON(t, srv, "POST", "/hooks/exec/does_not_exist", map[string]any{"session_id": "s"})
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestExecHook_Disabled(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	h := &msg.Hook{
		ID: "off", Harness: msg.HarnessClaudeCode, Event: "PreToolUse",
		Command: "cat", ScopeKind: msg.HookScopeGlobal, Enabled: false,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, srv, "POST", "/hooks/exec/off", map[string]any{"session_id": "s"})
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", resp.StatusCode)
	}
}

func TestHookRoutes_DisabledWithoutHookStore(t *testing.T) {
	// testServer creates a Server without a hook-store. /hooks routes should
	// not be registered, so the mux falls through to 404.
	srv, _ := testServer(t)
	resp := doJSON(t, srv, "GET", "/hooks", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 when hook-store missing", resp.StatusCode)
	}
}
