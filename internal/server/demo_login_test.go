package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

const demoLoginTestSigningKey = "0123456789abcdef0123456789abcdef-test-key"

const demoLoginTestServiceToken = "service-token-for-tests-0123456789abcdef"

// fakePrincipalDirectory answers GET /principals/{id} from a fixed set of
// records and principal-store's own {"error":…} 404 for anything else.
func fakePrincipalDirectory(t *testing.T, recordsByID map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		record, ok := recordsByID[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: principal ` + r.PathValue("id") + `"}`))
			return
		}
		_, _ = w.Write([]byte(record))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func standardPrincipalDirectory(t *testing.T) *httptest.Server {
	return fakePrincipalDirectory(t, map[string]string{
		"principal_000001": `{"id":"principal_000001","kind":"human","display_name":"Slava","disabled_at":0,"groups":[]}`,
		"principal_000006": `{"id":"principal_000006","kind":"group","display_name":"Data Team","disabled_at":0,"members":[]}`,
		"principal_000009": `{"id":"principal_000009","kind":"human","display_name":"Gone","disabled_at":1788980427,"groups":[]}`,
	})
}

// recordedKanbanRequest is what the fake kanban-store saw.
type recordedKanbanRequest struct {
	Method         string
	EscapedPath    string
	RawQuery       string
	Header         http.Header
	Body           string
	HeaderKeyNames []string
}

// fakeKanbanStore records every request and answers from respond.
type fakeKanbanStore struct {
	server   *httptest.Server
	mutex    sync.Mutex
	requests []recordedKanbanRequest
	respond  func(w http.ResponseWriter, r *http.Request)
}

func newFakeKanbanStore(t *testing.T) *fakeKanbanStore {
	t.Helper()
	fake := &fakeKanbanStore{
		respond: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"boards":[]}`))
		},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var keyNames []string
		for key := range r.Header {
			keyNames = append(keyNames, key)
		}
		fake.mutex.Lock()
		fake.requests = append(fake.requests, recordedKanbanRequest{
			Method: r.Method, EscapedPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: string(body), HeaderKeyNames: keyNames,
		})
		fake.mutex.Unlock()
		fake.respond(w, r)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeKanbanStore) recorded() []recordedKanbanRequest {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]recordedKanbanRequest(nil), fake.requests...)
}

func demoLoginTestServer(t *testing.T, demoLoginSetting, principalStoreURL, kanbanStoreURL string) *Server {
	t.Helper()
	directory := t.TempDir()
	bridgeStore, err := store.New(filepath.Join(directory, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { bridgeStore.Close() })
	cfg := &config.Config{
		ImagesDir:           filepath.Join(directory, "images"),
		BridgePrefsPath:     filepath.Join(directory, "prefs.json"),
		LogStoreURL:         "http://localhost:0",
		PrincipalStoreURL:   principalStoreURL,
		KanbanStoreURL:      kanbanStoreURL,
		GrantStoreURL:       "http://grant-store.invalid",
		DemoLoginSetting:    demoLoginSetting,
		DemoLoginSigningKey: demoLoginTestSigningKey,
		ServiceToken:        demoLoginTestServiceToken,
	}
	return New(bridgeStore, nil, nil, nil, nil, testModelStore(t), nil, cfg)
}

func serve(server *Server, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func demoLogin(t *testing.T, server *Server, principalID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", "/auth/demo-login", strings.NewReader(`{"principal_id":"`+principalID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	return serve(server, request)
}

func loginCookieFrom(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == demoLoginCookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in response (status %d, body %s)", demoLoginCookieName, recorder.Code, recorder.Body.String())
	return nil
}

func TestDemoLoginWithAHumanSetsACookieThatAuthPrincipalReads(t *testing.T) {
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, newFakeKanbanStore(t).server.URL)

	login := demoLogin(t, server, "principal_000001")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d body = %s, want 200", login.Code, login.Body.String())
	}
	cookie := loginCookieFrom(t, login)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie attributes = HttpOnly %v SameSite %v Path %q, want HttpOnly, Lax, /", cookie.HttpOnly, cookie.SameSite, cookie.Path)
	}
	var loginBody loggedInPrincipalResponse
	if err := json.Unmarshal(login.Body.Bytes(), &loginBody); err != nil || loginBody.PrincipalID != "principal_000001" {
		t.Fatalf("login body = %s (%v), want principal_000001", login.Body.String(), err)
	}
	if lifetime := time.Until(loginBody.ExpiresAt); lifetime < 11*time.Hour || lifetime > 12*time.Hour+time.Minute {
		t.Fatalf("expires_at %v is %v away, want about 12h", loginBody.ExpiresAt, lifetime)
	}

	who := httptest.NewRequest("GET", "/auth/principal", nil)
	who.AddCookie(cookie)
	answer := serve(server, who)
	var whoBody loggedInPrincipalResponse
	if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &whoBody) != nil ||
		whoBody.PrincipalID != "principal_000001" || !whoBody.ExpiresAt.Equal(loginBody.ExpiresAt) {
		t.Fatalf("GET /auth/principal = %d %s, want 200 principal_000001 with the login's expiry", answer.Code, answer.Body.String())
	}

	anonymous := serve(server, httptest.NewRequest("GET", "/auth/principal", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("GET /auth/principal without a cookie = %d, want 401", anonymous.Code)
	}

	logout := serve(server, httptest.NewRequest("POST", "/auth/logout", nil))
	cleared := loginCookieFrom(t, logout)
	if logout.Code != http.StatusNoContent || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout = %d cookie %+v, want 204 and a cleared cookie", logout.Code, cleared)
	}
}

