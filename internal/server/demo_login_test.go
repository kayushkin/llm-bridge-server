package server

import (
	"encoding/json"
	"fmt"
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

// fakePrincipalDirectory answers GET /principals/{id} from a set of records
// and principal-store's own {"error":…} 404 for anything else. A record can be
// rewritten while the server runs, which is how a test demotes or disables
// somebody between requests.
type fakePrincipalDirectory struct {
	server      *httptest.Server
	mutex       sync.Mutex
	recordsByID map[string]string
	// reads counts the lookups that actually reached it, so a test can show
	// the caller's principal is read once per page load and not once per
	// request.
	reads int
	// groupIDsByMember answers GET /principals/{id}/groups: the groups each
	// human belongs to. A human missing here belongs to none.
	groupIDsByMember map[string][]string
}

func newFakePrincipalDirectory(t *testing.T, recordsByID map[string]string) *fakePrincipalDirectory {
	t.Helper()
	directory := &fakePrincipalDirectory{recordsByID: recordsByID}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		directory.mutex.Lock()
		directory.reads++
		record, ok := directory.recordsByID[r.PathValue("id")]
		directory.mutex.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: principal ` + r.PathValue("id") + `"}`))
			return
		}
		_, _ = w.Write([]byte(record))
	})
	mux.HandleFunc("GET /principals/{id}/groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		directory.mutex.Lock()
		groups := []map[string]any{}
		for _, groupID := range directory.groupIDsByMember[r.PathValue("id")] {
			groups = append(groups, map[string]any{"id": groupID, "kind": "group"})
		}
		directory.mutex.Unlock()
		_ = json.NewEncoder(w).Encode(groups)
	})
	directory.server = httptest.NewServer(mux)
	t.Cleanup(directory.server.Close)
	return directory
}

// readCount is how many lookups have reached this directory.
func (directory *fakePrincipalDirectory) readCount() int {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	return directory.reads
}

// setRecord replaces what the directory answers for one principal.
func (directory *fakePrincipalDirectory) setRecord(principalID, record string) {
	directory.mutex.Lock()
	defer directory.mutex.Unlock()
	directory.recordsByID[principalID] = record
}

func standardPrincipalDirectory(t *testing.T) *fakePrincipalDirectory {
	return newFakePrincipalDirectory(t, map[string]string{
		"principal_000001": `{"id":"principal_000001","kind":"human","display_name":"Slava","disabled_at":0,"is_administrator":false,"groups":[]}`,
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

func demoLoginTestServer(t *testing.T, principalStoreURL, kanbanStoreURL string) *Server {
	t.Helper()
	directory := t.TempDir()
	bridgeStore, err := store.New(filepath.Join(directory, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { bridgeStore.Close() })
	cfg := &config.Config{
		ImagesDir:         filepath.Join(directory, "images"),
		BridgePrefsPath:   filepath.Join(directory, "prefs.json"),
		LogStoreURL:       "http://localhost:0",
		PrincipalStoreURL: principalStoreURL,
		KanbanStoreURL:    kanbanStoreURL,
	}
	testAuthorizationConfig(cfg)
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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, newFakeKanbanStore(t).server.URL)

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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, newFakeKanbanStore(t).server.URL)
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
	server := demoLoginTestServer(t, directory.server.URL, newFakeKanbanStore(t).server.URL)
	directory.server.Close()
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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, kanban.server.URL)
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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))

	request := httptest.NewRequest("GET", "/kanban/boards", nil)
	request.Header.Set("X-Kanban-Store-Service-Token", "forged")
	// A key set directly on the map bypasses canonicalisation.
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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, kanban.server.URL)
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
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))
	kanban.server.Close()
	request := httptest.NewRequest("GET", "/kanban/boards", nil)
	request.AddCookie(cookie)
	response := serve(server, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "kanban_store_unavailable") {
		t.Fatalf("kanban-store down = %d %s, want 502 kanban_store_unavailable", response.Code, response.Body.String())
	}
}

