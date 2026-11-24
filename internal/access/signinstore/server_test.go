package signinstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

const (
	testAdministrator   = "principal_000001"
	testPerson          = "principal_000002"
	testGroup           = "principal_000006"
	testMultichatToken  = "multichat-bearer"
	testDiscordAccount  = "@discord_1234:matrix.example.test"
	testDiscordRedirect = "https://example.test/api/sign-in/discord/callback"
)

// fakeWorld stands in for people-store, auth-store, multichat and Discord over
// HTTP, and for principal-store in Go (fakePrincipalDirectory), and records
// what multichat was asked to send.
type fakeWorld struct {
	mutex      sync.Mutex
	principals map[string]Principal
	sent       []string
	server     *httptest.Server
	// discordBindings maps a Discord user id to the principal bound to it.
	discordBindings map[string]string
	linkedBy        map[string]string
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	viewer := Principal{ID: testPerson, Kind: "human", DisplayName: "Viewer"}
	viewer.Groups = append(viewer.Groups, struct {
		ID string `json:"id"`
	}{ID: "principal_000130"})
	world := &fakeWorld{principals: map[string]Principal{
		testAdministrator: {ID: testAdministrator, Kind: "human", DisplayName: "Operator", IsAdministrator: true},
		testPerson:        viewer,
		testGroup:         {ID: testGroup, Kind: "group", DisplayName: "Library members"},
	}}
	world.discordBindings, world.linkedBy = map[string]string{}, map[string]string{}
	mux := http.NewServeMux()
	// Discord: a code becomes an access token, and the token names a user.
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_secret") != testMultichatToken || r.Form.Get("client_id") != "discord-app" || r.Form.Get("redirect_uri") != testDiscordRedirect {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"access_token":"access-for-%s"}`, r.Form.Get("code"))
	})
	mux.HandleFunc("GET /users/@me", func(w http.ResponseWriter, r *http.Request) {
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-for-")
		fmt.Fprintf(w, `{"id":%q}`, user)
	})
	mux.HandleFunc("GET /identities/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") == "discord" && r.URL.Query().Get("value") == "viewer" {
			json.NewEncoder(w).Encode(map[string]any{"person": map[string]any{"id": "person_000002"}})
			return
		}
		http.Error(w, `{"error":"no such identity"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /persons", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("principal_id") == testPerson {
			fmt.Fprint(w, `[{"id":"person_000002"}]`)
			return
		}
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /persons/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "person_000002", "principal_id": testPerson,
			"identities": []any{map[string]any{"kind": "matrix_user", "value": testDiscordAccount}}})
	})
	mux.HandleFunc("GET /api/credentials/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer auth-store-token" || r.Header.Get("X-Auth-App") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "access_token": testMultichatToken})
	})
	mux.HandleFunc("GET /api/conversations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testMultichatToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]any{
			map[string]any{"room_id": "!group:matrix", "member_count": 12, "members": []any{map[string]any{"user_id": testDiscordAccount}}},
			map[string]any{"room_id": "!direct:matrix", "member_count": 2, "members": []any{map[string]any{"user_id": testDiscordAccount}}},
		})
	})
	mux.HandleFunc("POST /api/conversations/{room}/send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body string `json:"body"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		world.mutex.Lock()
		world.sent = append(world.sent, r.PathValue("room")+" "+body.Body)
		world.mutex.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	world.server = httptest.NewServer(mux)
	t.Cleanup(world.server.Close)
	return world
}

// disablePrincipal stamps a principal disabled, as principal-store would.
func (world *fakeWorld) disablePrincipal(principalID string, at int64) {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	principal := world.principals[principalID]
	principal.DisabledAt = at
	world.principals[principalID] = principal
}

// fakePrincipalDirectory is principal-store as the server answers it in
// process: the world's principals and Discord bindings.
type fakePrincipalDirectory struct{ world *fakeWorld }

func (directory fakePrincipalDirectory) Principal(ctx context.Context, principalID string) (Principal, error) {
	directory.world.mutex.Lock()
	defer directory.world.mutex.Unlock()
	principal, known := directory.world.principals[principalID]
	if !known {
		return Principal{}, fmt.Errorf("%w: principal-store has no %s", ErrNotFound, principalID)
	}
	return principal, nil
}

