package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeGrantStoreForPrincipals answers GET /principals/{id}/effective for every
// principal in grantsByPrincipal (keyed "relation/resource_type"), and 404 for
// any other.
func fakeGrantStoreForPrincipals(t *testing.T, grantsByPrincipal map[string]map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		grants, known := grantsByPrincipal[r.PathValue("id")]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		key := r.URL.Query().Get("relation") + "/" + r.URL.Query().Get("resource_type")
		rows := []map[string]string{}
		for _, resourceID := range grants[key] {
			rows = append(rows, map[string]string{"id": "grant_000001", "principal_id": r.PathValue("id"),
				"relation": r.URL.Query().Get("relation"), "resource_type": r.URL.Query().Get("resource_type"), "resource_id": resourceID})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

const (
	firstTestPrincipalID = "principal_000001"
	// secondTestPrincipalID is principal-store's administrator in the gated
	// test server, so a check that an administrator is unrestricted and a
	// check that a principal is restricted use different ids.
	secondTestPrincipalID        = "principal_000002"
	administratorTestPrincipalID = "principal_000003"
	disabledTestPrincipalID      = "principal_000009"
	groupTestPrincipalID         = "principal_000006"
	unknownTestPrincipalID       = "principal_000404"
)

// gatedTestServer is a server with one enabled mock instance ("inst_test" on
// machine "m_test") and a second one ("inst_second" on "m_second"), a
// principal directory knowing two ordinary humans, an administrator, a
// disabled human and a group, and a grant-store whose grants the caller
// supplies.
type gatedTestServer struct {
	server *Server
	store  *store.Store
	kanban *fakeKanbanStore
	// principals is the directory the server reads a caller from; a test
	// rewrites a record in it to demote or disable somebody.
	principals *fakePrincipalDirectory
	// clock is what the principal lookup cache reads the time from, so a test
	// can let a cached answer expire without sleeping.
	clock *testClock
}

// testClock is a time source a test moves by hand.
type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) advance(by time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(by)
}

// gatedTestServerPublicURL is the gateway URL session agents are given.
const gatedTestServerPublicURL = "https://gateway.example.test"

func newGatedTestServer(t *testing.T, grantsByPrincipal map[string]map[string][]string) *gatedTestServer {
	t.Helper()
	directory := t.TempDir()
	bridgeStore, err := store.New(filepath.Join(directory, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { bridgeStore.Close() })
	harnesses, err := harnessstore.Open(filepath.Join(directory, "harness.db"))
	if err != nil {
		t.Fatalf("open harness-store: %v", err)
	}
	t.Cleanup(func() { harnesses.Close() })
	for _, seed := range []struct{ machineID, instanceID string }{{"m_test", "inst_test"}, {"m_second", "inst_second"}} {
		if err := harnesses.CreateMachine(&msg.Machine{ID: seed.machineID, Name: seed.machineID, Hostname: "secret-host.internal", SSHKeyPath: "/home/operator/.ssh/id", Transport: msg.TransportLocal}); err != nil {
			t.Fatalf("seed machine: %v", err)
		}
		if err := harnesses.CreateInstance(&msg.Instance{ID: seed.instanceID, HarnessType: msg.HarnessMock, Name: seed.instanceID, MachineID: seed.machineID, Enabled: true}); err != nil {
			t.Fatalf("seed instance: %v", err)
		}
	}
	principalRecords := map[string]string{
		firstTestPrincipalID:         `{"id":"principal_000001","kind":"human","display_name":"One","disabled_at":0,"is_administrator":false,"groups":[]}`,
		secondTestPrincipalID:        `{"id":"principal_000002","kind":"human","display_name":"Two","disabled_at":0,"is_administrator":false,"groups":[]}`,
		administratorTestPrincipalID: `{"id":"principal_000003","kind":"human","display_name":"Root","disabled_at":0,"is_administrator":true,"groups":[]}`,
		groupTestPrincipalID:         `{"id":"principal_000006","kind":"group","display_name":"Data Team","disabled_at":0,"is_administrator":false,"members":[]}`,
		disabledTestPrincipalID:      `{"id":"principal_000009","kind":"human","display_name":"Gone","disabled_at":1788980427,"is_administrator":true,"groups":[]}`,
	}
	principals := newFakePrincipalDirectory(t, principalRecords)
	kanban := newFakeKanbanStore(t)
	if grantsByPrincipal == nil {
		grantsByPrincipal = map[string]map[string][]string{firstTestPrincipalID: {}, secondTestPrincipalID: {}}
	}
	if grantsByPrincipal[administratorTestPrincipalID] == nil {
		grantsByPrincipal[administratorTestPrincipalID] = map[string][]string{}
	}
	cfg := &config.Config{
		ImagesDir:         filepath.Join(directory, "images"),
		BridgePrefsPath:   filepath.Join(directory, "prefs.json"),
		LogStoreURL:       "http://localhost:0",
		PrincipalStoreURL: principals.server.URL,
		GrantStoreURL:     fakeGrantStoreForPrincipals(t, grantsByPrincipal).URL,
		KanbanStoreURL:    kanban.server.URL,
		PublicURL:         gatedTestServerPublicURL,
	}
	testAuthorizationConfig(cfg)
	gated := &gatedTestServer{
		server:     New(bridgeStore, nil, nil, harnesses, nil, testModelStore(t), nil, cfg),
		store:      bridgeStore,
		kanban:     kanban,
		principals: principals,
		clock:      &testClock{now: time.Now()},
	}
	gated.server.principalLookupCache.now = gated.clock.Now
	return gated
}

// requestAs sends a request with the given login cookie (nil for none).
func (gated *gatedTestServer) requestAs(t *testing.T, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	return serve(gated.server, request)
}

func (gated *gatedTestServer) requestAsService(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	return serve(gated.server, request)
}

// requestAsServiceAssertingPrincipal is how dash calls this server for one of
// its logged-in users: its own service token plus the principal it says the
// request is for.
func (gated *gatedTestServer) requestAsServiceAssertingPrincipal(t *testing.T, principalID, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, principalID)
	return serve(gated.server, request)
}

// loginAs signs in and returns the cookie.
func (gated *gatedTestServer) loginAs(t *testing.T, principalID string) *http.Cookie {
	t.Helper()
	return loginCookieFrom(t, demoLogin(t, gated.server, principalID))
}

func (gated *gatedTestServer) createSessionAs(t *testing.T, cookie *http.Cookie, principalID string) *store.Session {
	t.Helper()
	response := gated.requestAs(t, cookie, "POST", "/sessions", msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: "inst_test", PrincipalID: principalID,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s, want 201", response.Code, response.Body.String())
	}
	var created store.Session
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return &created
}

func sessionIDsOfList(t *testing.T, body []byte) []string {
	t.Helper()
	var sessions []store.Session
	if err := json.Unmarshal(body, &sessions); err != nil {
		t.Fatalf("decode session list %s: %v", body, err)
	}
	ids := []string{}
	for _, session := range sessions {
		ids = append(ids, session.SessionID)
	}
	sort.Strings(ids)
	return ids
}

func TestTwoPrincipalsEachSeeAndReachOnlyTheirOwnSessions(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	firstCookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	secondCookie := loginCookieFrom(t, demoLogin(t, gated.server, secondTestPrincipalID))

	// The body names no principal; the session is created as the caller.
	firstSession := gated.createSessionAs(t, firstCookie, "")
	secondSession := gated.createSessionAs(t, secondCookie, secondTestPrincipalID)
	if firstSession.PrincipalID != firstTestPrincipalID || secondSession.PrincipalID != secondTestPrincipalID {
		t.Fatalf("sessions created as %q and %q, want each caller", firstSession.PrincipalID, secondSession.PrincipalID)
	}
	// A session nobody owns, created by the operator.
	if err := gated.store.CreateSession(&store.Session{SessionID: "br_unowned", Harness: msg.HarnessMock, InstanceID: "inst_test", State: string(msg.SessionIdle)}); err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct {
		cookie *http.Cookie
		own    *store.Session
		other  *store.Session
	}{{firstCookie, firstSession, secondSession}, {secondCookie, secondSession, firstSession}} {
		listed := gated.requestAs(t, check.cookie, "GET", "/sessions", nil)
		if got := sessionIDsOfList(t, listed.Body.Bytes()); len(got) != 1 || got[0] != check.own.SessionID {
			t.Errorf("%s lists %v, want only %s", check.own.PrincipalID, got, check.own.SessionID)
		}
		summary := gated.requestAs(t, check.cookie, "GET", "/sessions/summary", nil)
		var summaryBody SummaryResponse
		if err := json.Unmarshal(summary.Body.Bytes(), &summaryBody); err != nil || len(summaryBody.Sessions) != 1 || summaryBody.Sessions[0].SessionID != check.own.SessionID {
			t.Errorf("%s summary = %s, want only its own session", check.own.PrincipalID, summary.Body.String())
		}
		lookup := gated.requestAs(t, check.cookie, "POST", "/sessions/summary", map[string]any{"session_ids": []string{check.other.SessionID, "br_unowned"}})
		if err := json.Unmarshal(lookup.Body.Bytes(), &summaryBody); err != nil || len(summaryBody.Sessions) != 0 {
			t.Errorf("%s looked up another principal's and an unowned session by id and got %s", check.own.PrincipalID, lookup.Body.String())
		}

		if response := gated.requestAs(t, check.cookie, "GET", "/sessions/"+check.own.SessionID, nil); response.Code != http.StatusOK {
			t.Errorf("%s GET own session = %d, want 200", check.own.PrincipalID, response.Code)
		}
		for _, route := range []struct{ method, path string }{
			{"GET", "/sessions/" + check.other.SessionID},
			{"POST", "/sessions/" + check.other.SessionID + "/send"},
			{"POST", "/sessions/" + check.other.SessionID + "/stop"},
			{"POST", "/sessions/" + check.other.SessionID + "/rename"},
			{"POST", "/sessions/" + check.other.SessionID + "/fork"},
			{"GET", "/sessions/" + check.other.SessionID + "/events"},
			{"GET", "/sessions/" + check.other.SessionID + "/messages"},
			{"GET", "/sessions/" + check.other.SessionID + "/signals"},
			{"GET", "/sessions/br_unowned"},
			{"GET", "/sessions/br_does_not_exist"},
		} {
			response := gated.requestAs(t, check.cookie, route.method, route.path, map[string]string{})
			if response.Code != http.StatusNotFound {
				t.Errorf("%s %s %s = %d %s, want 404", check.own.PrincipalID, route.method, route.path, response.Code, response.Body.String())
			}
		}
	}

	serviceList := gated.requestAsService(t, "GET", "/sessions")
	if got := sessionIDsOfList(t, serviceList.Body.Bytes()); len(got) != 3 {
		t.Errorf("service token lists %v, want all three sessions", got)
	}
	if response := gated.requestAsService(t, "GET", "/sessions/"+secondSession.SessionID); response.Code != http.StatusOK {
		t.Errorf("service token GET a principal's session = %d, want 200", response.Code)
	}

	// An administrator is a person with a login, and principal-store says they
	// may do anything: they see every session whoever owns it, including the
	// one nobody owns, and the list is not narrowed.
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	administratorList := gated.requestAs(t, administratorCookie, "GET", "/sessions", nil)
	if got := sessionIDsOfList(t, administratorList.Body.Bytes()); len(got) != 3 {
		t.Errorf("an administrator lists %v, want all three sessions", got)
	}
	for _, sessionID := range []string{firstSession.SessionID, secondSession.SessionID, "br_unowned"} {
		if response := gated.requestAs(t, administratorCookie, "GET", "/sessions/"+sessionID, nil); response.Code != http.StatusOK {
			t.Errorf("an administrator GET /sessions/%s = %d %s, want 200", sessionID, response.Code, response.Body.String())
		}
	}
	if response := gated.requestAs(t, administratorCookie, "GET", "/sessions/br_does_not_exist", nil); response.Code != http.StatusNotFound {
		t.Errorf("an administrator GET a session that does not exist = %d, want 404", response.Code)
	}
}

// TestAnAdministratorReachesTheOperatorRoutesAndTheStoreProxies is the other
// half: past the per-resource checks, and identified to the stores all the
// same, because kanban-store and grant-store make their own administrator
// check and this server does not answer it for them.
func TestAnAdministratorReachesTheOperatorRoutesAndTheStoreProxies(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	ordinaryCookie := gated.loginAs(t, firstTestPrincipalID)

	for _, route := range []struct{ method, path string }{
		{"GET", "/bridge-prefs"}, {"GET", "/folders"}, {"GET", "/machines"}, {"GET", "/conformance"},
	} {
		administrator := gated.requestAs(t, administratorCookie, route.method, route.path, nil)
		if administrator.Code != http.StatusOK {
			t.Errorf("an administrator %s %s = %d %s, want 200", route.method, route.path, administrator.Code, administrator.Body.String())
		}
		ordinary := gated.requestAs(t, ordinaryCookie, route.method, route.path, nil)
		if ordinary.Code != http.StatusForbidden || !strings.Contains(ordinary.Body.String(), "operator route: use the service token") {
			t.Errorf("a non-administrator %s %s = %d %s, want 403 operator route", route.method, route.path, ordinary.Code, ordinary.Body.String())
		}
	}

	// The routes answered across every session from a source that cannot be
	// narrowed are per-principal refusals, so an administrator is past them too.
	if response := gated.requestAs(t, ordinaryCookie, "GET", "/sessions/aggregates", nil); response.Code != http.StatusForbidden {
		t.Errorf("a non-administrator GET /sessions/aggregates = %d, want 403", response.Code)
	}
	if response := gated.requestAs(t, administratorCookie, "GET", "/sessions/aggregates", nil); response.Code == http.StatusForbidden {
		t.Errorf("an administrator GET /sessions/aggregates = 403 %s, want the route to answer", response.Body.String())
	}

	if response := gated.requestAs(t, administratorCookie, "GET", "/kanban/boards", nil); response.Code != http.StatusOK {
		t.Fatalf("an administrator GET /kanban/boards = %d %s, want 200", response.Code, response.Body.String())
	}
	recorded := gated.kanban.recorded()
	if len(recorded) != 1 {
		t.Fatalf("kanban-store received %d requests, want 1", len(recorded))
	}
	if got := recorded[0].Header.Values(principalIdentityHeader); len(got) != 1 || got[0] != administratorTestPrincipalID {
		t.Errorf("kanban-store saw %s %v, want [%s]: the store makes its own administrator check", principalIdentityHeader, got, administratorTestPrincipalID)
	}

	// An unclassified route is not a per-resource check. Nobody has decided
	// who may call it, and an administrator cannot decide that by arriving.
	gated.server.mux.HandleFunc("GET /another-route-added-later", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	response := gated.requestAs(t, administratorCookie, "GET", "/another-route-added-later", nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "GET /another-route-added-later") {
		t.Errorf("an administrator on an unclassified route = %d %s, want 403 naming the route", response.Code, response.Body.String())
	}
}

// TestAnAdministratorListsEverySessionWhoeverOwnsIt pins the lists that are
// narrowed in the store. The operator's sidebar went down to two sessions out
// of 65,000 on 2026-09-17: principalRestrictingRequest handed back an
// administrator's id beside restricted=false, and the four callers that read
// only the id filtered on it. The same requests as the service, and as the
// administrator through a trusted caller, must all answer every session.
func TestAnAdministratorListsEverySessionWhoeverOwnsIt(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	firstCookie := gated.loginAs(t, firstTestPrincipalID)

	owned := gated.createSessionAs(t, firstCookie, "")
	if err := gated.store.CreateSession(&store.Session{SessionID: "br_unowned", Harness: msg.HarnessMock, InstanceID: "inst_test", State: string(msg.SessionIdle)}); err != nil {
		t.Fatal(err)
	}
	raised := gated.requestAs(t, firstCookie, "POST", "/sessions/"+owned.SessionID+"/signals", map[string]string{"title": "done", "severity": "info"})
	if raised.Code/100 != 2 {
		t.Fatalf("raise signal = %d %s", raised.Code, raised.Body.String())
	}
	want := []string{owned.SessionID, "br_unowned"}
	sort.Strings(want)

	summaryIDs := func(response *httptest.ResponseRecorder) []string {
		t.Helper()
		var body SummaryResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode summary %s: %v", response.Body.String(), err)
		}
		ids := []string{}
		for _, session := range body.Sessions {
			ids = append(ids, session.SessionID)
		}
		sort.Strings(ids)
		return ids
	}

	for name, got := range map[string][]string{
		"GET /sessions":                      sessionIDsOfList(t, gated.requestAs(t, administratorCookie, "GET", "/sessions", nil).Body.Bytes()),
		"GET /sessions/summary":              summaryIDs(gated.requestAs(t, administratorCookie, "GET", "/sessions/summary", nil)),
		"POST /sessions/summary":             summaryIDs(gated.requestAs(t, administratorCookie, "POST", "/sessions/summary", map[string]int{"limit": 50})),
		"GET /sessions/summary, as asserted": summaryIDs(gated.requestAsServiceAssertingPrincipal(t, administratorTestPrincipalID, "GET", "/sessions/summary")),
	} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("an administrator %s lists %v, want every session %v", name, got, want)
		}
	}

	recentBundle := gated.requestAs(t, administratorCookie, "GET", "/sessions/recent-bundle", nil)
	for _, sessionID := range want {
		if !strings.Contains(recentBundle.Body.String(), sessionID) {
			t.Errorf("an administrator GET /sessions/recent-bundle = %s, want it to carry %s", recentBundle.Body.String(), sessionID)
		}
	}

	var signals []store.Signal
	if err := json.Unmarshal(gated.requestAs(t, administratorCookie, "GET", "/signals", nil).Body.Bytes(), &signals); err != nil || len(signals) != 1 {
		t.Errorf("an administrator GET /signals = %d signals (%v), want the one another principal's session raised", len(signals), err)
	}
}

