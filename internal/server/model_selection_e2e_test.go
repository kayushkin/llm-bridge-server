package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// These tests are the one check that would have caught the original defect.
// They drive the REAL server against the mock harness and read back the model
// the harness reports it was started with. Before this change nothing anywhere
// asserted that the model a session was given was the model its process ran on:
// every layer was green, and the seam between them was where fable sessions
// became opus. The mock echoes exactly what it was sent, so an empty or wrong
// model here is a real gap, not a stand-in.

// stubLogStore answers every request 200 so the /send path, which pushes the
// user message to log-store before spawning, does not fail on a missing peer.
func stubLogStore(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitForReportedModel polls the row until the harness's session_info has
// landed and names a model, then returns it.
func waitForReportedModel(t *testing.T, st *store.Store, bridgeID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(bridgeID)
		if err == nil && sess != nil && sess.Info != nil && sess.Info.Model != "" {
			return sess.Info.Model
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never reported a model within %s — the harness started without one, which is the original defect", bridgeID, timeout)
	return ""
}

func autoStartWithConfig(instID, cfg string) msg.CreateSessionRequest {
	req := autoStartRequest(instID) // create_autostart_test.go
	req.HarnessConfig = json.RawMessage(cfg)
	return req
}

func TestAutoStartSpawnsOnTheModelTheSessionWasGiven(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessMock, stubLogStore(t))
	got := postCreateSession(t, srv, autoStartWithConfig(instID, `{"model":"mock-model-alt"}`))
	t.Cleanup(func() { srv.harness.Kill(got.SessionID) })
	if m := waitForReportedModel(t, st, got.SessionID, 5*time.Second); m != "mock-model-alt" {
		t.Fatalf("harness reports model %q; the session was given mock-model-alt", m)
	}
}

func TestAutoStartWithNoModelSpawnsOnTheRegistryDefaultNotAnAccountDefault(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessMock, stubLogStore(t))
	got := postCreateSession(t, srv, autoStartWithConfig(instID, ``))
	t.Cleanup(func() { srv.harness.Kill(got.SessionID) })
	// The defect: no model → no --model → whatever the harness's account default
	// happened to be. Now the registry's default role is decided here and SENT.
	if m := waitForReportedModel(t, st, got.SessionID, 5*time.Second); m != "mock-model" {
		t.Fatalf("harness reports model %q; want the registry default mock-model", m)
	}
}

func TestPrefsDefaultReachesTheSpawn(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessMock, stubLogStore(t))
	// The production shape: dash /settings writes bridge-prefs defaults[harness].model.
	// This is the tier that ends the client race — the create itself answers.
	srv.bridgePrefs.mu.Lock()
	srv.bridgePrefs.data.Defaults = map[string]msg.HarnessDefaults{string(msg.HarnessMock): {Model: "mock-model-alt"}}
	srv.bridgePrefs.mu.Unlock()
	got := postCreateSession(t, srv, autoStartWithConfig(instID, ``))
	t.Cleanup(func() { srv.harness.Kill(got.SessionID) })
	if m := waitForReportedModel(t, st, got.SessionID, 5*time.Second); m != "mock-model-alt" {
		t.Fatalf("harness reports model %q; want the bridge-prefs default mock-model-alt", m)
	}
	row, _ := st.GetSession(got.SessionID)
	if _, sel := pinnedModel(t, row.HarnessConfig); sel.SelectedBy != msg.ModelSelectedByPrefs {
		t.Fatalf("selection = %+v; want selected_by=prefs so the row can say WHY it is on this model", sel)
	}
}

func TestConfigThenSendSpawnsOnTheConfiguredModel(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstanceAndLogStore(t, msg.HarnessMock, stubLogStore(t))
	// The browser's ordering: create, configure, then the SEND spawns the process.
	req := autoStartRequest(instID)
	req.AutoStart = false
	got := postCreateSession(t, srv, req)
	t.Cleanup(func() { srv.harness.Kill(got.SessionID) })
	if resp := doJSON(t, srv, "POST", "/sessions/"+got.SessionID+"/config", msg.ConfigSessionRequest{Model: "efficient"}); resp.StatusCode != 200 {
		t.Fatalf("config: %d", resp.StatusCode)
	}
	if resp := doJSON(t, srv, "POST", "/sessions/"+got.SessionID+"/send", msg.SendMessageRequest{Message: "hello"}); resp.StatusCode/100 != 2 {
		t.Fatalf("send: %d", resp.StatusCode)
	}
	if m := waitForReportedModel(t, st, got.SessionID, 5*time.Second); m != "mock-model-alt" {
		t.Fatalf("harness reports model %q; the session was configured to the efficient role (mock-model-alt) before the send spawned it", m)
	}
	row, _ := st.GetSession(got.SessionID)
	if _, sel := pinnedModel(t, row.HarnessConfig); sel.Role != msg.ModelRoleEfficient || sel.SelectedBy != msg.ModelSelectedBySession {
		t.Fatalf("selection = %+v; want role=efficient selected_by=session", sel)
	}
}