func (directory fakePrincipalDirectory) PrincipalOfIdentity(ctx context.Context, provider, tenantID, subject string) (Principal, error) {
	directory.world.mutex.Lock()
	defer directory.world.mutex.Unlock()
	principalID, bound := directory.world.discordBindings[subject]
	if provider != "discord" || tenantID != "" || !bound {
		return Principal{}, fmt.Errorf("%w: no %s identity %s is bound", ErrNotFound, provider, subject)
	}
	return directory.world.principals[principalID], nil
}

func (directory fakePrincipalDirectory) LinkIdentity(ctx context.Context, principalID, provider, tenantID, subject, linkedBy, note string) error {
	directory.world.mutex.Lock()
	defer directory.world.mutex.Unlock()
	if _, bound := directory.world.discordBindings[subject]; bound {
		return fmt.Errorf("%w: %s", ErrIdentityAlreadyBound, subject)
	}
	directory.world.discordBindings[subject] = principalID
	directory.world.linkedBy[subject] = linkedBy
	return nil
}

type testService struct {
	handler   http.Handler
	store     *Store
	directory *Directory
	world     *fakeWorld
	clock     time.Time
}

// newTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module))
}

// testAdministratorHeader is how a test request names the administrator it
// acts for; the test mux turns it into ContextWithAdministrator, as the server
// that mounts these routes does from a verified credential. The routes
// themselves never read it.
const testAdministratorHeader = "X-Test-Administrator"

func newTestService(t *testing.T) *testService {
	t.Helper()
	world := newFakeWorld(t)
	store := newTestStore(t)
	directory := NewDirectory(fakePrincipalDirectory{world: world}, DirectoryConfig{PeopleStoreURL: world.server.URL, MultichatURL: world.server.URL,
		AuthStoreURL: world.server.URL, AuthStoreToken: "auth-store-token", MultichatCredentialID: "cred_multichat"})
	service := &testService{store: store, directory: directory, world: world, clock: time.Now()}
	store.now = func() time.Time { return service.clock }
	directory.now = func() time.Time { return service.clock }
	handlers := NewHandlers(store, directory, HandlersConfig{SignInPageURL: "https://example.test/signin", CookieDomain: "example.test",
		Discord: DiscordConfig{ApplicationID: "discord-app", ClientSecretCredentialID: "cred_discord", RedirectURL: testDiscordRedirect}})
	mux := http.NewServeMux()
	for pattern, route := range map[string]http.HandlerFunc{
		"GET /health":                            handlers.Health,
		"POST /link-requests":                    handlers.RequestLink,
		"POST /redeem":                           handlers.Redeem,
		"GET /session":                           handlers.CurrentSession,
		"POST /sign-out":                         handlers.SignOut,
		"GET /discord/start":                     handlers.StartDiscord,
		"GET /discord/callback":                  handlers.FinishDiscord,
		"POST /sign-in-links":                    handlers.MakeLink,
		"GET /sign-in-links":                     handlers.ListLinks,
		"GET /sessions":                          handlers.ListSessions,
		"POST /sessions/{id}/revoke":             handlers.RevokeSession,
		"POST /principals/{id}/discord-messages": handlers.SendDiscordMessage,
	} {
		mux.HandleFunc(pattern, route)
	}
	previousAuthorize, previousAPI := discordAuthorizeURL, discordAPIBase
	discordAuthorizeURL, discordAPIBase = world.server.URL+"/oauth2/authorize", world.server.URL
	t.Cleanup(func() { discordAuthorizeURL, discordAPIBase = previousAuthorize, previousAPI })
	service.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if administratorID := r.Header.Get(testAdministratorHeader); administratorID != "" {
			r.Header.Del(testAdministratorHeader)
			r = r.WithContext(ContextWithAdministrator(r.Context(), administratorID))
		}
		mux.ServeHTTP(w, r)
	})
	return service
}

func (service *testService) do(t *testing.T, method, path string, body any, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = "203.0.113.7:5000"
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	service.handler.ServeHTTP(recorder, request)
	return recorder
}

var asAdministrator = map[string]string{testAdministratorHeader: testAdministrator}

