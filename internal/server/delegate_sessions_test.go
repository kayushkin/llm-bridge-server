package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// delegateRequest is what bridge-agent sends to start a delegate.
func delegateRequest(instanceID string) msg.CreateSessionRequest {
	return msg.CreateSessionRequest{
		Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeDelegate, Origin: "bridge-agent",
		Harness: msg.HarnessMock, InstanceID: instanceID,
	}
}

func servedWithPostingToken(t *testing.T, srv *Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	return recorder
}

func TestASessionsAgentStartsADelegateUnderItselfWithItsPostingToken(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	token := currentPostingTokenFor(t, srv, "br_agent")

	recorder := servedWithPostingToken(t, srv, token, http.MethodPost, "/sessions", delegateRequest(instanceID))
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("create delegate: %d %s", recorder.Code, recorder.Body)
	}
	var created store.Session
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ManagerSessionID != "br_agent" {
		t.Errorf("manager_session_id = %q, want br_agent: a delegate sits under the session whose token started it", created.ManagerSessionID)
	}
	if created.PrincipalID != "" {
		t.Errorf("principal_id = %q, want none, as br_agent runs with none", created.PrincipalID)
	}

	// It reads and drives its delegate, and reads itself.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/" + created.SessionID},
		{http.MethodGet, "/sessions/br_agent"},
		{http.MethodPost, "/sessions/" + created.SessionID + "/stop"},
		{http.MethodGet, "/instances"},
	} {
		if code := servedWithPostingToken(t, srv, token, c.method, c.path, nil).Code; code >= 300 {
			t.Errorf("%s %s: %d, want success", c.method, c.path, code)
		}
	}
}

func TestADelegatingAgentReachesNoSessionItDidNotStart(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	newSessionForSignals(t, st, "br_neighbour", msg.SessionTypeInteractive)
	neighboursDelegate := &store.Session{SessionID: "br_neighbours_delegate", Harness: "claude_code", State: "idle",
		Type: msg.SessionTypeAutonomous, ManagerSessionID: "br_neighbour"}
	if err := st.CreateSession(neighboursDelegate); err != nil {
		t.Fatal(err)
	}
	token := currentPostingTokenFor(t, srv, "br_agent")

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/sessions/br_neighbour", nil},
		{http.MethodGet, "/sessions/br_neighbours_delegate", nil},
		{http.MethodPost, "/sessions/br_neighbours_delegate/send", map[string]string{"message": "x"}},
		{http.MethodPost, "/sessions/br_neighbours_delegate/stop", nil},
		{http.MethodGet, "/sessions/br_neighbours_delegate/events", nil},
		// Its own session is not its delegate: it may read it, not drive it.
		{http.MethodPost, "/sessions/br_agent/send", map[string]string{"message": "x"}},
	} {
		if code := servedWithPostingToken(t, srv, token, c.method, c.path, c.body).Code; code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, code)
		}
	}

	// Routes that take no delegate access still refuse the token.
	if code := servedWithPostingToken(t, srv, token, http.MethodGet, "/sessions", nil).Code; code != http.StatusUnauthorized {
		t.Errorf("GET /sessions: %d, want 401", code)
	}

	request := delegateRequest(instanceID)
	request.ManagerSessionID = "br_neighbour"
	recorder := servedWithPostingToken(t, srv, token, http.MethodPost, "/sessions", request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "delegate_manager_mismatch") {
		t.Errorf("delegate under another session: %d %s, want 403 delegate_manager_mismatch", recorder.Code, recorder.Body)
	}

	forged := strings.TrimSuffix(token, token[len(token)-4:]) + "AAAA"
	if code := servedWithPostingToken(t, srv, forged, http.MethodPost, "/sessions", delegateRequest(instanceID)).Code; code != http.StatusUnauthorized {
		t.Errorf("forged token: %d, want 401", code)
	}
}

