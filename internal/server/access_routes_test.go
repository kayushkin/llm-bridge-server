package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/permissions"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/access/signin"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// accessTestServer is a server whose access stores are real, on a Postgres
// database of the test's own, with three people in principal-store: a member
// (principal_000001), an administrator (principal_000002) and a person the
// tests try to change (principal_000003).
type accessTestServer struct {
	server     *Server
	stores     *access.Stores
	instanceID string
	// loginCookies holds a login cookie for the member and the
	// administrator, made through demo login, which reads principal-store
	// in process.
	memberCookie, administratorCookie *http.Cookie
}

const (
	accessTestMemberID        = "principal_000001"
	accessTestAdministratorID = "principal_000002"
	accessTestTargetID        = "principal_000003"
)

func newAccessTestServer(t *testing.T) *accessTestServer {
	t.Helper()
	server, _, instanceID := testServerWithInstance(t, msg.HarnessMock)
	modules, err := access.Modules()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, modules...)
	stores := access.StoresOnPools(database.Open(modules[0]), database.Open(modules[1]), database.Open(modules[2]), database.Open(modules[3]))
	server.cfg.SignInPageURL = "https://sign-in.example.test/signin"
	server.cfg.SignInCookieDomain = "example.test"
	server.EnableAccess(stores)

	ctx := context.Background()
	for _, name := range []string{"Member", "Administrator", "Target"} {
		if _, err := stores.Principals.Create(ctx, &principals.Principal{Kind: "human", DisplayName: name}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	administrator := true
	if _, err := stores.Principals.Patch(ctx, accessTestAdministratorID, principals.Patch{IsAdministrator: &administrator}); err != nil {
		t.Fatalf("make the administrator: %v", err)
	}
	test := &accessTestServer{server: server, stores: stores, instanceID: instanceID}
	test.memberCookie = loginCookieFrom(t, demoLogin(t, server, accessTestMemberID))
	test.administratorCookie = loginCookieFrom(t, demoLogin(t, server, accessTestAdministratorID))
	return test
}

// accessTestCaller is one way of calling the server.
type accessTestCaller struct {
	name    string
	present func(test *accessTestServer, request *http.Request)
}

var (
	callerWithNoCredential = accessTestCaller{"no credential", func(*accessTestServer, *http.Request) {}}
	// A non-administrator, as dash calls for one of its signed-in users.
	callerMemberThroughDash = accessTestCaller{"member through dash", func(_ *accessTestServer, request *http.Request) {
		request.Header.Set(serviceTokenHeader, testServiceToken)
		request.Header.Set(principalIdentityHeader, accessTestMemberID)
	}}
	// A non-administrator signed in to this server, carrying every store
	// token a client might forge.
	callerMemberWithLoginCookie = accessTestCaller{"member with a login cookie and forged store tokens", func(test *accessTestServer, request *http.Request) {
		addLoginCookie(request, test.memberCookie)
		request.Header.Set("X-Grant-Store-Service-Token", "forged")
		request.Header.Set("X-Sign-In-Store-Service-Token", "forged")
	}}
	callerAdministratorThroughDash = accessTestCaller{"administrator through dash", func(_ *accessTestServer, request *http.Request) {
		request.Header.Set(serviceTokenHeader, testServiceToken)
		request.Header.Set(principalIdentityHeader, accessTestAdministratorID)
	}}
	callerAdministratorWithLoginCookie = accessTestCaller{"administrator with a login cookie", func(test *accessTestServer, request *http.Request) {
		addLoginCookie(request, test.administratorCookie)
	}}
	callerServiceTokenAlone = accessTestCaller{"the service token naming nobody", func(_ *accessTestServer, request *http.Request) {
		request.Header.Set(serviceTokenHeader, testServiceToken)
	}}
	callersWhoAreNotAdministrators = []accessTestCaller{callerWithNoCredential, callerMemberThroughDash, callerMemberWithLoginCookie}
)

func (test *accessTestServer) call(t *testing.T, caller accessTestCaller, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	caller.present(test, request)
	return serve(test.server, request)
}

// expectRefused checks that a caller who is not an administrator is refused
// with the status the gate gives it: 401 with no credential, 403
// operator_route for a member.
func expectRefusedToEveryoneButAnAdministrator(t *testing.T, test *accessTestServer, method, path, body string) {
	t.Helper()
	for _, caller := range callersWhoAreNotAdministrators {
		response := test.call(t, caller, method, path, body)
		want := http.StatusForbidden
		if caller.name == callerWithNoCredential.name {
			want = http.StatusUnauthorized
		}
		if response.Code != want {
			t.Errorf("%s %s as %s = %d %s, want %d", method, path, caller.name, response.Code, response.Body, want)
		}
		if want == http.StatusForbidden && !strings.Contains(response.Body.String(), "operator_route") {
			t.Errorf("%s %s as %s refused with %s, want operator_route", method, path, caller.name, response.Body)
		}
	}
}

func TestOnlyAnAdministratorCreatesOrChangesAPrincipal(t *testing.T) {
	test := newAccessTestServer(t)
	ctx := context.Background()

	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/principals/principals", `{"kind":"human","display_name":"Intruder"}`)
	expectRefusedToEveryoneButAnAdministrator(t, test, "PATCH", "/principals/principals/"+accessTestTargetID, `{"is_administrator":true}`)
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/principals/principals/"+accessTestAdministratorID+"/disable", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "PUT", "/principals/principals/"+accessTestTargetID+"/members/"+accessTestMemberID, "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/principals/contacts/resolve", `{"email":"someone@example.test","display_name":"Someone"}`)
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/principals/principals", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/principals/principals/"+accessTestAdministratorID, "")

	listed, err := test.stores.Principals.List(ctx, principals.Filter{IncludeDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("principal-store holds %d principals after the refused calls, want the 3 seeded", len(listed))
	}
	target, err := test.stores.Principals.Get(ctx, accessTestTargetID, false)
	if err != nil || target.IsAdministrator {
		t.Fatalf("target after the refused PATCH: %+v %v, want not an administrator", target, err)
	}
	administrator, err := test.stores.Principals.Get(ctx, accessTestAdministratorID, false)
	if err != nil || administrator.DisabledAt != 0 {
		t.Fatalf("administrator after the refused disable: %+v %v, want enabled", administrator, err)
	}

	for _, caller := range []accessTestCaller{callerAdministratorThroughDash, callerAdministratorWithLoginCookie, callerServiceTokenAlone} {
		if response := test.call(t, caller, "POST", "/principals/principals", `{"kind":"human","display_name":"Made by `+caller.name+`"}`); response.Code != http.StatusCreated {
			t.Errorf("POST /principals/principals as %s = %d %s, want 201", caller.name, response.Code, response.Body)
		}
	}
	if response := test.call(t, callerAdministratorWithLoginCookie, "PATCH", "/principals/principals/"+accessTestTargetID, `{"is_administrator":true}`); response.Code != http.StatusOK {
		t.Fatalf("PATCH as the administrator = %d %s, want 200", response.Code, response.Body)
	}
}

// The vocabularies are the only principal-store routes a member reads.
func TestAMemberReadsPrincipalStoresVocabulariesOnly(t *testing.T) {
	test := newAccessTestServer(t)
	for _, path := range []string{"/principals/kinds", "/principals/kind-definitions", "/principals/availability-reasons",
		"/principals/weekday-codes", "/principals/identity-providers"} {
		if response := test.call(t, callerWithNoCredential, "GET", path, ""); response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401", path, response.Code)
		}
		for _, caller := range []accessTestCaller{callerMemberThroughDash, callerMemberWithLoginCookie} {
			if response := test.call(t, caller, "GET", path, ""); response.Code != http.StatusOK {
				t.Errorf("GET %s as %s = %d %s, want 200", path, caller.name, response.Code, response.Body)
			}
		}
	}
}