// TestADemotedAdministratorLosesAccessWhenTheCachedAnswerExpires pins the cost
// of caching principal-store's answer: the demotion bites late, and by no more
// than principalLookupCacheLifetime.
func TestADemotedAdministratorLosesAccessWhenTheCachedAnswerExpires(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := gated.loginAs(t, administratorTestPrincipalID)
	if response := gated.requestAs(t, cookie, "GET", "/bridge-prefs", nil); response.Code != http.StatusOK {
		t.Fatalf("administrator GET /bridge-prefs = %d %s, want 200", response.Code, response.Body.String())
	}

	gated.principals.setRecord(administratorTestPrincipalID,
		`{"id":"principal_000003","kind":"human","display_name":"Root","disabled_at":0,"is_administrator":false,"groups":[]}`)

	// What the cache buys: a page load is many requests and one lookup.
	readsBeforeThePage := gated.principals.readCount()
	for range 10 {
		gated.requestAs(t, cookie, "GET", "/sessions", nil)
	}
	if extraReads := gated.principals.readCount() - readsBeforeThePage; extraReads != 0 {
		t.Errorf("ten requests inside the cache lifetime made %d principal-store lookups, want 0", extraReads)
	}

	gated.clock.advance(principalLookupCacheLifetime - time.Second)
	if response := gated.requestAs(t, cookie, "GET", "/bridge-prefs", nil); response.Code != http.StatusOK {
		t.Errorf("one second before the cached answer expires = %d, want the stale 200 this cache is paid for", response.Code)
	}

	gated.clock.advance(2 * time.Second)
	response := gated.requestAs(t, cookie, "GET", "/bridge-prefs", nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator route: use the service token") {
		t.Errorf("after the cached answer expired = %d %s, want 403 operator route", response.Code, response.Body.String())
	}
	// Disabling bites the same way, and is refused before is_administrator is
	// read — this record says is_administrator true and disabled_at set.
	gated.principals.setRecord(administratorTestPrincipalID,
		`{"id":"principal_000003","kind":"human","display_name":"Root","disabled_at":1788980427,"is_administrator":true,"groups":[]}`)
	gated.clock.advance(principalLookupCacheLifetime)
	response = gated.requestAs(t, cookie, "GET", "/sessions", nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "principal_disabled") {
		t.Errorf("a disabled administrator = %d %s, want 403 principal_disabled", response.Code, response.Body.String())
	}
}

