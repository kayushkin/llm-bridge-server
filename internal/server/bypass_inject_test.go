package server

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestInjectBypassFlag_Off_NoChange(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"model":"gpt-5"}`),
	}
	before := string(sess.HarnessConfig)

	srv.injectBypassFlag(sess)

	if string(sess.HarnessConfig) != before {
		t.Fatalf("HarnessConfig mutated with bypass off: before=%s after=%s", before, sess.HarnessConfig)
	}
}

func TestInjectBypassFlag_On_SetsFlag(t *testing.T) {
	// Harness-agnostic: every harness gets the same canonical flag. The
	// translation to harness-specific behavior lives in each harness bridge.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	for _, h := range []msg.Harness{msg.HarnessCodex, msg.HarnessClaudeCode, msg.Harness("inber")} {
		sess := &store.Session{BridgeID: "b1", Harness: h}
		srv.injectBypassFlag(sess)
		var got map[string]any
		if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
			t.Fatalf("HarnessConfig unparseable for %s: %v (%s)", h, err, sess.HarnessConfig)
		}
		if got["bypass_permissions"] != true {
			t.Errorf("harness=%s: bypass_permissions = %v, want true", h, got["bypass_permissions"])
		}
	}
}

func TestInjectBypassFlag_PreservesOtherKeys(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)

	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"model":"gpt-5","sandbox":"workspace-write"}`),
	}

	srv.injectBypassFlag(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v", err)
	}
	if got["bypass_permissions"] != true {
		t.Errorf("bypass_permissions = %v, want true", got["bypass_permissions"])
	}
	if got["model"] != "gpt-5" {
		t.Errorf("model lost during merge: %v", got["model"])
	}
	// Pre-existing harness fields are NOT overridden — bridge-server is
	// transparent. The harness bridge decides whether the bypass flag wins
	// over its other fields.
	if got["sandbox"] != "workspace-write" {
		t.Errorf("sandbox unexpectedly mutated: %v", got["sandbox"])
	}
}
