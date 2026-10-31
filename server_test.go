package signinstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testServiceToken    = "a-service-token-that-is-long-enough-for-the-check"
	testAdministrator   = "principal_000001"
	testPerson          = "principal_000002"
	testGroup           = "principal_000006"
	testMultichatToken  = "multichat-bearer"
	testDiscordAccount  = "@discord_1234:matrix.kayushkin.com"
	testDiscordRedirect = "https://kayushkin.com/api/sign-in/discord/callback"
)

// fakeWorld stands in for principal-store, people-store, auth-store and
// multichat, and records what multichat was asked to send.
type fakeWorld struct {
	mutex      sync.Mutex
	principals map[string]map[string]any
	sent       []string
	server     *httptest.Server
	// discordBindings maps a Discord user id to the principal bound to it.
	discordBindings map[string]string
	linkedBy        map[string]string
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	world := &fakeWorld{principals: map[string]map[string]any{
		testAdministrator: {"id": testAdministrator, "kind": "human", "display_name": "Operator", "disabled_at": 0, "is_administrator": true, "groups": []any{}},
		testPerson:        {"id": testPerson, "kind": "human", "display_name": "Viewer", "disabled_at": 0, "is_administrator": false, "groups": []any{map[string]any{"id": "principal_000130"}}},
		testGroup:         {"id": testGroup, "kind": "group", "display_name": "Library members", "disabled_at": 0, "is_administrator": false, "members": []any{}},
	}}
	world.discordBindings, world.linkedBy = map[string]string{}, map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity-bindings/resolve", func(w http.ResponseWriter, r *http.Request) {
		world.mutex.Lock()
		principalID, bound := world.discordBindings[r.URL.Query().Get("subject")]
		record := world.principals[principalID]
		world.mutex.Unlock()
		if r.URL.Query().Get("provider") != "discord" || r.URL.Query().Get("tenant_id") != "" || !bound {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"binding": map[string]any{"id": "identity_binding_000001"}, "principal": record})
	})
	mux.HandleFunc("POST /identity-bindings", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		world.mutex.Lock()
		defer world.mutex.Unlock()
		if _, bound := world.discordBindings[body["subject"]]; bound {
			http.Error(w, `{"error":"identity already bound"}`, http.StatusConflict)
			return
		}
		world.discordBindings[body["subject"]] = body["principal_id"]
		world.linkedBy[body["subject"]] = body["linked_by"]
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"identity_binding_000001"}`)
	})
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
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		world.mutex.Lock()
		record, known := world.principals[r.PathValue("id")]
		world.mutex.Unlock()
		if !known {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(record)
	})
	mux.HandleFunc("GET /identities/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") == "discord" && r.URL.Query().Get("value") == "viewer" {
			json.NewEncoder(w).Encode(map[string]any{"person": map[string]any{"id": "person_000002"}})
			return
		}
		http.Error(w, `{"error":"no such identity"}`, http.StatusNotFound)
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

func (world *fakeWorld) setPrincipalField(principalID, field string, value any) {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.principals[principalID][field] = value
}

type testService struct {
	handler   http.Handler
	store     *Store
	directory *Directory
	world     *fakeWorld
	clock     time.Time
}

func newTestService(t *testing.T) *testService {
	t.Helper()
	world := newFakeWorld(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	directory := NewDirectory(DirectoryConfig{PrincipalStoreURL: world.server.URL, PeopleStoreURL: world.server.URL, MultichatURL: world.server.URL,
		AuthStoreURL: world.server.URL, AuthStoreToken: "auth-store-token", MultichatCredentialID: "cred_multichat"})
	service := &testService{store: store, directory: directory, world: world, clock: time.Now()}
	store.now = func() time.Time { return service.clock }
	directory.now = func() time.Time { return service.clock }
	mux := http.NewServeMux()
	RegisterHandlers(mux, store, directory, ServerConfig{ServiceToken: testServiceToken, SignInPageURL: "https://kayushkin.com/signin", CookieDomain: "kayushkin.com",
		Discord: DiscordConfig{ApplicationID: "discord-app", ClientSecretCredentialID: "cred_discord", RedirectURL: testDiscordRedirect}})
	previousAuthorize, previousAPI := discordAuthorizeURL, discordAPIBase
	discordAuthorizeURL, discordAPIBase = world.server.URL+"/oauth2/authorize", world.server.URL
	t.Cleanup(func() { discordAuthorizeURL, discordAPIBase = previousAuthorize, previousAPI })
	service.handler = mux
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

var asAdministrator = map[string]string{ServiceTokenHeader: testServiceToken, PrincipalIDHeader: testAdministrator}

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
	if made.Link.MadeBy != testAdministrator || !strings.HasPrefix(made.URL, "https://kayushkin.com/signin#") {
		t.Fatalf("made = %s", answer.Body)
	}
	return strings.TrimPrefix(made.URL, "https://kayushkin.com/signin#")
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
	for _, want := range []string{"Domain=kayushkin.com", "HttpOnly", "Secure", "SameSite=Lax"} {
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

func TestTheAdministratorRoutesNeedTheTokenAndAnAdministrator(t *testing.T) {
	service := newTestService(t)
	body := map[string]string{"principal_id": testPerson}
	if answer := service.do(t, "POST", "/sign-in-links", body, nil, nil); answer.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", answer.Code)
	}
	if answer := service.do(t, "POST", "/sign-in-links", body, map[string]string{ServiceTokenHeader: "wrong"}, nil); answer.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", answer.Code)
	}
	if answer := service.do(t, "POST", "/sign-in-links", body, map[string]string{ServiceTokenHeader: testServiceToken}, nil); answer.Code != http.StatusForbidden {
		t.Errorf("token with no administrator = %d, want 403", answer.Code)
	}
	notAdministrator := map[string]string{ServiceTokenHeader: testServiceToken, PrincipalIDHeader: testPerson}
	if answer := service.do(t, "POST", "/sign-in-links", body, notAdministrator, nil); answer.Code != http.StatusForbidden {
		t.Errorf("token naming a non-administrator = %d, want 403", answer.Code)
	}
	if answer := service.do(t, "POST", "/sign-in-links", map[string]string{"principal_id": testGroup}, asAdministrator, nil); answer.Code != http.StatusBadRequest {
		t.Errorf("a link for a group = %d, want 400", answer.Code)
	}
}

func TestADisabledPersonIsSignedOutOnceTheCacheExpires(t *testing.T) {
	service := newTestService(t)
	cookie := sessionCookieFrom(t, service.do(t, "POST", "/redeem", map[string]string{"secret": service.makeLink(t, testPerson)}, nil, nil))
	service.world.setPrincipalField(testPerson, "disabled_at", 1790000000)
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