// TestATrustedCallerMayAssertWhichPrincipalARequestIsFor is dash's path: it
// holds the service token and says which of its logged-in users each request
// belongs to.
func TestATrustedCallerMayAssertWhichPrincipalARequestIsFor(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	firstCookie := gated.loginAs(t, firstTestPrincipalID)
	secondCookie := gated.loginAs(t, secondTestPrincipalID)
	firstSession := gated.createSessionAs(t, firstCookie, "")
	secondSession := gated.createSessionAs(t, secondCookie, "")

	// Asserted, the request is that principal's in every respect.
	listed := gated.requestAsServiceAssertingPrincipal(t, firstTestPrincipalID, "GET", "/sessions")
	if got := sessionIDsOfList(t, listed.Body.Bytes()); len(got) != 1 || got[0] != firstSession.SessionID {
		t.Errorf("asserted %s lists %v, want only %s", firstTestPrincipalID, got, firstSession.SessionID)
	}
	if response := gated.requestAsServiceAssertingPrincipal(t, firstTestPrincipalID, "GET", "/sessions/"+secondSession.SessionID); response.Code != http.StatusNotFound {
		t.Errorf("asserted %s reaching another principal's session = %d, want 404", firstTestPrincipalID, response.Code)
	}
	if response := gated.requestAsServiceAssertingPrincipal(t, firstTestPrincipalID, "GET", "/bridge-prefs"); response.Code != http.StatusForbidden {
		t.Errorf("asserted %s on an operator route = %d, want 403: the token's own freedom does not carry over", firstTestPrincipalID, response.Code)
	}
	// The same token with no assertion stays unrestricted.
	if response := gated.requestAsService(t, "GET", "/bridge-prefs"); response.Code != http.StatusOK {
		t.Errorf("service token with no %s on an operator route = %d, want 200", principalIdentityHeader, response.Code)
	}

	// The administrator rule applies to an asserted principal too.
	administratorList := gated.requestAsServiceAssertingPrincipal(t, administratorTestPrincipalID, "GET", "/sessions")
	if got := sessionIDsOfList(t, administratorList.Body.Bytes()); len(got) != 2 {
		t.Errorf("asserted administrator lists %v, want both sessions", got)
	}
	if response := gated.requestAsServiceAssertingPrincipal(t, administratorTestPrincipalID, "GET", "/bridge-prefs"); response.Code != http.StatusOK {
		t.Errorf("asserted administrator on an operator route = %d, want 200", response.Code)
	}

	// The proxy forwards the asserted principal and nothing the client sent.
	if response := gated.requestAsServiceAssertingPrincipal(t, secondTestPrincipalID, "GET", "/kanban/boards"); response.Code != http.StatusOK {
		t.Fatalf("asserted %s on /kanban/boards = %d %s, want 200", secondTestPrincipalID, response.Code, response.Body.String())
	}
	recorded := gated.kanban.recorded()
	if len(recorded) != 1 {
		t.Fatalf("kanban-store received %d requests, want 1", len(recorded))
	}
	if got := recorded[0].Header.Values(principalIdentityHeader); len(got) != 1 || got[0] != secondTestPrincipalID {
		t.Errorf("kanban-store saw %s %v, want exactly [%s]", principalIdentityHeader, got, secondTestPrincipalID)
	}
	for _, key := range recorded[0].HeaderKeyNames {
		if strings.EqualFold(key, serviceTokenHeader) {
			t.Errorf("kanban-store saw this server's service token under %q", key)
		}
	}
}

