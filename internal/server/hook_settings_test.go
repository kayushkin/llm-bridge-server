package server

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// seedHook is a small helper because the test-only hook-store API requires
// passing every field; the helper lets each test assert the synthesis
// behavior without boilerplate.
func seedHook(t *testing.T, hks *hookstore.Store, id, event, matcher string, scope msg.HookScope, scopeID string) {
	t.Helper()
	h := &msg.Hook{
		ID:        id,
		Harness:   msg.HarnessClaudeCode,
		Event:     event,
		Matcher:   matcher,
		Command:   ":",
		ScopeKind: scope,
		ScopeID:   scopeID,
		Enabled:   true,
	}
	if err := hks.CreateHook(h); err != nil {
		t.Fatalf("create hook %s: %v", id, err)
	}
}

// parseSettings round-trips the synthesized string back to a nested map.
// The "settings" layer of wrapping is what CC's --settings expects (inline
// JSON), so tests assert against the inner structure to stay robust to
// string-encoding changes.
func parseSettings(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse settings: %v (raw: %s)", err, raw)
	}
	return out
}

func TestBuildCCSettings_PermissionHookPresentWithNoHookEntries(t *testing.T) {
	// With an OPEN hook-store holding no entries, the permission gate must
	// still be wired so every CC tool call routes through
	// /permission/cc-prehook.
	//
	// Scope: this drives the helper, so it pins the helper only. It says
	// nothing about whether a spawn reaches the helper at all — that is the
	// caller's property and lives in
	// TestInjectHookSettings_NilHookStoreKeepsTheClaudeCodePermissionGate.
	// This test was named "...AlwaysPresent" for months and read as though it
	// covered both; it never called the caller.
	srv, _ := testServerWithHookStore(t)
	got, err := srv.buildClaudeCodeSettings(&store.Session{SessionID: "b1", Harness: msg.HarnessClaudeCode})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got == "" {
		t.Fatalf("expected permission hook in settings, got empty")
	}
	parsed := parseSettings(t, got)
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks key missing: %v", parsed)
	}
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok || len(pre) != 1 {
		t.Fatalf("expected one PreToolUse entry (the permission hook), got %v", hooks["PreToolUse"])
	}
	entry := pre[0].(map[string]any)
	if entry["matcher"] != "*" {
		t.Errorf("permission hook matcher = %v, want \"*\"", entry["matcher"])
	}
	inner := entry["hooks"].([]any)[0].(map[string]any)
	if inner["type"] != "http" {
		t.Errorf("permission hook type = %v, want \"http\"", inner["type"])
	}
	url, _ := inner["url"].(string)
	if !strings.Contains(url, "/permission/cc-prehook/b1") {
		t.Errorf("permission hook url = %q, want containing /permission/cc-prehook/b1", url)
	}
	// timeout must be present and large enough that human approval flows
	// can complete; the JSON number type round-trips as float64.
	if to, ok := inner["timeout"].(float64); !ok || to < 3600 {
		t.Errorf("permission hook timeout = %v, want >= 3600s", inner["timeout"])
	}
}

func TestBuildCCSettings_EmptyWhenNoBridgeID(t *testing.T) {
	// Defensive: a session without a BridgeID can't be permission-gated
	// (we'd have no id to put in the URL). Fall back to the previous
	// "empty when no user hooks" behavior so tests / utilities calling
	// this helper without a real session don't crash.
	srv, _ := testServerWithHookStore(t)
	got, err := srv.buildClaudeCodeSettings(&store.Session{Harness: msg.HarnessClaudeCode})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty when BridgeID missing, got %q", got)
	}
}

func TestBuildCCSettings_GlobalAndInstanceMerged(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	seedHook(t, hks, "g1", "PreToolUse", "Edit|Write", msg.HookScopeGlobal, "")
	seedHook(t, hks, "i1", "PostToolUse", "Edit|Write", msg.HookScopeInstance, "inst_x")
	// Not applicable — different instance.
	seedHook(t, hks, "i2", "PreToolUse", "Bash", msg.HookScopeInstance, "inst_y")

	sess := &store.Session{SessionID: "b1", InstanceID: "inst_x", Harness: msg.HarnessClaudeCode}
	raw, err := srv.buildClaudeCodeSettings(sess)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if raw == "" {
		t.Fatal("expected settings, got empty")
	}
	parsed := parseSettings(t, raw)
	hooks, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks missing: %v", parsed)
	}
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("PreToolUse absent")
	}
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("PostToolUse absent")
	}
	// inst_y hook must not leak in.
	for _, evArr := range hooks {
		entries, _ := evArr.([]any)
		for _, e := range entries {
			m, _ := e.(map[string]any)
			if m["matcher"] == "Bash" {
				t.Errorf("instance-scoped hook for a different instance leaked into settings")
			}
		}
	}
}

func TestBuildCCSettings_CommandPointsAtExecEndpoint(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	seedHook(t, hks, "abc123", "PreToolUse", "Edit|Write", msg.HookScopeGlobal, "")

	raw, err := srv.buildClaudeCodeSettings(&store.Session{SessionID: "b1", Harness: msg.HarnessClaudeCode})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(raw, "/hooks/exec/abc123") {
		t.Errorf("command should reference exec endpoint: %s", raw)
	}
	if !strings.Contains(raw, "--data-binary @-") {
		t.Errorf("command should pipe stdin: %s", raw)
	}
}

