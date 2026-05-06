package server

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// --- snapshotBypassIntoSession ---------------------------------------------

func TestSnapshotBypass_GlobalOn_PinsTrue(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)
	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessCodex}

	srv.snapshotBypassIntoSession(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v (%s)", err, sess.HarnessConfig)
	}
	if got["bypass_permissions"] != true {
		t.Errorf("bypass_permissions = %v, want true", got["bypass_permissions"])
	}
}

func TestSnapshotBypass_GlobalOff_PinsFalse(t *testing.T) {
	// Global=off must still record `false` so the session is fully
	// independent — flipping the global later must not silently turn
	// bypass on for an existing session.
	srv, _ := testServerWithHookStore(t)
	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessCodex}

	srv.snapshotBypassIntoSession(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v", err)
	}
	if got["bypass_permissions"] != false {
		t.Errorf("bypass_permissions = %v, want false", got["bypass_permissions"])
	}
}

func TestSnapshotBypass_CallerValueWins(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)
	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"bypass_permissions":false}`),
	}

	srv.snapshotBypassIntoSession(sess)

	var got map[string]any
	json.Unmarshal(sess.HarnessConfig, &got)
	if got["bypass_permissions"] != false {
		t.Errorf("caller value clobbered: %v", got["bypass_permissions"])
	}
}

// --- bypassEnabledForSession -----------------------------------------------

func TestBypassEnabled_PerSessionWins(t *testing.T) {
	srv, _ := testServerWithHookStore(t)
	// Global ON, session OFF — session value must win.
	srv.bridgePrefs.setBypassPermissions(true)
	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"bypass_permissions":false}`),
	}
	if srv.bypassEnabledForSession(sess) {
		t.Error("per-session false must win over global true")
	}

	// Global OFF, session ON — session value must win.
	srv.bridgePrefs.setBypassPermissions(false)
	sess.HarnessConfig = json.RawMessage(`{"bypass_permissions":true}`)
	if !srv.bypassEnabledForSession(sess) {
		t.Error("per-session true must win over global false")
	}
}

func TestBypassEnabled_LegacyFallsBackToGlobal(t *testing.T) {
	// Session has no bypass_permissions in HarnessConfig (legacy session
	// from before the snapshot existed). Effective value comes from global.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)
	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessCodex}

	if !srv.bypassEnabledForSession(sess) {
		t.Error("legacy session must inherit global=true")
	}

	srv.bridgePrefs.setBypassPermissions(false)
	if srv.bypassEnabledForSession(sess) {
		t.Error("legacy session must inherit global=false")
	}
}

// --- injectBypassFlag (start-time wiring) -----------------------------------

func TestInjectBypassFlag_Snapshotted_NoOp(t *testing.T) {
	// Sessions created via the new path already carry their snapshot —
	// inject must not overwrite.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true) // would clobber if inject misbehaved
	sess := &store.Session{
		BridgeID:      "b1",
		Harness:       msg.HarnessCodex,
		HarnessConfig: json.RawMessage(`{"bypass_permissions":false}`),
	}
	before := string(sess.HarnessConfig)

	srv.injectBypassFlag(sess)

	if string(sess.HarnessConfig) != before {
		t.Errorf("snapshotted value mutated: before=%s after=%s", before, sess.HarnessConfig)
	}
}

func TestInjectBypassFlag_Legacy_GlobalOn_InjectsTrue(t *testing.T) {
	// Pre-snapshot session with no key. Global on → inject true so the
	// harness still gets a definite signal at start.
	srv, _ := testServerWithHookStore(t)
	srv.bridgePrefs.setBypassPermissions(true)
	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessCodex}

	srv.injectBypassFlag(sess)

	var got map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &got); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v", err)
	}
	if got["bypass_permissions"] != true {
		t.Errorf("legacy fallback failed: bypass_permissions = %v", got["bypass_permissions"])
	}
}

func TestInjectBypassFlag_Legacy_GlobalOff_LeavesEmpty(t *testing.T) {
	// Pre-snapshot session, global off → don't write anything; harness
	// receives no flag, which is the default behavior we want for old
	// sessions when bypass is genuinely off.
	srv, _ := testServerWithHookStore(t)
	sess := &store.Session{BridgeID: "b1", Harness: msg.HarnessCodex}

	srv.injectBypassFlag(sess)

	if len(sess.HarnessConfig) != 0 {
		t.Errorf("HarnessConfig should stay empty, got %s", sess.HarnessConfig)
	}
}