func TestAnAssertedPrincipalIsCheckedWithPrincipalStore(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for name, testCase := range map[string]struct {
		principalID string
		wantStatus  int
		wantCode    string
	}{
		"unknown":   {unknownTestPrincipalID, http.StatusBadRequest, "unknown_principal"},
		"disabled":  {disabledTestPrincipalID, http.StatusForbidden, "principal_disabled"},
		"a group":   {groupTestPrincipalID, http.StatusBadRequest, "principal_not_human"},
		"not an id": {"Slava Kayushkin", http.StatusBadRequest, "invalid_principal_id"},
	} {
		response := gated.requestAsServiceAssertingPrincipal(t, testCase.principalID, "GET", "/sessions")
		if response.Code != testCase.wantStatus || !strings.Contains(response.Body.String(), testCase.wantCode) {
			t.Errorf("%s: asserted %q = %d %s, want %d %s", name, testCase.principalID, response.Code, response.Body.String(), testCase.wantStatus, testCase.wantCode)
		}
	}

	// principal-store unreachable is a 502, never a caller treated as an
	// ordinary principal and never one treated as an administrator.
	gated.principals.server.Close()
	gated.clock.advance(principalLookupCacheLifetime)
	response := gated.requestAsServiceAssertingPrincipal(t, administratorTestPrincipalID, "GET", "/sessions")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "principal_store_unavailable") {
		t.Errorf("principal-store down = %d %s, want 502 principal_store_unavailable", response.Code, response.Body.String())
	}
}

