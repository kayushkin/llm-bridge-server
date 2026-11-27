package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func (gated *gatedTestServer) sessionAgentTokenFor(t *testing.T, session *store.Session) string {
	t.Helper()
	environment, err := gated.server.sessionAgentEnvironment(session)
	if err != nil {
		t.Fatalf("sessionAgentEnvironment: %v", err)
	}
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, principalTokenEnvironmentVariable+"="); found {
			return value
		}
	}
	t.Fatalf("no %s in %v", principalTokenEnvironmentVariable, environment)
	return ""
}

func (gated *gatedTestServer) requestWithBearer(t *testing.T, token, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return serve(gated.server, request)
}

func TestSessionAgentTokenActsAsThePrincipalOnTheKanbanProxyOnly(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	session := gated.createSessionAs(t, cookie, "")
	token := gated.sessionAgentTokenFor(t, session)

	proxied := gated.requestWithBearer(t, token, "GET", "/issues/api/boards")
	if proxied.Code != http.StatusOK {
		t.Fatalf("token on /issues/api/boards = %d %s, want 200", proxied.Code, proxied.Body.String())
	}
	recorded := gated.kanban.recorded()
	if len(recorded) != 1 {
		t.Fatalf("kanban-store received %d requests, want 1", len(recorded))
	}
	if got := recorded[0].principalIDsSeen(); len(got) != 1 || got[0] != firstTestPrincipalID {
		t.Errorf("kanban-store saw X-Principal-Id %v, want [%s]", got, firstTestPrincipalID)
	}
	for _, key := range recorded[0].HeaderKeyNames {
		if strings.EqualFold(key, "Authorization") {
			t.Errorf("the session agent token was forwarded to kanban-store")
		}
	}

	for _, route := range []struct{ method, path string }{
		{"GET", "/sessions"},
		{"GET", "/sessions/" + session.SessionID},
		{"POST", "/sessions/" + session.SessionID + "/send"},
		{"GET", "/auth/principal"},
		{"GET", "/bridge-prefs"},
	} {
		response := gated.requestWithBearer(t, token, route.method, route.path)
		if route.path == "/auth/principal" {
			// Open route: it reads only the cookie, so the token logs nobody in.
			if response.Code != http.StatusUnauthorized {
				t.Errorf("token on %s = %d, want 401", route.path, response.Code)
			}
			continue
		}
		if response.Code != http.StatusUnauthorized {
			t.Errorf("token on %s %s = %d %s, want 401", route.method, route.path, response.Code, response.Body.String())
		}
	}
}

func TestSessionAgentTokenDiesWithItsSession(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))

	stopped := gated.createSessionAs(t, cookie, "")
	stoppedToken := gated.sessionAgentTokenFor(t, stopped)
	if response := gated.requestAs(t, cookie, "POST", "/sessions/"+stopped.SessionID+"/stop", nil); response.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", response.Code, response.Body.String())
	}
	if response := gated.requestWithBearer(t, stoppedToken, "GET", "/issues/api/boards"); response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "has ended") {
		t.Errorf("token of a stopped session = %d %s, want 401 naming the ended session", response.Code, response.Body.String())
	}

	deleted := gated.createSessionAs(t, cookie, "")
	deletedToken := gated.sessionAgentTokenFor(t, deleted)
	if err := gated.store.DeleteSession(deleted.SessionID); err != nil {
		t.Fatal(err)
	}
	if response := gated.requestWithBearer(t, deletedToken, "GET", "/issues/api/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("token of a deleted session = %d %s, want 401", response.Code, response.Body.String())
	}
	if got := gated.kanban.recorded(); len(got) != 0 {
		t.Fatalf("kanban-store received %d requests from dead tokens", len(got))
	}
}

