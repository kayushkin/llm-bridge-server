package server

import (
	"encoding/json"
	"strings"
	"testing"

	hookstore "github.com/kayushkin/hook-store"
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

func TestBuildCCSettings_PermissionHookAlwaysPresent(t *testing.T) {
	// Even with no hook-store entries, the permission gate must be wired
	// so every CC tool call routes through /permission/cc-prehook.
	srv, _ := testServerWithHookStore(t)
	got, err := srv.buildClaudeCodeSettings(&store.Session{BridgeID: "b1", Harness: msg.HarnessClaudeCode})
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

	sess := &store.Session{BridgeID: "b1", InstanceID: "inst_x", Harness: msg.HarnessClaudeCode}
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

	raw, err := srv.buildClaudeCodeSettings(&store.Session{BridgeID: "b1", Harness: msg.HarnessClaudeCode})
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
		BridgeID:      "b1",
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

	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessClaudeCode}
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