func TestCreateSessionNamingAnotherPrincipalIsRefused(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	response := gated.requestAs(t, cookie, "POST", "/sessions", msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: "inst_test", PrincipalID: secondTestPrincipalID,
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "principal_mismatch") {
		t.Fatalf("create as another principal = %d %s, want 403 principal_mismatch", response.Code, response.Body.String())
	}
	if sessions, _ := gated.store.ListSessionsPaged(0, 0); len(sessions) != 0 {
		t.Fatalf("a refused create stored %d sessions", len(sessions))
	}
}

func TestCreateSessionAsAPrincipalStillAppliesTheGrantGates(t *testing.T) {
	gated := newGatedTestServer(t, map[string]map[string][]string{
		firstTestPrincipalID: {"can_dispatch_on/instance": {"inst_second"}},
	})
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	response := gated.requestAs(t, cookie, "POST", "/sessions", msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: "inst_test",
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "not_granted") {
		t.Fatalf("create on an instance the grants do not name = %d %s, want 403 not_granted", response.Code, response.Body.String())
	}
}

func TestGatedServerRefusesAnonymousOperatorAndUnclassifiedRequests(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))

	for _, route := range []struct{ method, path string }{
		{"GET", "/sessions"}, {"POST", "/sessions"}, {"GET", "/sessions/br_x"}, {"GET", "/signals"},
		{"GET", "/bridge-prefs"}, {"GET", "/instances"}, {"GET", "/kanban/boards"},
	} {
		if response := gated.requestAs(t, nil, route.method, route.path, nil); response.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", route.method, route.path, response.Code)
		}
	}
	if response := gated.requestAs(t, nil, "GET", "/health", nil); response.Code != http.StatusOK {
		t.Errorf("anonymous GET /health = %d, want 200", response.Code)
	}

	for _, route := range []struct{ method, path string }{
		{"GET", "/bridge-prefs"}, {"PUT", "/bridge-prefs"}, {"POST", "/admin/archive-old"}, {"GET", "/machines"},
		{"GET", "/credentials"}, {"POST", "/bridge/permission-mode"}, {"GET", "/folders"}, {"GET", "/services"},
		{"GET", "/conformance"}, {"GET", "/models"}, {"GET", "/sessions/discover"},
	} {
		response := gated.requestAs(t, cookie, route.method, route.path, nil)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator route: use the service token") {
			t.Errorf("principal %s %s = %d %s, want 403 operator route", route.method, route.path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/sessions/search", "/sessions/aggregates"} {
		if response := gated.requestAs(t, cookie, "GET", path, nil); response.Code != http.StatusForbidden {
			t.Errorf("principal GET %s = %d, want 403", path, response.Code)
		}
	}

	gated.server.mux.HandleFunc("GET /a-route-added-later", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	response := gated.requestAs(t, cookie, "GET", "/a-route-added-later", nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "GET /a-route-added-later") {
		t.Fatalf("unclassified route = %d %s, want 403 naming the route", response.Code, response.Body.String())
	}

	wrongToken := httptest.NewRequest("GET", "/bridge-prefs", nil)
	wrongToken.Header.Set(serviceTokenHeader, "not-the-service-token-but-long-enough-000")
	wrongToken.AddCookie(cookie)
	if response := serve(gated.server, wrongToken); response.Code != http.StatusUnauthorized {
		t.Errorf("wrong service token = %d, want 401 even alongside a valid cookie", response.Code)
	}
	if response := gated.requestAsService(t, "GET", "/bridge-prefs"); response.Code != http.StatusOK {
		t.Errorf("service token GET /bridge-prefs = %d %s, want 200", response.Code, response.Body.String())
	}
}

func TestSignalsAreNarrowedToThePrincipalsSessions(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	firstCookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	secondCookie := loginCookieFrom(t, demoLogin(t, gated.server, secondTestPrincipalID))
	firstSession := gated.createSessionAs(t, firstCookie, "")
	secondSession := gated.createSessionAs(t, secondCookie, "")

	raised := gated.requestAs(t, firstCookie, "POST", "/sessions/"+firstSession.SessionID+"/signals", map[string]string{"title": "done", "severity": "info"})
	if raised.Code/100 != 2 {
		t.Fatalf("raise signal = %d %s", raised.Code, raised.Body.String())
	}
	var signal store.Signal
	if err := json.Unmarshal(raised.Body.Bytes(), &signal); err != nil {
		t.Fatal(err)
	}

	var firstSees, secondSees []store.Signal
	_ = json.Unmarshal(gated.requestAs(t, firstCookie, "GET", "/signals", nil).Body.Bytes(), &firstSees)
	_ = json.Unmarshal(gated.requestAs(t, secondCookie, "GET", "/signals", nil).Body.Bytes(), &secondSees)
	if len(firstSees) != 1 || len(secondSees) != 0 {
		t.Fatalf("signal inbox: first sees %d, second sees %d; want 1 and 0", len(firstSees), len(secondSees))
	}
	if response := gated.requestAs(t, secondCookie, "POST", "/signals/"+signal.ID+"/resolve", map[string]string{"state": "dismissed"}); response.Code != http.StatusNotFound {
		t.Errorf("second principal resolving the first's signal = %d, want 404", response.Code)
	}
	_ = secondSession
}

func TestSessionEventsStreamCarriesOnlyThePrincipalsFrames(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	firstCookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	secondCookie := loginCookieFrom(t, demoLogin(t, gated.server, secondTestPrincipalID))

	listener := httptest.NewServer(gated.server)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", listener.URL+"/session-events", nil)
	request.AddCookie(firstCookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	lines := make(chan string, 100)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	secondSession := gated.createSessionAs(t, secondCookie, "")
	firstSession := gated.createSessionAs(t, firstCookie, "")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatal("stream closed before the first principal's frame arrived")
			}
			if strings.Contains(line, secondSession.SessionID) {
				t.Fatalf("first principal's stream carried a frame about the second's session: %s", line)
			}
			if strings.Contains(line, firstSession.SessionID) {
				return
			}
		case <-deadline:
			t.Fatal("first principal's own session never appeared on its stream")
		}
	}
}