func TestADelegateRunsWithItsSessionsPrincipalAndNoOther(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	srv.principalClient = principalclient.New(fakePrincipalStore(t, "principal_000001").URL)
	srv.principalLookupCache = newPrincipalLookupCache(srv.principalClient)
	srv.grantClient = grantclient.New(fakeGrantStoreByRelation(t, "principal_000001", map[string][]string{}).URL, "")

	withoutPrincipal := newSessionForSignals(t, st, "br_without_principal", msg.SessionTypeInteractive)
	request := delegateRequest(instanceID)
	request.PrincipalID = "principal_000001"
	recorder := servedWithPostingToken(t, srv, currentPostingTokenFor(t, srv, withoutPrincipal.SessionID), http.MethodPost, "/sessions", request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "delegate_principal_mismatch") {
		t.Fatalf("a principal-less session's agent naming a principal: %d %s, want 403 delegate_principal_mismatch", recorder.Code, recorder.Body)
	}

	withPrincipal := &store.Session{SessionID: "br_with_principal", Harness: "claude_code", State: "idle",
		Type: msg.SessionTypeInteractive, PrincipalID: "principal_000001"}
	if err := st.CreateSession(withPrincipal); err != nil {
		t.Fatal(err)
	}
	recorder = servedWithPostingToken(t, srv, currentPostingTokenFor(t, srv, withPrincipal.SessionID), http.MethodPost, "/sessions", delegateRequest(instanceID))
	if recorder.Code >= 300 {
		t.Fatalf("create delegate: %d %s", recorder.Code, recorder.Body)
	}
	var created store.Session
	json.Unmarshal(recorder.Body.Bytes(), &created)
	if created.PrincipalID != "principal_000001" {
		t.Errorf("principal_id = %q, want principal_000001, the principal of the session that started it", created.PrincipalID)
	}
}

func TestAnEndedSessionsAgentStartsNoDelegate(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	ended := &store.Session{SessionID: "br_ended", Harness: "claude_code", State: string(msg.SessionCompleted), Type: msg.SessionTypeInteractive}
	if err := st.CreateSession(ended); err != nil {
		t.Fatal(err)
	}
	recorder := servedWithPostingToken(t, srv, currentPostingTokenFor(t, srv, "br_ended"), http.MethodPost, "/sessions", delegateRequest(instanceID))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "delegating_session_ended") {
		t.Errorf("create from an ended session: %d %s, want 403 delegating_session_ended", recorder.Code, recorder.Body)
	}
}

// currentPostingTokenFor records a new spawn of the session and returns the
// posting token that spawn's agent would be given.
func currentPostingTokenFor(t *testing.T, srv *Server, sessionID string) string {
	t.Helper()
	spawnKey, err := newSessionPostingSpawnKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.RecordSessionPostingSpawn(sessionID, spawnKey); err != nil {
		t.Fatal(err)
	}
	return srv.harnessChildTokenSigner.encodeSessionPostingToken(sessionID, spawnKey)
}

// TestAPostingTokenFromAnEarlierSpawnIsRefused spawns a session twice and
// checks only the second spawn's token is accepted.
func TestAPostingTokenFromAnEarlierSpawnIsRefused(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	earlier := currentPostingTokenFor(t, srv, "br_agent")
	current := currentPostingTokenFor(t, srv, "br_agent")
	recorder := servedWithPostingToken(t, srv, earlier, http.MethodPost, "/sessions", delegateRequest(instanceID))
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "session_posting_token_from_an_earlier_spawn") {
		t.Fatalf("earlier spawn's token = %d %s, want 401 session_posting_token_from_an_earlier_spawn", recorder.Code, recorder.Body)
	}
	if recorder := servedWithPostingToken(t, srv, current, http.MethodPost, "/sessions", delegateRequest(instanceID)); recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		t.Fatalf("current spawn's token = %d %s", recorder.Code, recorder.Body)
	}
	unrecorded := srv.harnessChildTokenSigner.encodeSessionPostingToken("br_agent", "never-recorded")
	if recorder := servedWithPostingToken(t, srv, unrecorded, http.MethodPost, "/sessions", delegateRequest(instanceID)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a token for a spawn never recorded = %d, want 401", recorder.Code)
	}
}

// TestAnEndedSessionsTokenDrivesNoDelegate checks that once the agent's
// session has ended, its token can no longer read, send to, stop or stream a
// delegate it started.
func TestAnEndedSessionsTokenDrivesNoDelegate(t *testing.T) {
	srv, st, _ := testServerWithInstance(t, msg.HarnessMock)
	ended := &store.Session{SessionID: "br_ended_agent", Harness: "claude_code", State: string(msg.SessionCompleted), Type: msg.SessionTypeInteractive}
	delegate := &store.Session{SessionID: "br_its_delegate", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, ManagerSessionID: "br_ended_agent"}
	for _, session := range []*store.Session{ended, delegate} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	token := currentPostingTokenFor(t, srv, "br_ended_agent")
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/br_its_delegate"},
		{http.MethodPost, "/sessions/br_its_delegate/send"},
		{http.MethodPost, "/sessions/br_its_delegate/stop"},
		{http.MethodGet, "/sessions/br_its_delegate/events"},
	} {
		var body any
		if route.method == http.MethodPost {
			body = map[string]any{"content": "after the end"}
		}
		recorder := servedWithPostingToken(t, srv, token, route.method, route.path, body)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "delegating_session_ended") {
			t.Errorf("%s %s after the agent's session ended = %d %s, want 403 delegating_session_ended", route.method, route.path, recorder.Code, recorder.Body)
		}
	}
}
