package grants

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// In the fake directory principal_000001 is a human in group principal_000006,
// principal_000002 is a human in no group, and principal_000006 is a group.
const (
	boardAdministratorThroughGroup = "principal_000001"
	plainMember                    = "principal_000002"
	administratorsGroup            = "principal_000006"
)

func newEnforcingTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newEnforcingTestServerWith(t, newFakeDirectory())
}

func newEnforcingTestServerWith(t *testing.T, directory PrincipalDirectory) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mountForTest(NewHandlers(newTestStore(t), directory, &fakeResourceChecker{})))
	t.Cleanup(srv.Close)
	return srv
}

func doWithHeaders(t *testing.T, srv *httptest.Server, headers map[string]string, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

var asGrantStoreService = map[string]string{testInternalServiceHeader: "yes"}

func asGrantStorePrincipal(principalID string) map[string]string {
	return map[string]string{testPrincipalHeader: principalID}
}

func boardGrantBody(principalID, relation, boardID string) map[string]any {
	return map[string]any{"principal_id": principalID, "relation": relation, "resource_type": "board", "resource_id": boardID}
}

func expectStatus(t *testing.T, want int, got int, body []byte, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: want %d, got %d: %s", what, want, got, body)
	}
}

func TestEnforcingGrantStoreIdentifiesTheCaller(t *testing.T) {
	srv := newEnforcingTestServer(t)
	status, body := doWithHeaders(t, srv, nil, "GET", "/health", nil)
	expectStatus(t, 200, status, body, "health is open")
	status, body = doWithHeaders(t, srv, nil, "GET", "/grants?principal_id=principal_000001", nil)
	expectStatus(t, 401, status, body, "no identity")
	status, body = doWithHeaders(t, srv, map[string]string{testInternalServiceHeader: "yes", testPrincipalHeader: plainMember}, "GET", "/grants", nil)
	expectStatus(t, 500, status, body, "a caller that is both the internal service and a principal is a wiring bug")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal("Dana"), "GET", "/grants?principal_id=Dana", nil)
	expectStatus(t, 401, status, body, "a name is not a principal id")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal("principal_000099"), "GET", "/grants?principal_id=principal_000099", nil)
	expectStatus(t, 401, status, body, "unknown principal")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(administratorsGroup), "GET", "/grants?principal_id="+administratorsGroup, nil)
	expectStatus(t, 401, status, body, "a group does not act")
	status, body = doWithHeaders(t, srv, asGrantStoreService, "GET", "/grants", nil)
	expectStatus(t, 200, status, body, "service lists everything")
}

func TestPrincipalGrantsOnlyOnBoardsItAdministers(t *testing.T) {
	srv := newEnforcingTestServer(t)
	status, body := doWithHeaders(t, srv, asGrantStoreService, "POST", "/grants", boardGrantBody(administratorsGroup, "can_administer", "board-a"))
	expectStatus(t, 201, status, body, "service makes the group administer board-a")
	var groupAdministration Grant
	json.Unmarshal(body, &groupAdministration)

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "POST", "/grants", boardGrantBody(plainMember, "can_view", "board-a"))
	expectStatus(t, 201, status, body, "administrator through the group grants view on board-a")
	var memberView Grant
	json.Unmarshal(body, &memberView)

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "POST", "/grants", boardGrantBody(plainMember, "can_edit", "board-a"))
	expectStatus(t, 403, status, body, "a viewer promotes itself")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "POST", "/grants", boardGrantBody(plainMember, "can_view", "board-b"))
	expectStatus(t, 403, status, body, "administrator of board-a grants on board-b")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "POST", "/grants",
		map[string]any{"principal_id": plainMember, "relation": "can_use", "resource_type": "tool", "resource_id": "13"})
	expectStatus(t, 403, status, body, "a principal grants a tool")

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/grants/"+memberView.ID, nil)
	expectStatus(t, 200, status, body, "member reads its own grant")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/grants/"+groupAdministration.ID, nil)
	expectStatus(t, 404, status, body, "member reads another's grant on a board it does not administer")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "GET", "/grants/"+groupAdministration.ID, nil)
	expectStatus(t, 200, status, body, "administrator reads a grant on its board")

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/grants?resource_type=board&resource_id=board-a", nil)
	expectStatus(t, 403, status, body, "member lists board-a grants")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "GET", "/grants?resource_type=board&resource_id=board-a", nil)
	expectStatus(t, 200, status, body, "administrator lists board-a grants")
	if grants := decodeGrants(t, body); len(grants) != 2 {
		t.Fatalf("board-a should carry two grants, got %+v", grants)
	}
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/grants?principal_id="+plainMember, nil)
	expectStatus(t, 200, status, body, "member lists its own grants")

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/principals/"+boardAdministratorThroughGroup+"/effective", nil)
	expectStatus(t, 403, status, body, "member reads someone else's effective grants")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/principals/"+plainMember+"/effective", nil)
	expectStatus(t, 200, status, body, "member reads its own effective grants")

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "POST", "/grants/"+memberView.ID+"/revoke", nil)
	expectStatus(t, 403, status, body, "member revokes its own view grant without administering the board")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(boardAdministratorThroughGroup), "POST", "/grants/"+memberView.ID+"/revoke", nil)
	expectStatus(t, 200, status, body, "administrator revokes")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "POST", "/grants/"+groupAdministration.ID+"/revoke", nil)
	expectStatus(t, 404, status, body, "member revokes a grant it cannot read")
}

// principal_000008 in the fake directory carries is_administrator.
const deploymentAdministrator = "principal_000008"

// TestAHeaderNamesNoCaller: the standalone store's own headers are just
// headers now, and a request carrying only them has no caller.
func TestAHeaderNamesNoCaller(t *testing.T) {
	srv := newEnforcingTestServer(t)
	status, body := doWithHeaders(t, srv, map[string]string{"X-Principal-Id": deploymentAdministrator, "X-Grant-Store-Service-Token": "anything"}, "GET", "/grants", nil)
	expectStatus(t, 401, status, body, "the old identity headers")
}

// TestAnAdministratorIsPastEveryRule pins the one fact that lets a person reach
// a grant nobody gave them: administering the deployment includes handing out
// and taking back access to it.
func TestAnAdministratorIsPastEveryRule(t *testing.T) {
	srv := newEnforcingTestServer(t)
	status, body := doWithHeaders(t, srv, asGrantStorePrincipal(deploymentAdministrator), "POST", "/grants",
		boardGrantBody(plainMember, "can_view", "board-nobody-granted-them"))
	expectStatus(t, 201, status, body, "an administrator grants on a board it was never granted")
	var granted Grant
	json.Unmarshal(body, &granted)

	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(deploymentAdministrator), "GET", "/grants", nil)
	expectStatus(t, 200, status, body, "an administrator lists every grant")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(deploymentAdministrator), "GET", "/principals/"+plainMember+"/effective", nil)
	expectStatus(t, 200, status, body, "an administrator reads someone else's effective grants")
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(deploymentAdministrator), "POST", "/grants/"+granted.ID+"/revoke", nil)
	expectStatus(t, 200, status, body, "an administrator revokes")

	// And a principal who is not an administrator is refused the same calls.
	status, body = doWithHeaders(t, srv, asGrantStorePrincipal(plainMember), "GET", "/principals/"+deploymentAdministrator+"/effective", nil)
	expectStatus(t, 403, status, body, "a plain principal reads an administrator's effective grants")
}