// Disabling someone through the mounted store refuses their next request at
// once: the write forgets the cached principal.
func TestADisableThroughTheMountedStoreTakesEffectOnTheNextRequest(t *testing.T) {
	test := newAccessTestServer(t)
	if response := test.call(t, callerMemberWithLoginCookie, "GET", "/principals/kinds", ""); response.Code != http.StatusOK {
		t.Fatalf("member before = %d %s", response.Code, response.Body)
	}
	if response := test.call(t, callerAdministratorThroughDash, "POST", "/principals/principals/"+accessTestMemberID+"/disable", ""); response.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", response.Code, response.Body)
	}
	if response := test.call(t, callerMemberWithLoginCookie, "GET", "/principals/kinds", ""); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "principal_disabled") {
		t.Fatalf("member after the disable = %d %s, want 403 principal_disabled", response.Code, response.Body)
	}
}

// Linking an identity through /principals/ stamps the caller as linker,
// as /admin/identity-bindings does, and refuses a body naming one.
func TestAnIdentityLinkThroughThePrincipalStoreRoutesRecordsTheCaller(t *testing.T) {
	test := newAccessTestServer(t)
	link := `{"principal_id":"` + accessTestMemberID + `","provider":"discord","tenant_id":"","subject":"123456789012345678"}`
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/principals/identity-bindings", link)
	forged := `{"principal_id":"` + accessTestMemberID + `","provider":"discord","tenant_id":"","subject":"123456789012345678","linked_by":"` + accessTestTargetID + `"}`
	if response := test.call(t, callerAdministratorThroughDash, "POST", "/principals/identity-bindings", forged); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "linked_by_not_accepted") {
		t.Fatalf("a link naming linked_by = %d %s, want 400 linked_by_not_accepted", response.Code, response.Body)
	}
	response := test.call(t, callerAdministratorThroughDash, "POST", "/principals/identity-bindings", link)
	if response.Code != http.StatusCreated {
		t.Fatalf("link as the administrator = %d %s, want 201", response.Code, response.Body)
	}
	var binding struct {
		LinkedBy string `json:"linked_by"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &binding); err != nil || binding.LinkedBy != accessTestAdministratorID {
		t.Fatalf("linked_by = %q (%v), want the calling administrator %s", binding.LinkedBy, err, accessTestAdministratorID)
	}
}

func TestOnlyAnAdministratorOrTheInternalServiceChangesAGrant(t *testing.T) {
	test := newAccessTestServer(t)
	ctx := context.Background()
	grant := `{"principal_id":"` + accessTestMemberID + `","relation":"can_dispatch_on","resource_type":"instance","resource_id":"` + test.instanceID + `"}`

	if response := test.call(t, callerWithNoCredential, "POST", "/grants/grants", grant); response.Code != http.StatusUnauthorized {
		t.Errorf("POST /grants/grants with no credential = %d %s, want 401", response.Code, response.Body)
	}
	// A member reaches grant-store, which refuses every grant but one on a
	// board the member administers; a forged grant-store token changes
	// nothing, because grant-store reads its caller from the request context.
	for _, caller := range []accessTestCaller{callerMemberThroughDash, callerMemberWithLoginCookie} {
		if response := test.call(t, caller, "POST", "/grants/grants", grant); response.Code != http.StatusForbidden {
			t.Errorf("POST /grants/grants as %s = %d %s, want 403", caller.name, response.Code, response.Body)
		}
	}
	granted, err := test.stores.Grants.List(ctx, grants.Filter{PrincipalID: accessTestMemberID})
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 0 {
		t.Fatalf("the member holds %d grants after the refused calls, want none", len(granted))
	}

	response := test.call(t, callerAdministratorWithLoginCookie, "POST", "/grants/grants", grant)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /grants/grants as the administrator = %d %s, want 201", response.Code, response.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created grant %s: %v", response.Body, err)
	}
	if response := test.call(t, callerServiceTokenAlone, "POST", "/grants/grants",
		`{"principal_id":"`+accessTestTargetID+`","relation":"can_dispatch_on","resource_type":"instance","resource_id":"`+test.instanceID+`"}`); response.Code != http.StatusCreated {
		t.Fatalf("POST /grants/grants as the internal service = %d %s, want 201", response.Code, response.Body)
	}

	// The grant is read in process at spawn: the member is now held to it.
	ids, err := test.server.grantChecker.EffectiveGrantResourceIDs(ctx, accessTestMemberID, "can_dispatch_on", "instance")
	if err != nil || len(ids) != 1 || ids[0] != test.instanceID {
		t.Fatalf("the member's effective can_dispatch_on = %v %v, want [%s]", ids, err, test.instanceID)
	}

	// A member may not revoke it, nor read another principal's grants.
	for _, caller := range []accessTestCaller{callerMemberThroughDash, callerMemberWithLoginCookie} {
		if response := test.call(t, caller, "POST", "/grants/grants/"+created.ID+"/revoke", ""); response.Code != http.StatusForbidden {
			t.Errorf("revoke as %s = %d %s, want 403", caller.name, response.Code, response.Body)
		}
		if response := test.call(t, caller, "GET", "/grants/grants?principal_id="+accessTestTargetID, ""); response.Code != http.StatusForbidden {
			t.Errorf("another's grants as %s = %d %s, want 403", caller.name, response.Code, response.Body)
		}
		if response := test.call(t, caller, "GET", "/grants/principals/"+accessTestMemberID+"/effective", ""); response.Code != http.StatusOK {
			t.Errorf("own effective grants as %s = %d %s, want 200", caller.name, response.Code, response.Body)
		}
	}
	if response := test.call(t, callerAdministratorThroughDash, "POST", "/grants/grants/"+created.ID+"/revoke", ""); response.Code != http.StatusOK {
		t.Fatalf("revoke as the administrator = %d %s, want 200", response.Code, response.Body)
	}
}

func TestOnlyAnAdministratorMintsASignInLink(t *testing.T) {
	test := newAccessTestServer(t)
	ctx := context.Background()
	makeLink := `{"principal_id":"` + accessTestMemberID + `"}`

	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/sign-in/sign-in-links", makeLink)
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/sign-in/sign-in-links", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/sign-in/sessions", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/sign-in/sessions/session_000001/revoke", `{"reason":"test"}`)
	// The service token naming nobody passes the gate, and sign-in-store
	// refuses it: every link records the administrator who made it.
	if response := test.call(t, callerServiceTokenAlone, "POST", "/sign-in/sign-in-links", makeLink); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "administrator_required") {
		t.Errorf("POST /sign-in/sign-in-links as the service token alone = %d %s, want 403 administrator_required", response.Code, response.Body)
	}
	links, err := test.stores.SignIn.ListSignInLinks(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("sign-in-store holds %d links after the refused calls, want none", len(links))
	}

	response := test.call(t, callerAdministratorWithLoginCookie, "POST", "/sign-in/sign-in-links", makeLink)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /sign-in/sign-in-links as the administrator = %d %s, want 201", response.Code, response.Body)
	}
	var made struct {
		URL  string            `json:"url"`
		Link signin.SignInLink `json:"link"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &made); err != nil {
		t.Fatalf("link answer %s: %v", response.Body, err)
	}
	if made.Link.MadeBy != accessTestAdministratorID {
		t.Fatalf("made_by = %q, want the calling administrator; answer %s", made.Link.MadeBy, response.Body)
	}

	// The browser routes are open: anyone holding the secret redeems it, once,
	// and the cookie it sets signs the member in.
	address, err := url.Parse(made.URL)
	if err != nil || address.Fragment == "" {
		t.Fatalf("link URL %q carries no secret after #", made.URL)
	}
	redeem := test.call(t, callerWithNoCredential, "POST", "/sign-in/redeem", `{"secret":"`+address.Fragment+`"}`)
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s, want 200", redeem.Code, redeem.Body)
	}
	if again := test.call(t, callerWithNoCredential, "POST", "/sign-in/redeem", `{"secret":"`+address.Fragment+`"}`); again.Code == http.StatusOK {
		t.Fatalf("a second redeem of one link = 200 %s, want a refusal", again.Body)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range redeem.Result().Cookies() {
		if cookie.Name == signin.SessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatalf("redeem set no %s cookie", signin.SessionCookieName)
	}
	request := httptest.NewRequest("GET", "/sign-in/session", nil)
	request.AddCookie(sessionCookie)
	session := serve(test.server, request)
	if session.Code != http.StatusOK || !strings.Contains(session.Body.String(), accessTestMemberID) {
		t.Fatalf("GET /sign-in/session with the cookie = %d %s, want 200 naming %s", session.Code, session.Body, accessTestMemberID)
	}
	if anonymous := test.call(t, callerWithNoCredential, "GET", "/sign-in/session", ""); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("GET /sign-in/session with no cookie = %d %s, want 401", anonymous.Code, anonymous.Body)
	}
}