func TestSessionAgentTokensAndLoginCookiesAreNotInterchangeable(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	session := gated.createSessionAs(t, cookie, "")
	token := gated.sessionAgentTokenFor(t, session)
	codec := gated.server.harnessChildTokenSigner

	if response := gated.requestWithBearer(t, cookie.Value, "GET", "/issues/api/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("login cookie value as a bearer token = %d, want 401", response.Code)
	}
	tokenAsCookie := httptest.NewRequest("GET", "/sessions", nil)
	tokenAsCookie.AddCookie(&http.Cookie{Name: loginSessionCookieName, Value: token})
	if response := serve(gated.server, tokenAsCookie); response.Code != http.StatusUnauthorized {
		t.Errorf("session agent token as the login cookie = %d, want 401", response.Code)
	}

	// A session posting token is signed with the same key under another
	// domain separator, and is not a session agent token.
	if _, err := codec.decodeSessionAgentToken(codec.encodeSessionPostingToken(session.SessionID, "spawn_1")); err == nil {
		t.Errorf("a session posting token verified as a session agent token")
	}

	expired := codec.encodeSessionAgentToken(sessionAgentTokenClaims{PrincipalID: firstTestPrincipalID, SessionID: session.SessionID, ExpiresAt: time.Now().Add(-time.Minute)})
	if response := gated.requestWithBearer(t, expired, "GET", "/issues/api/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("expired token = %d, want 401", response.Code)
	}
	otherPrincipal := codec.encodeSessionAgentToken(sessionAgentTokenClaims{PrincipalID: secondTestPrincipalID, SessionID: session.SessionID, ExpiresAt: time.Now().Add(time.Hour)})
	if response := gated.requestWithBearer(t, otherPrincipal, "GET", "/issues/api/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("token naming a principal the session is not started as = %d, want 401", response.Code)
	}
}

// writeEnvironmentRecordingMockHarness puts an executable named like the mock
// harness first on PATH; it records its environment to the returned file and
// then reads stdin until killed.
func writeEnvironmentRecordingMockHarness(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	recordTo := filepath.Join(t.TempDir(), "environment.txt")
	script := "#!/bin/sh\nenv > " + recordTo + ".partial && mv " + recordTo + ".partial " + recordTo + "\nexec cat > /dev/null\n"
	if err := os.WriteFile(filepath.Join(directory, msg.HarnessBinaryName(msg.HarnessMock)), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return recordTo
}

func TestASpawnedPrincipalSessionReceivesTheGatewayAndTokenButNotTheSigningKey(t *testing.T) {
	for _, name := range config.SecretEnvironmentVariableNames() {
		t.Setenv(name, "must-not-reach-the-agent-"+name)
	}
	recordTo := writeEnvironmentRecordingMockHarness(t)
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))

	response := gated.requestAs(t, cookie, "POST", "/sessions", msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: "inst_test", AutoStart: true,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", response.Code, response.Body.String())
	}
	var created store.Session
	decodeRecorderJSON(t, response, &created)
	t.Cleanup(func() { gated.server.harness.Kill(created.SessionID) })
	if created.State != string(msg.SessionStarting) {
		t.Fatalf("session state = %q, want starting — the spawn failed", created.State)
	}

	var environment []string
	deadline := time.Now().Add(10 * time.Second)
	for environment == nil && time.Now().Before(deadline) {
		if data, err := os.ReadFile(recordTo); err == nil {
			environment = strings.Split(strings.TrimSpace(string(data)), "\n")
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if environment == nil {
		t.Fatal("the harness child never recorded its environment")
	}
	var gatewayURL, token string
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case gatewayURLEnvironmentVariable:
			gatewayURL = value
		case principalTokenEnvironmentVariable:
			token = value
		}
		for _, secretName := range config.SecretEnvironmentVariableNames() {
			if name == secretName {
				t.Errorf("the harness child received %s", secretName)
			}
		}
	}
	if gatewayURL != gatedTestServerPublicURL {
		t.Errorf("%s = %q, want %q", gatewayURLEnvironmentVariable, gatewayURL, gatedTestServerPublicURL)
	}
	if proxied := gated.requestWithBearer(t, token, "GET", "/issues/api/boards"); proxied.Code != http.StatusOK {
		t.Errorf("the token the child received does not work on /issues/: %d %s", proxied.Code, proxied.Body.String())
	}
}

