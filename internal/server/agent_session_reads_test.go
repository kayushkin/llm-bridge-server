package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// authorizationPassed reports whether a request got past authorization: its
// handler answered, whatever it then said. A handler here may answer 502 for a
// log-store the test does not run, which is still past the gate.
func authorizationPassed(code int) bool {
	return code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusNotFound
}

func sessionIDsIn(t *testing.T, recorder *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /sessions: %d %s", recorder.Code, recorder.Body)
	}
	var sessions []store.Session
	if err := json.Unmarshal(recorder.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode session list: %v: %s", err, recorder.Body)
	}
	ids := map[string]bool{}
	for _, session := range sessions {
		ids[session.SessionID] = true
	}
	return ids
}

// On a server one operator runs alone, sessions carry no principal, and an
// agent reads every session the way the operator's own tools do: the list,
// search, one session and its history. It still sends to, stops and streams
// nothing but its own delegates.
func TestAPrincipallessSessionsAgentReadsEverySessionAndDrivesNone(t *testing.T) {
	srv, st, _ := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	newSessionForSignals(t, st, "br_neighbour", msg.SessionTypeInteractive)
	token := currentPostingTokenFor(t, srv, "br_agent")

	listed := sessionIDsIn(t, servedWithPostingToken(t, srv, token, http.MethodGet, "/sessions", nil))
	if !listed["br_agent"] || !listed["br_neighbour"] {
		t.Errorf("listed %v, want both sessions", listed)
	}
	for _, path := range []string{
		"/sessions/br_neighbour",
		"/sessions/br_neighbour/messages",
		"/sessions/search?q=anything",
	} {
		if code := servedWithPostingToken(t, srv, token, http.MethodGet, path, nil).Code; !authorizationPassed(code) {
			t.Errorf("GET %s: %d, want past authorization", path, code)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/sessions/br_neighbour/send", map[string]string{"message": "x"}},
		{http.MethodPost, "/sessions/br_neighbour/stop", nil},
		{http.MethodGet, "/sessions/br_neighbour/events", nil},
	} {
		if code := servedWithPostingToken(t, srv, token, c.method, c.path, c.body).Code; code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404: reading is not driving", c.method, c.path, code)
		}
	}
	// A read route that takes no agent access still refuses the token.
	if code := servedWithPostingToken(t, srv, token, http.MethodGet, "/sessions/br_neighbour/effective-config", nil).Code; code != http.StatusUnauthorized {
		t.Errorf("GET effective-config: %d, want 401", code)
	}
}

// The posting token is for this server's own harness children. Through the
// public reverse proxy it reads nothing.
func TestAnAgentReadsNoSessionThroughThePublicProxy(t *testing.T) {
	srv, st, _ := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	token := currentPostingTokenFor(t, srv, "br_agent")
	for _, header := range publicProxyHeaders {
		request := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set(header, "203.0.113.7")
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "session_posting_token_not_public") {
			t.Errorf("with %s: %d %s, want 403 session_posting_token_not_public", header, recorder.Code, recorder.Body)
		}
	}
}

// A token from an earlier spawn, or of a session that has ended, reads
// nothing, as on the delegate routes.
func TestAStaleOrEndedPostingTokenReadsNoSession(t *testing.T) {
	srv, st, _ := testServerWithInstance(t, msg.HarnessMock)
	newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	earlier := currentPostingTokenFor(t, srv, "br_agent")
	currentPostingTokenFor(t, srv, "br_agent") // a later spawn
	recorder := servedWithPostingToken(t, srv, earlier, http.MethodGet, "/sessions", nil)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "session_posting_token_from_an_earlier_spawn") {
		t.Errorf("earlier spawn's token: %d %s, want 401 session_posting_token_from_an_earlier_spawn", recorder.Code, recorder.Body)
	}

	ended := &store.Session{SessionID: "br_ended_reader", Harness: "claude_code", State: string(msg.SessionCompleted), Type: msg.SessionTypeInteractive}
	if err := st.CreateSession(ended); err != nil {
		t.Fatal(err)
	}
	recorder = servedWithPostingToken(t, srv, currentPostingTokenFor(t, srv, ended.SessionID), http.MethodGet, "/sessions", nil)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "delegating_session_ended") {
		t.Errorf("ended session's token: %d %s, want 403 delegating_session_ended", recorder.Code, recorder.Body)
	}
}