func TestDemoLoginRefusesUnknownGroupDisabledAndMalformedPrincipals(t *testing.T) {
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, newFakeKanbanStore(t).server.URL)
	for _, testCase := range []struct {
		principalID string
		code        string
	}{
		{"principal_000404", "unknown_principal"},
		{"principal_000006", "principal_not_human"},
		{"principal_000009", "principal_disabled"},
		{"Slava Kayushkin", "invalid_principal_id"},
		{"principal_000001.9999999999", "invalid_principal_id"},
	} {
		response := demoLogin(t, server, testCase.principalID)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.code) {
			t.Errorf("login as %q = %d %s, want 400 %s", testCase.principalID, response.Code, response.Body.String(), testCase.code)
		}
		if len(response.Result().Cookies()) != 0 {
			t.Errorf("login as %q set a cookie on refusal", testCase.principalID)
		}
	}
}

func TestDemoLoginAnswers502WhenPrincipalStoreIsDown(t *testing.T) {
	directory := standardPrincipalDirectory(t)
	server := demoLoginTestServer(t, "enabled", directory.URL, newFakeKanbanStore(t).server.URL)
	directory.Close()
	response := demoLogin(t, server, "principal_000001")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "principal_store_unavailable") {
		t.Fatalf("login with principal-store down = %d %s, want 502 principal_store_unavailable", response.Code, response.Body.String())
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("a cookie was set although principal-store could not be asked")
	}
}

func TestKanbanProxyWithoutAValidCookieIs401AndReachesNothing(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))

	tampered := *cookie
	lastCharacter := tampered.Value[len(tampered.Value)-1]
	replacement := byte('A')
	if lastCharacter == 'A' {
		replacement = 'B'
	}
	tampered.Value = tampered.Value[:len(tampered.Value)-1] + string(replacement)

	tamperedPrincipal := *cookie
	tamperedPrincipal.Value = strings.Replace(cookie.Value, "principal_000001", "principal_000002", 1)

	expired := &http.Cookie{Name: demoLoginCookieName, Value: server.principalSessionCookieCodec.encode(principalSession{
		PrincipalID: "principal_000001", ExpiresAt: time.Now().Add(-time.Minute),
	})}
	signedWithAnotherKey := &http.Cookie{Name: demoLoginCookieName, Value: (&principalSessionCookieCodec{
		signingKey: []byte("a-different-key-that-is-32-bytes-long!!"), now: time.Now,
	}).encode(principalSession{PrincipalID: "principal_000001", ExpiresAt: time.Now().Add(time.Hour)})}

	for name, requestCookie := range map[string]*http.Cookie{
		"no cookie":               nil,
		"tampered signature":      &tampered,
		"tampered principal":      &tamperedPrincipal,
		"expired":                 expired,
		"signed with another key": signedWithAnotherKey,
		"malformed":               {Name: demoLoginCookieName, Value: "not-a-session"},
	} {
		request := httptest.NewRequest("GET", "/kanban/boards", nil)
		request.Header.Set("X-Principal-Id", "principal_000001")
		if requestCookie != nil {
			request.AddCookie(requestCookie)
		}
		response := serve(server, request)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "not_logged_in") {
			t.Errorf("%s: /kanban/boards = %d %s, want 401 not_logged_in", name, response.Code, response.Body.String())
		}
		who := httptest.NewRequest("GET", "/auth/principal", nil)
		if requestCookie != nil {
			who.AddCookie(requestCookie)
		}
		if answer := serve(server, who); answer.Code != http.StatusUnauthorized {
			t.Errorf("%s: /auth/principal = %d, want 401", name, answer.Code)
		}
	}
	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("kanban-store received %d requests from unauthenticated callers: %+v", len(got), got)
	}
}