func TestAClientSentPrincipalHeaderIsRefusedWithoutTheServiceToken(t *testing.T) {
	kanban := newFakeKanbanStore(t)
	server := demoLoginTestServer(t, standardPrincipalDirectory(t).server.URL, kanban.server.URL)
	cookie := loginCookieFrom(t, demoLogin(t, server, "principal_000001"))

	for name, addHeader := range map[string]func(http.Header){
		"canonical":     func(h http.Header) { h.Set(principalIdentityHeader, "principal_000002") },
		"non-canonical": func(h http.Header) { h["x-principal-id"] = []string{"principal_000002"} },
	} {
		for _, path := range []string{"/kanban/boards", "/health", "/sessions"} {
			request := httptest.NewRequest("GET", path, nil)
			addHeader(request.Header)
			request.AddCookie(cookie)
			response := serve(server, request)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "principal_header_without_service_token") {
				t.Errorf("%s %s with a cookie = %d %s, want 401 principal_header_without_service_token",
					name, path, response.Code, response.Body.String())
			}
		}
	}
	if got := kanban.recorded(); len(got) != 0 {
		t.Fatalf("kanban-store received %d requests from a caller naming its own principal: %+v", len(got), got)
	}
}

func TestStartupIsRefusedWithoutEachRequiredSetting(t *testing.T) {
	complete := config.Config{
		DemoLoginSigningKey: testSigningKey, ServiceToken: testServiceToken,
		KanbanStoreURL: "http://kanban.invalid", PrincipalStoreURL: "http://principal.invalid", GrantStoreURL: "http://grant.invalid",
	}
	if err := complete.ValidateRequestAuthorizationSettings(); err != nil {
		t.Fatalf("complete configuration: %v, want no error", err)
	}
	for name, testCase := range map[string]struct {
		mutate              func(*config.Config)
		environmentVariable string
	}{
		"no signing key":      {func(c *config.Config) { c.DemoLoginSigningKey = "" }, "LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY"},
		"short signing key":   {func(c *config.Config) { c.DemoLoginSigningKey = "short" }, "LLMBRIDGE_DEMO_LOGIN_SIGNING_KEY"},
		"no service token":    {func(c *config.Config) { c.ServiceToken = "" }, "LLMBRIDGE_SERVICE_TOKEN"},
		"short service token": {func(c *config.Config) { c.ServiceToken = "short" }, "LLMBRIDGE_SERVICE_TOKEN"},
		"no principal-store":  {func(c *config.Config) { c.PrincipalStoreURL = "" }, "LLMBRIDGE_PRINCIPAL_STORE_URL"},
		"no kanban-store":     {func(c *config.Config) { c.KanbanStoreURL = "" }, "LLMBRIDGE_KANBAN_STORE_URL"},
		"no grant-store":      {func(c *config.Config) { c.GrantStoreURL = "" }, "LLMBRIDGE_GRANT_STORE_URL"},
	} {
		candidate := complete
		testCase.mutate(&candidate)
		err := candidate.ValidateRequestAuthorizationSettings()
		if err == nil {
			t.Errorf("%s: no error, want a refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.environmentVariable) {
			t.Errorf("%s: %v, want the message to name %s", name, err, testCase.environmentVariable)
		}
	}
}

// TestNewRefusesAServerThatCouldNotIdentifyACaller pins the same refusal at the
// other end: a config missing a credential does not build a server that serves
// every route ungated.
func TestNewRefusesAServerThatCouldNotIdentifyACaller(t *testing.T) {
	for name, credentials := range map[string]struct{ signingKey, serviceToken string }{
		"no signing key":   {"", testServiceToken},
		"no service token": {testSigningKey, ""},
	} {
		func() {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Errorf("%s: New returned a server", name)
					return
				}
				if !strings.Contains(fmt.Sprint(recovered), "request authorization") {
					t.Errorf("%s: New panicked with %v, want it to name request authorization", name, recovered)
				}
			}()
			directory := t.TempDir()
			bridgeStore, err := store.New(filepath.Join(directory, "test.db"))
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			t.Cleanup(func() { bridgeStore.Close() })
			New(bridgeStore, nil, nil, nil, nil, testModelStore(t), nil, &config.Config{
				ImagesDir:           filepath.Join(directory, "images"),
				BridgePrefsPath:     filepath.Join(directory, "prefs.json"),
				LogStoreURL:         "http://localhost:0",
				PrincipalStoreURL:   "http://principal-store.invalid",
				GrantStoreURL:       "http://grant-store.invalid",
				DemoLoginSigningKey: credentials.signingKey,
				ServiceToken:        credentials.serviceToken,
			})
		}()
	}
}
