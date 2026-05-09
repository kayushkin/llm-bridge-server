package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestCreateSession_SnapshotsBypassFromGlobal exercises the end-to-end
// "session inherits the toggle from settings at creation" behavior.
func TestCreateSession_SnapshotsBypassFromGlobal(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.bridgePrefs.setBypassPermissions(true)

	req := msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
	}
	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	created := decodeJSON[msg.ManagedSession](t, resp)

	// Read back from the store — snapshot must be persisted.
	sess, err := st.GetSession(created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("HarnessConfig unparseable: %v (%s)", err, sess.HarnessConfig)
	}
	if cfg["bypass_permissions"] != true {
		t.Errorf("snapshot at create failed: bypass_permissions = %v", cfg["bypass_permissions"])
	}
}

// TestCreateSession_SnapshotIsDurable_WhenGlobalChanges asserts the key
// "snapshot, then independent" property: changing the global toggle after
// creation must NOT change the existing session's effective value.
func TestCreateSession_SnapshotIsDurable_WhenGlobalChanges(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)
	srv.bridgePrefs.setBypassPermissions(true) // global ON at create

	req := msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
	}
	resp := doJSON(t, srv, "POST", "/sessions", req)
	if resp.StatusCode != 201 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	created := decodeJSON[msg.ManagedSession](t, resp)

	// Flip global OFF after creation. The session's snapshot must hold.
	srv.bridgePrefs.setBypassPermissions(false)

	sess, _ := st.GetSession(created.SessionID)
	if !srv.bypassEnabledForSession(sess) {
		t.Error("session lost its snapshot when global flipped — should still be true")
	}
}

// TestSetSessionBypass_OverridesGlobal exercises the PATCH endpoint and
// confirms the per-session value persists and beats the global toggle.
func TestSetSessionBypass_OverridesGlobal(t *testing.T) {
	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)

	// Create with global OFF (snapshot = false).
	req := msg.CreateSessionRequest{
		Harness:    "claude_code",
		InstanceID: instID,
	}
	resp := doJSON(t, srv, "POST", "/sessions", req)
	created := decodeJSON[msg.ManagedSession](t, resp)

	// Toggle the per-session value to true.
	body, _ := json.Marshal(map[string]any{"enabled": true})
	patch := httptest.NewRequest("PUT", "/sessions/"+created.SessionID+"/bypass-permissions", bytes.NewReader(body))
	patch.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, patch)
	if w.Code != 200 {
		t.Fatalf("PUT status = %d, body = %s", w.Code, w.Body.String())
	}

	// Persisted in store.
	sess, _ := st.GetSession(created.SessionID)
	if !srv.bypassEnabledForSession(sess) {
		t.Error("PATCH true didn't take effect")
	}

	// Per-session true wins even if global stays false.
	if srv.bridgePrefs.get().BypassPermissions {
		t.Fatal("test setup: global must be off for this assertion")
	}
	if !srv.bypassEnabledForSession(sess) {
		t.Error("per-session true must win over global false")
	}

	// Flip per-session back to false.
	body2, _ := json.Marshal(map[string]any{"enabled": false})
	patch2 := httptest.NewRequest("PUT", "/sessions/"+created.SessionID+"/bypass-permissions", bytes.NewReader(body2))
	patch2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, patch2)
	if w2.Code != 200 {
		t.Fatalf("PUT (off) status = %d", w2.Code)
	}

	// Even if global flips ON now, the session's explicit false wins.
	srv.bridgePrefs.setBypassPermissions(true)
	sess, _ = st.GetSession(created.SessionID)
	if srv.bypassEnabledForSession(sess) {
		t.Error("per-session false must win over global true")
	}
}

// TestSetSessionBypass_UnknownSession_404 covers the not-found path.
func TestSetSessionBypass_UnknownSession_404(t *testing.T) {
	srv, _, _ := testServerWithInstance(t, msg.HarnessClaudeCode)
	body, _ := json.Marshal(map[string]any{"enabled": true})
	req := httptest.NewRequest("PUT", "/sessions/br_does_not_exist/bypass-permissions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