// makeLink has the operator make a link and returns its secret.
func (service *testService) makeLink(t *testing.T, principalID string) string {
	t.Helper()
	answer := service.do(t, "POST", "/sign-in-links", map[string]string{"principal_id": principalID}, asAdministrator, nil)
	if answer.Code != http.StatusCreated {
		t.Fatalf("POST /sign-in-links = %d %s", answer.Code, answer.Body)
	}
	var made struct {
		URL  string     `json:"url"`
		Link SignInLink `json:"link"`
	}
	json.Unmarshal(answer.Body.Bytes(), &made)
	if made.Link.MadeBy != testAdministrator || !strings.HasPrefix(made.URL, "https://example.test/signin#") {
		t.Fatalf("made = %s", answer.Body)
	}
	return strings.TrimPrefix(made.URL, "https://example.test/signin#")
}

func sessionCookieFrom(t *testing.T, answer *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range answer.Result().Cookies() {
		if cookie.Name == SessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", SessionCookieName, answer.Header()["Set-Cookie"])
	return nil
}

func TestALinkTheOperatorMakesSignsInOnceAcrossTheDomain(t *testing.T) {
	service := newTestService(t)
	secret := service.makeLink(t, testPerson)

	redeemed := service.do(t, "POST", "/redeem", map[string]string{"secret": secret}, nil, nil)
	if redeemed.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s", redeemed.Code, redeemed.Body)
	}
	cookie := sessionCookieFrom(t, redeemed)
	setCookie := redeemed.Header().Get("Set-Cookie")
	for _, want := range []string{"Domain=example.test", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(setCookie, want) {
			t.Errorf("Set-Cookie %q lacks %s", setCookie, want)
		}
	}
	if again := service.do(t, "POST", "/redeem", map[string]string{"secret": secret}, nil, nil); again.Code != http.StatusGone {
		t.Fatalf("a second redeem = %d, want 410: a link works once", again.Code)
	}

	current := service.do(t, "GET", "/session", nil, nil, cookie)
	if current.Code != http.StatusOK || current.Header().Get(SignedInPrincipalHeader) != testPerson {
		t.Fatalf("GET /session = %d %s", current.Code, current.Body)
	}
	var answer sessionAnswer
	json.Unmarshal(current.Body.Bytes(), &answer)
	if answer.IsAdministrator || len(answer.GroupIDs) != 1 || answer.GroupIDs[0] != "principal_000130" || answer.Method != MethodSignInLink {
		t.Fatalf("session answer = %+v", answer)
	}

	if out := service.do(t, "POST", "/sign-out", nil, nil, cookie); out.Code != http.StatusOK {
		t.Fatalf("sign-out = %d", out.Code)
	}
	if after := service.do(t, "GET", "/session", nil, nil, cookie); after.Code != http.StatusUnauthorized || !strings.Contains(after.Body.String(), "session_revoked") {
		t.Fatalf("after sign-out = %d %s, want 401 session_revoked", after.Code, after.Body)
	}
}

func TestTheAdministratorRoutesNeedAnEnabledAdministratorInTheContext(t *testing.T) {
	service := newTestService(t)
	body := map[string]string{"principal_id": testPerson}
	routes := []struct{ method, path string }{
		{"POST", "/sign-in-links"}, {"GET", "/sign-in-links"}, {"GET", "/sessions"},
		{"POST", "/sessions/session_000001/revoke"}, {"POST", "/principals/" + testPerson + "/discord-messages"},
	}
	for _, route := range routes {
		for name, headers := range map[string]map[string]string{
			"no administrator":                        nil,
			"a principal who is not an administrator": {testAdministratorHeader: testPerson},
			"a principal principal-store lacks":       {testAdministratorHeader: "principal_000999"},
			"a group":                                 {testAdministratorHeader: testGroup},
		} {
			if answer := service.do(t, route.method, route.path, body, headers, nil); answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), "administrator_required") {
				t.Errorf("%s %s with %s = %d %s, want 403 administrator_required", route.method, route.path, name, answer.Code, answer.Body)
			}
		}
	}
	// An X-Principal-Id header names nobody: the routes read the context only.
	if answer := service.do(t, "POST", "/sign-in-links", body, map[string]string{"X-Principal-Id": testAdministrator}, nil); answer.Code != http.StatusForbidden {
		t.Errorf("X-Principal-Id naming an administrator = %d, want 403", answer.Code)
	}
	service.world.disablePrincipal(testAdministrator, 1790000000)
	service.clock = service.clock.Add(principalCacheLifetime + time.Second)
	if answer := service.do(t, "POST", "/sign-in-links", body, asAdministrator, nil); answer.Code != http.StatusForbidden {
		t.Errorf("a disabled administrator = %d, want 403", answer.Code)
	}
}

