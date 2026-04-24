package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// testServer creates a test server with an in-memory SQLite store.
// Optional stores (agent, memory, harness, model) are nil.
func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     "http://localhost:0", // unused in unit tests
	}

	srv := New(st, nil, nil, nil, nil, nil, nil, cfg)
	return srv, st
}

// testServerWithInstance returns a server wired to a harness-store that
// already contains one enabled instance of the given harness type. The
// instance id is returned so tests can reference it in create requests.
func testServerWithInstance(t *testing.T, harness msg.Harness) (*Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hs, err := harnessstore.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open harness-store: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	inst := &msg.Instance{
		ID:          "inst_test",
		HarnessType: harness,
		Name:        "test-instance",
		Host:        "localhost",
		Transport:   msg.TransportLocal,
		Enabled:     true,
	}
	if err := hs.CreateInstance(inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     "http://localhost:0",
	}

	srv := New(st, nil, nil, hs, nil, nil, nil, cfg)
	return srv, st, inst.ID
}

func doJSON(t *testing.T, srv http.Handler, method, path string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w.Result()
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /health
// ──────────────────────────────────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/health", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	health := decodeJSON[msg.HealthResponse](t, resp)
	if health.Status == "" {
		t.Error("status field is empty")
	}
	if len(health.Harnesses) == 0 {
		t.Error("expected at least one harness in list")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /harnesses
// ──────────────────────────────────────────────────────────────────────────────

func TestHarnesses(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/harnesses", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var harnesses []msg.HarnessInfo
	harnesses = decodeJSON[[]msg.HarnessInfo](t, resp)

	if len(harnesses) == 0 {
		t.Fatal("expected harness list")
	}

	// Verify claude_code is in the list
	found := false
	for _, h := range harnesses {
		if h.Name == "claude_code" {
			found = true
			if h.Label != "Claude Code" {
				t.Errorf("claude_code label = %q, want Claude Code", h.Label)
			}
			if len(h.Capabilities) == 0 {
				t.Error("claude_code should have capabilities")
			}
		}
	}
	if !found {
		t.Error("claude_code not found in harness list")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /harnesses/{name}/capabilities
// ──────────────────────────────────────────────────────────────────────────────

func TestHarnessCapabilities_ClaudeCode(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/harnesses/claude_code/capabilities", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	info := decodeJSON[msg.HarnessInfo](t, resp)
	if info.Name != "claude_code" {
		t.Errorf("name = %q, want claude_code", info.Name)
	}
	want := []string{
		"PreToolUse", "PostToolUse", "UserPromptSubmit", "Notification",
		"Stop", "SubagentStop", "PreCompact", "SessionStart", "SessionEnd",
	}
	if len(info.HookEvents) != len(want) {
		t.Fatalf("hook_events = %v, want %v", info.HookEvents, want)
	}
	for i, ev := range want {
		if info.HookEvents[i] != ev {
			t.Errorf("hook_events[%d] = %q, want %q", i, info.HookEvents[i], ev)
		}
	}
}

func TestHarnessCapabilities_NoHooksHarness(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/harnesses/hermes/capabilities", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	info := decodeJSON[msg.HarnessInfo](t, resp)
	if len(info.HookEvents) != 0 {
		t.Errorf("hermes hook_events = %v, want empty", info.HookEvents)
	}
}

func TestHarnessCapabilities_UnknownHarness(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/harnesses/does_not_exist/capabilities", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Session CRUD via HTTP
// ──────────────────────────────────────────────────────────────────────────────

func TestCreateSession_NoAutoStart(t *testing.T) {
	srv, _, instID := testServerWithInstance(t, "claude_code")

	req := msg.CreateSessionRequest{
		Harness:     "claude_code",
		InstanceID:  instID,
		ClientID:    "fe_test_1",
		DisplayName: "Test Task",
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}

	sess := decodeJSON[msg.ManagedSession](t, resp)
	if sess.BridgeID == "" {
		t.Error("bridge_id is empty")
	}
	if sess.ClientID != "fe_test_1" {
		t.Errorf("client_id = %q, want fe_test_1", sess.ClientID)
	}
	if sess.InstanceID != instID {
		t.Errorf("instance_id = %q, want %q", sess.InstanceID, instID)
	}
	if sess.State != "idle" {
		t.Errorf("state = %q, want idle (no auto_start)", sess.State)
	}
	if sess.Harness != "claude_code" {
		t.Errorf("harness = %q, want claude_code", sess.Harness)
	}
}

func TestCreateSession_RejectedWithoutInstance(t *testing.T) {
	srv, _ := testServer(t)

	req := msg.CreateSessionRequest{
		Harness:  "claude_code",
		ClientID: "fe_1",
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 503 (no instance): %s", resp.StatusCode, body)
	}
}

func TestCreateSession_MissingClientID(t *testing.T) {
	srv, _ := testServer(t)

	req := msg.CreateSessionRequest{
		Harness: "claude_code",
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (missing client_id)", resp.StatusCode)
	}
}

func TestCreateSession_InvalidHarness(t *testing.T) {
	srv, _ := testServer(t)

	req := msg.CreateSessionRequest{
		Harness:  "nonexistent_harness",
		ClientID: "fe_1",
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (invalid harness)", resp.StatusCode)
	}
}

func TestCreateSession_InvalidBody(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest("POST", "/sessions", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestListSessions(t *testing.T) {
	srv, st := testServer(t)

	// Create some sessions directly in store
	for _, id := range []string{"br_a", "br_b"} {
		st.CreateSession(&store.Session{
			BridgeID: id, ClientID: "fe_x", Harness: "mock", State: "idle",
		})
	}

	resp := doJSON(t, srv, "GET", "/sessions", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var sessions []msg.ManagedSession
	sessions = decodeJSON[[]msg.ManagedSession](t, resp)
	if len(sessions) != 2 {
		t.Errorf("session count = %d, want 2", len(sessions))
	}
}

func TestListSessions_Empty(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/sessions", nil)
	sessions := decodeJSON[[]msg.ManagedSession](t, resp)
	if sessions == nil {
		t.Error("expected non-nil empty array, got nil")
	}
	if len(sessions) != 0 {
		t.Errorf("session count = %d, want 0", len(sessions))
	}
}

func TestGetSession(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_get", ClientID: "fe_x", DisplayName: "Get Test",
		Harness: "mock", State: "idle",
	})

	resp := doJSON(t, srv, "GET", "/sessions/br_get", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	sess := decodeJSON[msg.ManagedSession](t, resp)
	if sess.DisplayName != "Get Test" {
		t.Errorf("display_name = %q, want Get Test", sess.DisplayName)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/sessions/nonexistent", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Session actions (interrupt, resume, stop, compact, fork, config)
// ──────────────────────────────────────────────────────────────────────────────

func TestInterruptSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/interrupt", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestInterruptSession_NotRunning(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_int", ClientID: "fe_x", Harness: "mock", State: "idle",
	})

	resp := doJSON(t, srv, "POST", "/sessions/br_int/interrupt", nil)
	if resp.StatusCode != 409 {
		t.Errorf("status = %d, want 409 (session not running)", resp.StatusCode)
	}
}

func TestResumeSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/resume", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestResumeSession_NotIdle(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_res", ClientID: "fe_x", Harness: "mock", State: "running",
	})

	resp := doJSON(t, srv, "POST", "/sessions/br_res/resume", nil)
	if resp.StatusCode != 409 {
		t.Errorf("status = %d, want 409 (session not idle)", resp.StatusCode)
	}
}

func TestStopSession(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_stop", ClientID: "fe_x", Harness: "mock", State: "running",
	})

	resp := doJSON(t, srv, "POST", "/sessions/br_stop/stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	sess := decodeJSON[msg.ManagedSession](t, resp)
	if sess.State != "aborted" {
		t.Errorf("state = %q, want aborted", sess.State)
	}

	// Verify persisted state
	got, _ := st.GetSession("br_stop")
	if got.State != "aborted" {
		t.Errorf("persisted state = %q, want aborted", got.State)
	}
}

func TestStopSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/stop", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSendMessage_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/send", msg.SendMessageRequest{Message: "hello"})
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSendMessage_InvalidBody(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_msg", ClientID: "fe_x", Harness: "mock", State: "idle",
	})

	req := httptest.NewRequest("POST", "/sessions/br_msg/send", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestForkSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/fork", msg.ForkSessionRequest{ClientID: "fe_1"})
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestForkSession_MissingClientID(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_fork", ClientID: "fe_x", Harness: "mock", State: "idle",
	})

	resp := doJSON(t, srv, "POST", "/sessions/br_fork/fork", msg.ForkSessionRequest{})
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (missing client_id): %s", resp.StatusCode, body)
	}
}

func TestCompactSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/compact", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestConfigSession_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "POST", "/sessions/nonexistent/config", msg.ConfigSessionRequest{Model: "opus"})
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestConfigSession_InvalidBody(t *testing.T) {
	srv, st := testServer(t)

	st.CreateSession(&store.Session{
		BridgeID: "br_cfg", ClientID: "fe_x", Harness: "mock", State: "idle",
	})

	req := httptest.NewRequest("POST", "/sessions/br_cfg/config", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SSE events endpoint
// ──────────────────────────────────────────────────────────────────────────────

func TestSessionEvents_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/sessions/nonexistent/events", nil)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Credentials
// ──────────────────────────────────────────────────────────────────────────────

func TestCredentialsList(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/credentials", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Should return an array (may be empty)
	var creds []msg.Credential
	creds = decodeJSON[[]msg.Credential](t, resp)
	if creds == nil {
		t.Error("expected non-nil credential array")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Bridge preferences
// ──────────────────────────────────────────────────────────────────────────────

func TestBridgePrefs_GetEmpty(t *testing.T) {
	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/bridge-prefs", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	prefs := decodeJSON[msg.BridgePrefs](t, resp)
	// Empty prefs is fine
	_ = prefs
}

func TestBridgePrefs_PutAndGet(t *testing.T) {
	srv, _ := testServer(t)

	// Set preferences
	prefs := msg.BridgePrefs{
		LastHarness: "claude_code",
		LastSession: map[string]string{"claude_code": "br_123"},
	}
	resp := doJSON(t, srv, "PUT", "/bridge-prefs", prefs)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d, want 200: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Get them back
	resp = doJSON(t, srv, "GET", "/bridge-prefs", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	got := decodeJSON[msg.BridgePrefs](t, resp)
	if got.LastHarness != "claude_code" {
		t.Errorf("last_harness = %q, want claude_code", got.LastHarness)
	}
	if got.LastSession["claude_code"] != "br_123" {
		t.Errorf("last_session[claude_code] = %q, want br_123", got.LastSession["claude_code"])
	}
}

func TestBridgePrefs_InvalidBody(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest("PUT", "/bridge-prefs", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Instance routes (disabled when harness-store is nil)
// ──────────────────────────────────────────────────────────────────────────────

func TestInstances_NotRegistered(t *testing.T) {
	srv, _ := testServer(t) // harness-store is nil

	resp := doJSON(t, srv, "GET", "/instances", nil)
	// Should 404 since routes aren't registered without harness-store
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Errorf("status = %d, want 404 or 405 (routes not registered)", resp.StatusCode)
	}
	resp.Body.Close()
}

// ──────────────────────────────────────────────────────────────────────────────
// Models route (disabled when model-store is nil)
// ──────────────────────────────────────────────────────────────────────────────

func TestModels_NotRegistered(t *testing.T) {
	srv, _ := testServer(t) // model-store is nil

	resp := doJSON(t, srv, "GET", "/models", nil)
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Errorf("status = %d, want 404 or 405 (route not registered)", resp.StatusCode)
	}
	resp.Body.Close()
}

// ──────────────────────────────────────────────────────────────────────────────
// Create session with auto_start (harness not available)
// ──────────────────────────────────────────────────────────────────────────────

func TestCreateSession_AutoStart_HarnessUnavailable(t *testing.T) {
	srv, _ := testServer(t)

	// Use a harness type whose binary is not in PATH
	req := msg.CreateSessionRequest{
		Harness:   "dexto", // stub, not installed
		ClientID:  "fe_1",
		AutoStart: true,
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	// Should fail because binary not found
	if resp.StatusCode != 503 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 503 (harness unavailable): %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// ──────────────────────────────────────────────────────────────────────────────
// Session with harness_config
// ──────────────────────────────────────────────────────────────────────────────

func TestCreateSession_WithHarnessConfig(t *testing.T) {
	srv, _, instID := testServerWithInstance(t, "claude_code")

	req := msg.CreateSessionRequest{
		Harness:       "claude_code",
		InstanceID:    instID,
		ClientID:      "fe_cfg",
		DisplayName:   "Config Test",
		HarnessConfig: json.RawMessage(`{"system_prompt":"test prompt","model":"opus"}`),
	}

	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}

	sess := decodeJSON[msg.ManagedSession](t, resp)
	if string(sess.HarnessConfig) == "" {
		t.Error("harness_config should be preserved")
	}

	var cfg map[string]string
	json.Unmarshal(sess.HarnessConfig, &cfg)
	if cfg["system_prompt"] != "test prompt" {
		t.Errorf("system_prompt = %q, want test prompt", cfg["system_prompt"])
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discover sessions (no harness available)
// ──────────────────────────────────────────────────────────────────────────────

func TestDiscoverSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("discover spawns harness binaries; skipping in short mode")
	}

	srv, _ := testServer(t)

	resp := doJSON(t, srv, "GET", "/sessions/discover", nil)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	// Should return an array (may be empty if no harnesses available)
	var sessions []msg.StoredSession
	sessions = decodeJSON[[]msg.StoredSession](t, resp)
	if sessions == nil {
		t.Error("expected non-nil array")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Content-Type verification
// ──────────────────────────────────────────────────────────────────────────────

func TestResponseContentType(t *testing.T) {
	srv, _ := testServer(t)

	for _, path := range []string{"/health", "/harnesses", "/sessions", "/credentials", "/bridge-prefs"} {
		resp := doJSON(t, srv, "GET", path, nil)
		ct := resp.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("GET %s Content-Type = %q, want application/json", path, ct)
		}
		resp.Body.Close()
	}
}