func TestKanbanProxyCarriesOnlyTheCookiesPrincipalAndNoServiceToken(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))

	request := httptest.NewRequest("GET", "/kanban/boards", nil)
	request.Header.Set("X-Principal-Id", "principal_999999")
	request.Header.Set("X-Kanban-Store-Service-Token", "forged")
	// A key set directly on the map bypasses canonicalisation.
	request.Header["x-principal-id"] = []string{"principal_888888"}
	request.Header["x-kanban-store-service-token"] = []string{"forged-lowercase"}
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: "unrelated", Value: "kept"})
	request.AddCookie(cookie)

	response := serve(server, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxied GET = %d %s, want 200", response.Code, response.Body.String())
	}
	got := kanban.recorded()
	if len(got) != 1 {
		t.Fatalf("kanban-store received %d requests, want 1", len(got))
	}
	seen := got[0]
	if values := seen.Header.Values("X-Principal-Id"); len(values) != 1 || values[0] != "principal_000001" {
		t.Fatalf("kanban-store saw X-Principal-Id %v, want exactly [principal_000001]", values)
	}
	for _, key := range seen.HeaderKeyNames {
		if strings.EqualFold(key, "X-Kanban-Store-Service-Token") {
			t.Fatalf("kanban-store saw a service token header %q: %v", key, seen.Header[key])
		}
	}
	if cookieHeader := seen.Header.Get("Cookie"); strings.Contains(cookieHeader, demoLoginCookieName) || !strings.Contains(cookieHeader, "unrelated=kept") {
		t.Fatalf("kanban-store saw Cookie %q, want the unrelated cookie and not the login cookie", cookieHeader)
	}
	if seen.Header.Get("Accept") != "application/json" {
		t.Fatalf("Accept header not passed through: %v", seen.Header)
	}
}

func TestKanbanProxyPassesPathQueryBodyAndUpstreamAnswerThroughUnchanged(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))

	escapedPathRequest := httptest.NewRequest("GET", "/kanban/boards/a%2Fb/cards?tag=x&tag=y%20z", nil)
	escapedPathRequest.AddCookie(cookie)
	if response := serve(server, escapedPathRequest); response.Code != http.StatusOK {
		t.Fatalf("escaped path GET = %d %s", response.Code, response.Body.String())
	}

	kanban.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kanban-Refusal", "grant")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"principal_000001 has no grant on board b1"}`))
	}
	postRequest := httptest.NewRequest("POST", "/kanban/boards/b1/cards", strings.NewReader(`{"title":"Call plumber"}`))
	postRequest.Header.Set("Content-Type", "application/json")
	postRequest.AddCookie(cookie)
	response := serve(server, postRequest)
	if response.Code != http.StatusForbidden ||
		response.Body.String() != `{"error":"principal_000001 has no grant on board b1"}` ||
		response.Header().Get("X-Kanban-Refusal") != "grant" ||
		response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("upstream 403 came back as %d %v %s, want it unchanged", response.Code, response.Header(), response.Body.String())
	}

	got := kanban.recorded()
	if len(got) != 2 {
		t.Fatalf("kanban-store received %d requests, want 2", len(got))
	}
	if got[0].Method != "GET" || got[0].EscapedPath != "/api/boards/a%2Fb/cards" || got[0].RawQuery != "tag=x&tag=y%20z" {
		t.Fatalf("GET forwarded as %s %s ? %s, want GET /api/boards/a%%2Fb/cards ? tag=x&tag=y%%20z", got[0].Method, got[0].EscapedPath, got[0].RawQuery)
	}
	if got[1].Method != "POST" || got[1].EscapedPath != "/api/boards/b1/cards" || got[1].Body != `{"title":"Call plumber"}` ||
		got[1].Header.Get("Content-Type") != "application/json" {
		t.Fatalf("POST forwarded as %s %s body %q type %q", got[1].Method, got[1].EscapedPath, got[1].Body, got[1].Header.Get("Content-Type"))
	}
}