func TestInstancesListIsNarrowedByDispatchGrantsAndHidesMachines(t *testing.T) {
	gated := newGatedTestServer(t, map[string]map[string][]string{
		firstTestPrincipalID:  {"can_dispatch_on/machine": {"m_second"}},
		secondTestPrincipalID: {},
	})
	for _, check := range []struct {
		principalID string
		want        []string
	}{{firstTestPrincipalID, []string{"inst_second"}}, {secondTestPrincipalID, []string{"inst_second", "inst_test"}}} {
		cookie := loginCookieFrom(t, demoLogin(t, gated.server, check.principalID))
		response := gated.requestAs(t, cookie, "GET", "/instances", nil)
		if strings.Contains(response.Body.String(), "secret-host.internal") || strings.Contains(response.Body.String(), ".ssh") {
			t.Errorf("%s instance list exposes machine details: %s", check.principalID, response.Body.String())
		}
		var instances []msg.Instance
		if err := json.Unmarshal(response.Body.Bytes(), &instances); err != nil {
			t.Fatalf("decode %s: %v", response.Body.String(), err)
		}
		got := []string{}
		for _, instance := range instances {
			got = append(got, instance.ID)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(check.want, ",") {
			t.Errorf("%s sees instances %v, want %v", check.principalID, got, check.want)
		}
	}
}

func TestForkAndSubagentPromotionCarryTheParentsPrincipal(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := loginCookieFrom(t, demoLogin(t, gated.server, firstTestPrincipalID))
	parent := gated.createSessionAs(t, cookie, "")
	if err := gated.store.SetHarnessSessionID(parent.SessionID, "mock-harness-session-1"); err != nil {
		t.Fatal(err)
	}

	forkResponse := gated.requestAs(t, cookie, "POST", "/sessions/"+parent.SessionID+"/fork", map[string]string{})
	if forkResponse.Code != http.StatusCreated {
		t.Fatalf("fork = %d %s, want 201", forkResponse.Code, forkResponse.Body.String())
	}
	var forked store.Session
	if err := json.Unmarshal(forkResponse.Body.Bytes(), &forked); err != nil {
		t.Fatal(err)
	}
	stored, err := gated.store.GetSession(forked.SessionID)
	if err != nil || stored.PrincipalID != firstTestPrincipalID {
		t.Fatalf("forked session principal = %q (%v), want %s", stored.PrincipalID, err, firstTestPrincipalID)
	}
	if response := gated.requestAs(t, cookie, "GET", "/sessions/"+forked.SessionID, nil); response.Code != http.StatusOK {
		t.Errorf("the principal cannot reach its own fork: %d", response.Code)
	}

	storedParent, err := gated.store.GetSession(parent.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	subagentID, created, err := gated.store.EnsureSubagentSession(storedParent, "mock-subagent-1", "subagent", "")
	if err != nil || !created {
		t.Fatalf("promote subagent: created=%v err=%v", created, err)
	}
	subagent, err := gated.store.GetSession(subagentID)
	if err != nil || subagent.PrincipalID != firstTestPrincipalID {
		t.Fatalf("promoted subagent principal = %q (%v), want %s", subagent.PrincipalID, err, firstTestPrincipalID)
	}
}

func TestPathValueForPatternReadsTheNamedSegment(t *testing.T) {
	for _, testCase := range []struct{ pattern, path, name, want string }{
		{"GET /sessions/{id}", "/sessions/br_1", "id", "br_1"},
		{"POST /sessions/{id}/hooks/{request_id}/resolve", "/sessions/br_2/hooks/r%2F1/resolve", "request_id", "r/1"},
		{"GET /sessions/{id}/entries/{eventId}", "/sessions/br%2F3/entries/9", "id", "br/3"},
	} {
		got, err := pathValueForPattern(testCase.pattern, testCase.path, testCase.name)
		if err != nil || got != testCase.want {
			t.Errorf("pathValueForPattern(%q, %q, %q) = %q, %v; want %q", testCase.pattern, testCase.path, testCase.name, got, err, testCase.want)
		}
	}
}

// TestEveryRegisteredRouteIsClassified reads every pattern literal this
// package registers and fails for any without an access rule, so a new route
// is classified in the change that adds it rather than discovered as a 403.
// agent-store's and memory-store's routes are registered by those libraries;
// TestEveryAccessRuleNamesARegisteredRoute covers them from the other side.
func TestEveryRegisteredRouteIsClassified(t *testing.T) {
	registration := regexp.MustCompile(`\.(?:HandleFunc|Handle)\(\s*("[^"]+"|kanbanProxyMountPrefix\s*\+\s*"/"|grantStoreProxyMountPrefix\s*\+\s*"/")`)
	sourceFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, sourceFile := range sourceFiles {
		if strings.HasSuffix(sourceFile, "_test.go") {
			continue
		}
		source, err := os.ReadFile(sourceFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range registration.FindAllStringSubmatch(string(source), -1) {
			pattern := match[1]
			switch {
			case strings.HasPrefix(pattern, "kanbanProxyMountPrefix"):
				pattern = kanbanProxyMountPrefix + "/"
			case strings.HasPrefix(pattern, "grantStoreProxyMountPrefix"):
				pattern = grantStoreProxyMountPrefix + "/"
			default:
				pattern = strings.Trim(pattern, `"`)
			}
			found++
			if _, classified := routeAccessRules[pattern]; !classified {
				t.Errorf("%s registers %q, which has no entry in routeAccessRules", sourceFile, pattern)
			}
		}
	}
	if found < 100 {
		t.Fatalf("found only %d route registrations; the pattern that finds them has stopped matching", found)
	}
}

// TestEveryAccessRuleNamesARegisteredRoute fails for a rule whose pattern the
// mux does not have, so a typo in the table cannot leave the real route
// unclassified while the table looks complete.
func TestEveryAccessRuleNamesARegisteredRoute(t *testing.T) {
	server := newServerWithAllStoresConfigured(t, func(cfg *config.Config) {
		cfg.KanbanStoreURL = "http://kanban-store.invalid"
	})
	wildcard := regexp.MustCompile(`\{[^}]+\}`)
	for pattern := range routeAccessRules {
		method, path, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			method, path = "GET", pattern
		}
		concretePath := wildcard.ReplaceAllString(path, "x")
		if strings.HasSuffix(concretePath, "/") {
			concretePath += "x"
		}
		_, matched := server.mux.Handler(httptest.NewRequest(method, concretePath, nil))
		if matched != pattern {
			t.Errorf("access rule %q: %s %s matches registered pattern %q", pattern, method, concretePath, matched)
		}
	}
}