// A session with a principal reads as that principal: its own sessions, no
// one else's, and no search, which cannot be narrowed to one principal. An
// administrator reads everything. A session started through a connected
// server that is not an administrator reads as that server's mapped user,
// never as a local administrator.
func TestASessionsAgentReadsAsItsSessionsPrincipal(t *testing.T) {
	accepting := newPeerTestServer(t, false)
	const ordinary = "principal_000801"
	const administrator = "principal_000802"
	const peerID = "principal_000803"
	accepting.directory.addPrincipal(ordinary, "human", false)
	accepting.directory.addPrincipal(administrator, "human", true)
	accepting.directory.addPrincipal(peerID, "peer", false)
	srv, st := accepting.server, accepting.server.store
	for _, session := range []*store.Session{
		{SessionID: "br_ordinary_agent", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: ordinary},
		{SessionID: "br_ordinary_other", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: ordinary},
		{SessionID: "br_administrators", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: administrator},
		{SessionID: "br_through_peer", Harness: "claude_code", State: string(msg.SessionIdle), Type: msg.SessionTypeInteractive, PrincipalID: administrator},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordSessionPeerOrigin(store.SessionPeerOrigin{SessionID: "br_through_peer", PeerPrincipalID: peerID, RemotePrincipalID: "principal_000804"}); err != nil {
		t.Fatal(err)
	}

	ordinaryToken := currentPostingTokenFor(t, srv, "br_ordinary_agent")
	listed := sessionIDsIn(t, servedWithPostingToken(t, srv, ordinaryToken, http.MethodGet, "/sessions", nil))
	if !listed["br_ordinary_other"] || listed["br_administrators"] || listed["br_through_peer"] {
		t.Errorf("an ordinary principal's agent listed %v, want its principal's sessions only", listed)
	}
	if code := servedWithPostingToken(t, srv, ordinaryToken, http.MethodGet, "/sessions/br_ordinary_other", nil).Code; code != http.StatusOK {
		t.Errorf("its principal's other session: %d, want 200", code)
	}
	for _, path := range []string{"/sessions/br_administrators", "/sessions/br_administrators/messages"} {
		if code := servedWithPostingToken(t, srv, ordinaryToken, http.MethodGet, path, nil).Code; code != http.StatusNotFound {
			t.Errorf("GET %s as an ordinary principal's agent: %d, want 404", path, code)
		}
	}
	recorder := servedWithPostingToken(t, srv, ordinaryToken, http.MethodGet, "/sessions/search?q=x", nil)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "route_not_available_to_principals") {
		t.Errorf("search as an ordinary principal's agent: %d %s, want 403 route_not_available_to_principals", recorder.Code, recorder.Body)
	}

	administratorToken := currentPostingTokenFor(t, srv, "br_administrators")
	listed = sessionIDsIn(t, servedWithPostingToken(t, srv, administratorToken, http.MethodGet, "/sessions", nil))
	if !listed["br_ordinary_agent"] || !listed["br_through_peer"] {
		t.Errorf("an administrator's agent listed %v, want every session", listed)
	}
	if code := servedWithPostingToken(t, srv, administratorToken, http.MethodGet, "/sessions/search?q=x", nil).Code; !authorizationPassed(code) {
		t.Errorf("search as an administrator's agent: %d, want past authorization", code)
	}

	peerToken := currentPostingTokenFor(t, srv, "br_through_peer")
	for _, path := range []string{"/sessions/br_ordinary_agent", "/sessions/br_administrators"} {
		if code := servedWithPostingToken(t, srv, peerToken, http.MethodGet, path, nil).Code; code != http.StatusNotFound {
			t.Errorf("GET %s as the agent of a session a non-administrator peer started: %d, want 404", path, code)
		}
	}
	recorder = servedWithPostingToken(t, srv, peerToken, http.MethodGet, "/sessions/search?q=x", nil)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("search as the agent of a peer-started session: %d %s, want 403", recorder.Code, recorder.Body)
	}
}
