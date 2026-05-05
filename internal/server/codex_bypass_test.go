package server

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestInjectCodexBypass_Off_NoChange(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"model":"gpt-5"}`),
	}
	before := string(sess.HarnessConfig)

	srv.injectCodexBypass(sess)

	if string(sess.HarnessConfig) != before {
		t.Fatalf("HarnessConfig mutated with bypass off: before=%s after=%s", before, sess.HarnessConfig)
	}
}

func TestInjectCodexBypass_OnlyAffectsCodex(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessClaudeCode,
		HarnessConfig: json.RawMessage(`{"settings":"{}"}`),
	}
	before := string(sess.HarnessConfig)

	srv.injectCodexBypass(sess)

	if string(sess.HarnessConfig) != before {
		t.Fatalf("non-codex session mutated: before=%s after=%s", before, sess.HarnessConfig)
	}
}

func TestInjectCodexBypass_On_SetsFullAccess(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	sess := &store.Session{
		BridgeID: "b1",
		Harness:  msg.HarnessCodex,
	}

	srv.injectCodexBypass(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v (%s)", err, sess.HarnessConfig)
	}
	if got["permission_mode"] != "never" {
		t.Errorf("permission_mode = %v, want \"never\"", got["permission_mode"])
	}
	if got["sandbox"] != "danger-full-access" {
		t.Errorf("sandbox = %v, want \"danger-full-access\"", got["sandbox"])
	}
}

func TestInjectCodexBypass_On_StripsAutoApprove(t *testing.T) {
	// auto_approve=true would clobber sandbox back to workspace-write inside
	// the codex bridge's HandleStart. Bypass must win, so strip it.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"auto_approve":true,"model":"gpt-5"}`),
	}

	srv.injectCodexBypass(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v", err)
	}
	if _, present := got["auto_approve"]; present {
		t.Errorf("auto_approve should be stripped, got %v", got["auto_approve"])
	}
	if got["sandbox"] != "danger-full-access" {
		t.Errorf("sandbox = %v, want \"danger-full-access\"", got["sandbox"])
	}
	if got["model"] != "gpt-5" {
		t.Errorf("model lost during merge: %v", got["model"])
	}
}

func TestInjectCodexBypass_On_OverridesUserSandbox(t *testing.T) {
	// User-level workspace-write must be overridden by bypass; whole point
	// of the toggle is to escape the sandbox failure mode.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"sandbox":"workspace-write","permission_mode":"on-request"}`),
	}

	srv.injectCodexBypass(sess)

	var got map[string]any
	json.Unmarshal(sess.HarnessConfig, &got)
	if got["sandbox"] != "danger-full-access" {
		t.Errorf("sandbox = %v, want \"danger-full-access\"", got["sandbox"])
	}
	if got["permission_mode"] != "never" {
		t.Errorf("permission_mode = %v, want \"never\"", got["permission_mode"])
	}
}