func TestOnlyAnAdministratorChangesOrReadsPermissionRules(t *testing.T) {
	test := newAccessTestServer(t)
	rule := `{"id":"test-deny-rm","scope":"global","priority":1,"tool":"Bash","pattern":"^rm ","outcome":"deny","enabled":true}`
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/permissions/rules", rule)
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/permissions/rules", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "GET", "/permissions/audit", "")
	expectRefusedToEveryoneButAnAdministrator(t, test, "POST", "/permissions/evaluate", `{"tool":"Bash","input":{"command":"ls"}}`)
	if response := test.call(t, callerAdministratorWithLoginCookie, "POST", "/permissions/rules", rule); response.Code != http.StatusCreated {
		t.Fatalf("POST /permissions/rules as the administrator = %d %s, want 201", response.Code, response.Body)
	}
	// The prehook asks the same engine, so the new rule binds the next verdict.
	verdict, err := test.server.permissionEngine.Evaluate(context.Background(), permissions.EvaluateRequest{Tool: "Bash", Input: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)})
	if err != nil || verdict.Outcome != "deny" || verdict.MatchedRuleID != "test-deny-rm" {
		t.Fatalf("verdict after the rule = %+v %v, want deny by test-deny-rm", verdict, err)
	}
}

// A server whose access stores are not open answers their routes 503.
func TestAccessRoutesAnswer503WhenAccessIsOff(t *testing.T) {
	server, _ := testServer(t)
	for _, path := range []string{"/principals/kinds", "/grants/relations", "/sign-in/health", "/permissions/rules"} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set(serviceTokenHeader, testServiceToken)
		if response := serve(server, request); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "access_off") {
			t.Errorf("GET %s with access off = %d %s, want 503 access_off", path, response.Code, response.Body)
		}
	}
}