func TestALinkForAGroupIsRefused(t *testing.T) {
	service := newTestService(t)
	if answer := service.do(t, "POST", "/sign-in-links", map[string]string{"principal_id": testGroup}, asAdministrator, nil); answer.Code != http.StatusBadRequest {
		t.Errorf("a link for a group = %d, want 400", answer.Code)
	}
}

func TestADisabledPersonIsSignedOutOnceTheCacheExpires(t *testing.T) {
	service := newTestService(t)
	cookie := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": service.makeLink(t, testPerson)}, nil, nil))
	service.world.disablePrincipal(testPerson, 1790000000)
	if still := service.do(t, "GET", "/session", nil, nil, cookie); still.Code != http.StatusOK {
		t.Fatalf("within the cache lifetime = %d", still.Code)
	}
	service.clock = service.clock.Add(principalCacheLifetime + time.Second)
	if after := service.do(t, "GET", "/session", nil, nil, cookie); after.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled person after the cache expires = %d %s, want 401", after.Code, after.Body)
	}
}

func TestAnExpiredLinkAndAnExpiredSessionAreRefused(t *testing.T) {
	service := newTestService(t)
	unused := service.makeLink(t, testPerson)
	used := service.makeLink(t, testPerson)
	cookie := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": used}, nil, nil))
	service.clock = service.clock.Add(SignInLinkLifetime + time.Second)
	if answer := service.do(t, "POST", "/redeem", map[string]string{"secret": unused}, nil, nil); answer.Code != http.StatusGone {
		t.Fatalf("an expired link = %d, want 410", answer.Code)
	}
	service.clock = service.clock.Add(SessionLifetime)
	if answer := service.do(t, "GET", "/session", nil, nil, cookie); answer.Code != http.StatusUnauthorized || !strings.Contains(answer.Body.String(), "session_expired") {
		t.Fatalf("an expired session = %d %s", answer.Code, answer.Body)
	}
}

func TestTheAdministratorRevokesASession(t *testing.T) {
	service := newTestService(t)
	cookie := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": service.makeLink(t, testPerson)}, nil, nil))
	listed := service.do(t, "GET", "/sessions?principal_id="+testPerson, nil, asAdministrator, nil)
	var sessions []Session
	json.Unmarshal(listed.Body.Bytes(), &sessions)
	if listed.Code != http.StatusOK || len(sessions) != 1 || strings.Contains(listed.Body.String(), cookie.Value) {
		t.Fatalf("GET /sessions = %d %s: one session, never its secret", listed.Code, listed.Body)
	}
	if answer := service.do(t, "POST", "/sessions/"+sessions[0].ID+"/revoke", map[string]string{"reason": "lost phone"}, asAdministrator, nil); answer.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", answer.Code, answer.Body)
	}
	if answer := service.do(t, "GET", "/session", nil, nil, cookie); answer.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked session = %d, want 401", answer.Code)
	}
}

