package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	proxied := gated.requestWithBearer(t, token, "GET", "/kanban/boards")
	if proxied.Code != http.StatusOK {
		t.Fatalf("token on /kanban/boards = %d %s, want 200", proxied.Code, proxied.Body.String())
	}
	recorded := gated.kanban.recorded()
	if len(recorded) != 1 {
		t.Fatalf("kanban-store received %d requests, want 1", len(recorded))
	}
	if got := recorded[0].Header.Values("X-Principal-Id"); len(got) != 1 || got[0] != firstTestPrincipalID {
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
	if response := gated.requestWithBearer(t, stoppedToken, "GET", "/kanban/boards"); response.Code != http.StatusUnauthorized ||
		!strings.Contains(response.Body.String(), "has ended") {
		t.Errorf("token of a stopped session = %d %s, want 401 naming the ended session", response.Code, response.Body.String())
	}

	deleted := gated.createSessionAs(t, cookie, "")
	deletedToken := gated.sessionAgentTokenFor(t, deleted)
	if err := gated.store.DeleteSession(deleted.SessionID); err != nil {
		t.Fatal(err)
	}
	if response := gated.requestWithBearer(t, deletedToken, "GET", "/kanban/boards"); response.Code != http.StatusUnauthorized {
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
	codec := gated.server.principalSessionCookieCodec

	if response := gated.requestWithBearer(t, cookie.Value, "GET", "/kanban/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("login cookie value as a bearer token = %d, want 401", response.Code)
	}
	if _, err := codec.decode(token); err == nil {
		t.Errorf("a session agent token decoded as a login cookie")
	}
	tokenAsCookie := httptest.NewRequest("GET", "/sessions", nil)
	tokenAsCookie.AddCookie(&http.Cookie{Name: demoLoginCookieName, Value: token})
	if response := serve(gated.server, tokenAsCookie); response.Code != http.StatusUnauthorized {
		t.Errorf("session agent token as the login cookie = %d, want 401", response.Code)
	}

	// Same key, same fields, signed as the other kind: neither verifies.
	cookieSignatureOverTokenFields := sessionAgentTokenPrefix + "." + firstTestPrincipalID + "." + strings.Split(token, ".")[2] + "." + strings.Split(token, ".")[3]
	forged := cookieSignatureOverTokenFields + "." + codec.signature(cookieSignatureOverTokenFields)
	if _, err := codec.decodeSessionAgentToken(forged); err == nil {
		t.Errorf("a token signed the way a cookie is signed verified as a session agent token")
	}
	expiry := strings.Split(cookie.Value, ".")[1]
	tokenSignedCookie := firstTestPrincipalID + "." + expiry + "." + codec.sessionAgentTokenSignature(firstTestPrincipalID+"."+expiry)
	if _, err := codec.decode(tokenSignedCookie); err == nil {
		t.Errorf("a cookie signed the way a token is signed verified as a login cookie")
	}

	expired := codec.encodeSessionAgentToken(sessionAgentTokenClaims{PrincipalID: firstTestPrincipalID, SessionID: session.SessionID, ExpiresAt: time.Now().Add(-time.Minute)})
	if response := gated.requestWithBearer(t, expired, "GET", "/kanban/boards"); response.Code != http.StatusUnauthorized {
		t.Errorf("expired token = %d, want 401", response.Code)
	}
	otherPrincipal := codec.encodeSessionAgentToken(sessionAgentTokenClaims{PrincipalID: secondTestPrincipalID, SessionID: session.SessionID, ExpiresAt: time.Now().Add(time.Hour)})
	if response := gated.requestWithBearer(t, otherPrincipal, "GET", "/kanban/boards"); response.Code != http.StatusUnauthorized {
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
	if proxied := gated.requestWithBearer(t, token, "GET", "/kanban/boards"); proxied.Code != http.StatusOK {
		t.Errorf("the token the child received does not work on /kanban/: %d %s", proxied.Code, proxied.Body.String())
	}
}

func decodeRecorderJSON(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

// fakeGrantStoreRecording records every request it receives and answers 200
// with an empty list.
type fakeGrantStoreRecording struct {
	server   *httptest.Server
	mutex    sync.Mutex
	requests []recordedKanbanRequest
}

func TestGrantStoreProxyMapsEveryRouteAndCarriesOnlyThePrincipal(t *testing.T) {
	recording := &fakeGrantStoreRecording{}
	recording.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var keyNames []string
		for key := range r.Header {
			keyNames = append(keyNames, key)
		}
		recording.mutex.Lock()
		recording.requests = append(recording.requests, recordedKanbanRequest{
			Method: r.Method, EscapedPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: string(body), HeaderKeyNames: keyNames,
		})
		recording.mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(recording.server.Close)

	gated := newGatedTestServer(t, nil)
	gated.server.cfg.GrantStoreURL = recording.server.URL
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	session := gated.createSessionAs(t, cookie, "")
	token := gated.sessionAgentTokenFor(t, session)

	for _, route := range []struct{ method, gatewayPath, upstreamPath, upstreamQuery string }{
		{"GET", "/grant-store/grants?principal_id=principal_000001", "/grants", "principal_id=principal_000001"},
		{"POST", "/grant-store/grants", "/grants", ""},
		{"GET", "/grant-store/grants/grant_000001", "/grants/grant_000001", ""},
		{"POST", "/grant-store/grants/grant_000001/revoke", "/grants/grant_000001/revoke", ""},
		{"GET", "/grant-store/principals/principal_000001/effective?relation=can_use", "/principals/principal_000001/effective", "relation=can_use"},
		{"GET", "/grant-store/relations", "/relations", ""},
		{"GET", "/grant-store/resource-types", "/resource-types", ""},
	} {
		recording.mutex.Lock()
		recording.requests = nil
		recording.mutex.Unlock()

		request := httptest.NewRequest(route.method, route.gatewayPath, strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Principal-Id", secondTestPrincipalID)
		request.Header.Set("X-Grant-Store-Service-Token", "forged")
		request.Header.Set("X-Kanban-Store-Service-Token", "forged")
		request.Header["x-grant-store-service-token"] = []string{"forged-lowercase"}
		request.AddCookie(&http.Cookie{Name: "unrelated", Value: "kept"})
		if route.method == "POST" {
			request.Header.Set("Authorization", "Bearer "+token)
		} else {
			request.AddCookie(cookie)
		}
		response := serve(gated.server, request)
		if response.Code != http.StatusOK {
			t.Errorf("%s %s = %d %s, want 200", route.method, route.gatewayPath, response.Code, response.Body.String())
			continue
		}

		recording.mutex.Lock()
		requests := append([]recordedKanbanRequest(nil), recording.requests...)
		recording.mutex.Unlock()
		if len(requests) != 1 {
			t.Errorf("%s %s reached grant-store %d times, want 1", route.method, route.gatewayPath, len(requests))
			continue
		}
		seen := requests[0]
		if seen.Method != route.method || seen.EscapedPath != route.upstreamPath || seen.RawQuery != route.upstreamQuery {
			t.Errorf("%s %s forwarded as %s %s ? %s, want %s ? %s", route.method, route.gatewayPath, seen.Method, seen.EscapedPath, seen.RawQuery, route.upstreamPath, route.upstreamQuery)
		}
		if got := seen.Header.Values("X-Principal-Id"); len(got) != 1 || got[0] != firstTestPrincipalID {
			t.Errorf("%s %s: grant-store saw X-Principal-Id %v, want [%s]", route.method, route.gatewayPath, got, firstTestPrincipalID)
		}
		for _, key := range seen.HeaderKeyNames {
			for _, forbidden := range []string{"X-Grant-Store-Service-Token", "X-Kanban-Store-Service-Token", "Authorization"} {
				if strings.EqualFold(key, forbidden) {
					t.Errorf("%s %s: grant-store saw %s", route.method, route.gatewayPath, key)
				}
			}
		}
		if cookieHeader := seen.Header.Get("Cookie"); strings.Contains(cookieHeader, demoLoginCookieName) || !strings.Contains(cookieHeader, "unrelated=kept") {
			t.Errorf("%s %s: grant-store saw Cookie %q, want only the unrelated cookie", route.method, route.gatewayPath, cookieHeader)
		}
	}

	if response := gated.requestAs(t, nil, "GET", "/grant-store/relations", nil); response.Code != http.StatusUnauthorized {
		t.Errorf("anonymous /grant-store/relations = %d, want 401", response.Code)
	}
	if response := gated.requestAsService(t, "GET", "/grant-store/relations"); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "store_proxy_requires_a_principal") {
		t.Errorf("service token on /grant-store/relations = %d %s, want 403 store_proxy_requires_a_principal", response.Code, response.Body.String())
	}
}