func decodeRecorderJSON(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

// TestAnAdministratorsSessionAgentTokenReachesWhatTheAdministratorDoes covers
// a chat the operator started: its agent's token reaches the operator routes
// and every session, as the administrator's own login does, while anyone
// else's token stays on the store proxies.
func TestAnAdministratorsSessionAgentTokenReachesWhatTheAdministratorDoes(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	administratorSession := gated.createSessionAs(t, administratorCookie, administratorTestPrincipalID)
	administratorToken := gated.sessionAgentTokenFor(t, administratorSession)
	ordinaryCookie := gated.loginAs(t, firstTestPrincipalID)
	ordinarySession := gated.createSessionAs(t, ordinaryCookie, "")
	ordinaryToken := gated.sessionAgentTokenFor(t, ordinarySession)

	for _, route := range []struct{ method, path string }{
		{"GET", "/machines"},
		{"GET", "/peer-connections"},
		{"GET", "/bridge-prefs"},
		{"GET", "/sessions"},
		{"GET", "/sessions/" + ordinarySession.SessionID},
		{"GET", "/issues/api/boards"},
	} {
		if response := gated.requestWithBearer(t, administratorToken, route.method, route.path); response.Code != http.StatusOK {
			t.Errorf("administrator's token on %s %s = %d %s, want 200", route.method, route.path, response.Code, response.Body.String())
		}
	}
	var listed []json.RawMessage
	decodeRecorderJSON(t, gated.requestWithBearer(t, administratorToken, "GET", "/sessions"), &listed)
	if len(listed) < 2 {
		t.Errorf("administrator's token lists %d sessions, want every session (at least 2)", len(listed))
	}

	for _, route := range []struct{ method, path string }{
		{"GET", "/machines"},
		{"GET", "/peer-connections"},
		{"GET", "/sessions"},
	} {
		response := gated.requestWithBearer(t, ordinaryToken, route.method, route.path)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "authorization_header_not_accepted") {
			t.Errorf("ordinary token on %s %s = %d %s, want 401 authorization_header_not_accepted", route.method, route.path, response.Code, response.Body.String())
		}
	}

	throughProxy := httptest.NewRequest("GET", "/machines", nil)
	throughProxy.Header.Set("Authorization", "Bearer "+administratorToken)
	throughProxy.Header.Set("X-Forwarded-For", "203.0.113.7")
	if response := serve(gated.server, throughProxy); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "session_agent_token_not_public") {
		t.Errorf("administrator's token through the public proxy = %d %s, want 403 session_agent_token_not_public", response.Code, response.Body.String())
	}

	if response := gated.requestAs(t, administratorCookie, "POST", "/sessions/"+administratorSession.SessionID+"/stop", nil); response.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", response.Code, response.Body.String())
	}
	if response := gated.requestWithBearer(t, administratorToken, "GET", "/machines"); response.Code != http.StatusUnauthorized {
		t.Errorf("administrator's token after its session stopped = %d %s, want 401", response.Code, response.Body.String())
	}
}

// TestAnAdministratorsSessionAgentTokenIsHeldToThePeerItsSessionCameThrough
// matches the posting token's rule: a session a connected server started acts
// as an administrator only when that peer is one too.
func TestAnAdministratorsSessionAgentTokenIsHeldToThePeerItsSessionCameThrough(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	session := gated.createSessionAs(t, administratorCookie, administratorTestPrincipalID)
	if err := gated.store.RecordSessionPeerOrigin(store.SessionPeerOrigin{
		SessionID: session.SessionID, PeerPrincipalID: firstTestPrincipalID, RemotePrincipalID: "principal_000801"}); err != nil {
		t.Fatal(err)
	}
	token := gated.sessionAgentTokenFor(t, session)
	if response := gated.requestWithBearer(t, token, "GET", "/machines"); response.Code != http.StatusUnauthorized {
		t.Errorf("token of a session started through a peer that is not an administrator = %d %s, want 401", response.Code, response.Body.String())
	}
}