// TestALinkAskedForByDiscordNameReachesTheirOwnChat: the link goes to the
// one-to-one room, never the group, and works; an unknown name gets the same
// answer and nothing is sent.
func TestALinkAskedForByDiscordNameReachesTheirOwnChat(t *testing.T) {
	service := newTestService(t)
	answer := service.do(t, "POST", "/link-requests", map[string]string{"handle": "@viewer"}, nil, nil)
	if answer.Code != http.StatusAccepted {
		t.Fatalf("link request = %d %s", answer.Code, answer.Body)
	}
	if len(service.world.sent) != 1 || !strings.HasPrefix(service.world.sent[0], "!direct:matrix ") {
		t.Fatalf("sent = %v, want one message to the direct room", service.world.sent)
	}
	secret := service.world.sent[0][strings.Index(service.world.sent[0], "#")+1:]
	if redeemed := service.do(t, "POST", "/redeem", map[string]string{"secret": secret}, nil, nil); redeemed.Code != http.StatusOK {
		t.Fatalf("redeeming the sent link = %d %s", redeemed.Code, redeemed.Body)
	}

	unknown := service.do(t, "POST", "/link-requests", map[string]string{"handle": "nobody"}, nil, nil)
	if unknown.Code != http.StatusAccepted || unknown.Body.String() != answer.Body.String() || len(service.world.sent) != 1 {
		t.Fatalf("an unknown name = %d %s, sent %d: the same answer, nothing sent", unknown.Code, unknown.Body, len(service.world.sent))
	}
	for range linkRequestsPerHour {
		service.do(t, "POST", "/link-requests", map[string]string{"handle": "someone"}, nil, nil)
	}
	if limited := service.do(t, "POST", "/link-requests", map[string]string{"handle": "another"}, nil, nil); limited.Code != http.StatusTooManyRequests {
		t.Fatalf("past the hourly limit for one address = %d, want 429", limited.Code)
	}
}

func TestHandleKind(t *testing.T) {
	for handle, want := range map[string]string{"+1 (206) 555-0100": SentByPhone + " +12065550100", "@nrmh": SentByDiscord + " nrmh", "2065550100": SentByDiscord + " 2065550100"} {
		kind, value := handleKind(handle)
		if kind+" "+value != want {
			t.Errorf("handleKind(%q) = %s %s, want %s", handle, kind, value, want)
		}
	}
}

func TestOnlyALoopbackCallerMayNameTheClientAddress(t *testing.T) {
	request := httptest.NewRequest("POST", "/link-requests", nil)
	request.RemoteAddr = "198.51.100.4:1000"
	request.Header.Set("X-Real-IP", "10.0.0.1")
	if got := clientAddress(request); got != "198.51.100.4" {
		t.Errorf("a remote caller's X-Real-IP was believed: %s", got)
	}
	request.RemoteAddr = "127.0.0.1:1000"
	if got := clientAddress(request); got != "10.0.0.1" {
		t.Errorf("nginx's X-Real-IP was not read: %s", got)
	}
}

func TestADiscordMessageReachesThePersonPointingAtThePrincipal(t *testing.T) {
	service := newTestService(t)
	body := map[string]string{"body": "Member recommends Dune (2021): example.test/library/movies"}
	if answer := service.do(t, "POST", "/principals/"+testPerson+"/discord-messages", body, map[string]string{testAdministratorHeader: testPerson}, nil); answer.Code != http.StatusForbidden {
		t.Fatalf("from a principal who is not an administrator = %d, want 403", answer.Code)
	}
	answer := service.do(t, "POST", "/principals/"+testPerson+"/discord-messages", body, asAdministrator, nil)
	if answer.Code != http.StatusOK {
		t.Fatalf("send = %d %s", answer.Code, answer.Body)
	}
	if len(service.world.sent) != 1 || service.world.sent[0] != "!direct:matrix "+body["body"] {
		t.Fatalf("sent = %v, want the body in the direct room", service.world.sent)
	}
	if empty := service.do(t, "POST", "/principals/"+testPerson+"/discord-messages", map[string]string{"body": " "}, asAdministrator, nil); empty.Code != http.StatusBadRequest {
		t.Errorf("an empty body = %d, want 400", empty.Code)
	}
	if nobody := service.do(t, "POST", "/principals/"+testAdministrator+"/discord-messages", body, asAdministrator, nil); nobody.Code != http.StatusNotFound {
		t.Errorf("a principal no person points at = %d %s, want 404", nobody.Code, nobody.Body)
	}
	if unknown := service.do(t, "POST", "/principals/principal_000999/discord-messages", body, asAdministrator, nil); unknown.Code != http.StatusNotFound {
		t.Errorf("an unknown principal = %d %s, want 404", unknown.Code, unknown.Body)
	}
	if len(service.world.sent) != 1 {
		t.Errorf("refused sends still sent: %v", service.world.sent)
	}
}