func TestInjectHookSettings_RespectsUserOverride(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	seedHook(t, hks, "g1", "PreToolUse", "Edit|Write", msg.HookScopeGlobal, "")

	sess := &store.Session{
		SessionID:     "b1",
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: []byte(`{"settings":"/path/to/user.json"}`),
	}
	srv.injectHookSettings(sess)

	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg["settings"] != "/path/to/user.json" {
		t.Errorf("user override clobbered: %v", cfg["settings"])
	}
}

func TestInjectHookSettings_WritesSettingsStringForStartParams(t *testing.T) {
	srv, hks := testServerWithHookStore(t)
	seedHook(t, hks, "g1", "PreToolUse", "Edit|Write", msg.HookScopeGlobal, "")

	sess := &store.Session{SessionID: "b1", Harness: msg.HarnessClaudeCode}
	srv.injectHookSettings(sess)

	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	settingsStr, ok := cfg["settings"].(string)
	if !ok {
		t.Fatalf("settings should be a JSON string value, got %T", cfg["settings"])
	}
	if !strings.Contains(settingsStr, "PreToolUse") {
		t.Errorf("synthesized settings should contain PreToolUse: %s", settingsStr)
	}
}

// testServerWithoutHookStore builds a Server whose hookStore is nil — the
// state main() leaves behind when hookstore.Open fails and it logs
// "continuing without hook registry", and the state an empty
// LLMBRIDGE_HOOK_DB produces.
func testServerWithoutHookStore(t *testing.T) *Server {
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
		LogStoreURL:     "http://localhost:0",
	}
	srv := New(st, nil, nil, nil, nil, nil, nil, cfg)
	if srv.hookStore != nil {
		t.Fatalf("precondition: hookStore should be nil")
	}
	return srv
}

func TestInjectHookSettings_NilHookStoreKeepsTheClaudeCodePermissionGate(t *testing.T) {
	// The permission gate is built from the session id alone and never reads
	// the hook registry. So losing the registry must not lose the gate: a
	// hook-store outage is a hook-store outage, not an unguarded harness.
	srv := testServerWithoutHookStore(t)

	sess := &store.Session{SessionID: "b1", Harness: msg.HarnessClaudeCode}
	srv.injectHookSettings(sess)

	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal HarnessConfig (%q): %v", sess.HarnessConfig, err)
	}
	settingsStr, ok := cfg["settings"].(string)
	if !ok {
		t.Fatalf("settings should be a JSON string value, got %T (%v)", cfg["settings"], cfg)
	}
	if !strings.Contains(settingsStr, "/permission/cc-prehook/b1") {
		t.Errorf("permission gate missing from settings with a nil hook-store: %s", settingsStr)
	}
}

func TestInjectHookSettings_NilHookStoreKeepsTheCodexPermissionGate(t *testing.T) {
	// Same property on the codex path, which shares the caller's early return
	// and so shared the defect.
	srv := testServerWithoutHookStore(t)

	sess := &store.Session{SessionID: "b2", Harness: msg.HarnessCodex}
	srv.injectHookSettings(sess)

	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal HarnessConfig (%q): %v", sess.HarnessConfig, err)
	}
	raw, ok := cfg["codex_hooks"]
	if !ok {
		t.Fatalf("codex_hooks missing entirely with a nil hook-store: %v", cfg)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal codex_hooks: %v", err)
	}
	if !strings.Contains(string(encoded), "/permission/codex-prehook/b2") {
		t.Errorf("permission gate missing from codex_hooks with a nil hook-store: %s", encoded)
	}
}

func TestInjectHookSettings_NilHookStoreStillWritesNothingWithoutASessionID(t *testing.T) {
	// Control. Dropping the nil-store guard must not make the injector write
	// unconditionally: with no session id there is no gate URL to build, so
	// HarnessConfig must be left exactly as it was. Without this, the two
	// tests above would also pass on an injector that wrote a malformed gate
	// for every session.
	srv := testServerWithoutHookStore(t)

	sess := &store.Session{SessionID: "", Harness: msg.HarnessClaudeCode}
	srv.injectHookSettings(sess)

	if len(sess.HarnessConfig) != 0 {
		t.Errorf("HarnessConfig should stay empty when there is no session id, got %q", sess.HarnessConfig)
	}
}

func TestInjectHookSettings_NilHookStoreStillRespectsAUserOverride(t *testing.T) {
	// Control. The user-override branch sits after the guard that moved, so
	// pin that it still wins when the registry is gone.
	srv := testServerWithoutHookStore(t)

	sess := &store.Session{
		SessionID:     "b1",
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: []byte(`{"settings":"/path/to/user.json"}`),
	}
	srv.injectHookSettings(sess)

	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg["settings"] != "/path/to/user.json" {
		t.Errorf("user override clobbered: %v", cfg["settings"])
	}
}

func TestPublicBaseURL(t *testing.T) {
	cases := map[string]string{
		":8160":                 "http://localhost:8160",
		"0.0.0.0:8160":          "http://0.0.0.0:8160",
		"http://bridge:8160":    "http://bridge:8160",
		"https://bridge.local/": "https://bridge.local",
	}
	for in, want := range cases {
		if got := publicBaseURL(in); got != want {
			t.Errorf("publicBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