func TestKanbanProxyAnswers502WhenKanbanStoreIsDown(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, "enabled", standardPrincipalDirectory(t).URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))
	kanban.server.Close()
	request := httptest.NewRequest("GET", "/kanban/boards", nil)
	request.AddCookie(cookie)
	response := serve(server, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "kanban_store_unavailable") {
		t.Fatalf("kanban-store down = %d %s, want 502 kanban_store_unavailable", response.Code, response.Body.String())
	}
}

func TestDemoLoginDisabledRoutesNothing(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, "", standardPrincipalDirectory(t).URL, kanban.server.URL)
	for _, request := range []*http.Request{
		httptest.NewRequest("POST", "/auth/demo-login", strings.NewReader(`{"principal_id":"principal_000001"}`)),
		httptest.NewRequest("GET", "/auth/principal", nil),
		httptest.NewRequest("POST", "/auth/logout", nil),
		httptest.NewRequest("GET", "/kanban/boards", nil),
		httptest.NewRequest("GET", "/grant-store/relations", nil),
	} {
		if response := serve(server, request); response.Code != http.StatusNotFound {
			t.Errorf("%s %s with demo login disabled = %d, want 404", request.Method, request.URL.Path, response.Code)
		}
	}
	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("kanban-store received %d requests with demo login disabled", len(got))
	}
}

func TestDemoLoginConfigurationIsRefusedWhenIncomplete(t *testing.T) {
	complete := config.Config{
		DemoLoginSetting: "enabled", DemoLoginSigningKey: demoLoginTestSigningKey, ServiceToken: demoLoginTestServiceToken,
		KanbanStoreURL: "http://kanban.invalid", PrincipalStoreURL: "http://principal.invalid", GrantStoreURL: "http://grant.invalid",
	}
	if enabled, err := complete.DemoLoginEnabled(); !enabled || err != nil {
		t.Fatalf("complete configuration: enabled=%v err=%v, want enabled", enabled, err)
	}
	for name, mutate := range map[string]func(*config.Config){
		"unrecognised value":  func(c *config.Config) { c.DemoLoginSetting = "true" },
		"no signing key":      func(c *config.Config) { c.DemoLoginSigningKey = "" },
		"short signing key":   func(c *config.Config) { c.DemoLoginSigningKey = "short" },
		"no service token":    func(c *config.Config) { c.ServiceToken = "" },
		"short service token": func(c *config.Config) { c.ServiceToken = "short" },
		"no kanban-store":     func(c *config.Config) { c.KanbanStoreURL = "" },
		"no grant-store":      func(c *config.Config) { c.GrantStoreURL = "" },
		"no principal-store":  func(c *config.Config) { c.PrincipalStoreURL = "" },
	} {
		candidate := complete
		mutate(&candidate)
		if enabled, err := candidate.DemoLoginEnabled(); enabled || err == nil {
			t.Errorf("%s: enabled=%v err=%v, want a refusal", name, enabled, err)
		}
	}
	off := config.Config{}
	if enabled, err := off.DemoLoginEnabled(); enabled || err != nil {
		t.Fatalf("unset: enabled=%v err=%v, want off with no error", enabled, err)
	}
}
