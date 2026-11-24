package principalstore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registerTestRoutes mounts the handlers at the paths the bridge serves them
// under /principal-store/, without the prefix.
func registerTestRoutes(mux *http.ServeMux, s *Store) {
	h := NewHandlers(s)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /kinds", h.Kinds)
	mux.HandleFunc("GET /kind-definitions", h.KindDefinitions)
	mux.HandleFunc("GET /availability-reasons", h.AvailabilityReasons)
	mux.HandleFunc("GET /weekday-codes", h.WeekdayCodes)
	mux.HandleFunc("GET /principals", h.ListPrincipals)
	mux.HandleFunc("POST /principals", h.CreatePrincipal)
	mux.HandleFunc("POST /contacts/resolve", h.ResolveContact)
	mux.HandleFunc("GET /principals/{id}", h.GetPrincipal)
	mux.HandleFunc("PATCH /principals/{id}", h.PatchPrincipal)
	mux.HandleFunc("POST /principals/{id}/disable", h.DisablePrincipal)
	mux.HandleFunc("POST /principals/{id}/enable", h.EnablePrincipal)
	mux.HandleFunc("GET /principals/{id}/members", h.ListMembers)
	mux.HandleFunc("PUT /principals/{group}/members/{member}", h.PutMember)
	mux.HandleFunc("DELETE /principals/{group}/members/{member}", h.DeleteMember)
	mux.HandleFunc("GET /principals/{id}/groups", h.ListGroups)
	mux.HandleFunc("GET /principals/{id}/availability", h.Availability)
	mux.HandleFunc("GET /principals/{id}/time-off", h.ListTimeOff)
	mux.HandleFunc("POST /principals/{id}/time-off", h.AddTimeOff)
	mux.HandleFunc("DELETE /principals/{id}/time-off/{timeOffID}", h.DeleteTimeOff)
	mux.HandleFunc("GET /identity-providers", h.IdentityProviders)
	mux.HandleFunc("GET /identity-bindings", h.ListIdentityBindings)
	mux.HandleFunc("POST /identity-bindings", h.LinkIdentity)
	mux.HandleFunc("GET /identity-bindings/resolve", h.ResolveIdentity)
	mux.HandleFunc("GET /identity-bindings/{id}", h.GetIdentityBinding)
	mux.HandleFunc("POST /identity-bindings/{id}/unlink", h.UnlinkIdentity)
}

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := newTestStore(t)
	mux := http.NewServeMux()
	registerTestRoutes(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func post(t *testing.T, srv *httptest.Server, kind, displayName, email string) Principal {
	t.Helper()
	status, body := do(t, srv, "POST", "/principals", map[string]any{
		"kind": kind, "display_name": displayName, "email": email,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /principals = %d: %s", status, body)
	}
	var p Principal
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

func TestPostCreatesAndGetReadsBack(t *testing.T) {
	srv, _ := newTestServer(t)
	dana := post(t, srv, "human", "Dana Example", "dana@example.test")
	if dana.ID != "principal_000001" {
		t.Fatalf("id = %q", dana.ID)
	}
	status, body := do(t, srv, "GET", "/principals/"+dana.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET = %d: %s", status, body)
	}
	// A human always answers with a groups array, even an empty one, and
	// never with members.
	if !strings.Contains(string(body), `"groups":[]`) || strings.Contains(string(body), `"members"`) {
		t.Fatalf("human body should carry groups:[] and no members, got: %s", body)
	}
	data := post(t, srv, "group", "Data Team", "")
	status, body = do(t, srv, "GET", "/principals/"+data.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"members":[]`) || strings.Contains(string(body), `"groups"`) {
		t.Fatalf("group body should carry members:[] and no groups, got %d: %s", status, body)
	}
	status, _ = do(t, srv, "GET", "/principals/principal_999999", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing = %d, want 404", status)
	}
}

func TestPostRejectsUnknownKindWithTheVocabulary(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "POST", "/principals", map[string]any{"kind": "robot", "display_name": "Marvin"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "human, group") {
		t.Fatalf("400 should name the vocabulary, got: %s", body)
	}
	status, body = do(t, srv, "POST", "/principals", map[string]any{"kind": "human", "display_name": "X", "emial": "x@y"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "emial") {
		t.Fatalf("unknown create field = %d: %s", status, body)
	}
}

func TestKindsRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "GET", "/kinds", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `["human","group","contact","peer","service"]` {
		t.Fatalf("GET /kinds = %d: %s", status, body)
	}
}

func TestPatchRefusesUnknownAndProtectedFields(t *testing.T) {
	srv, _ := newTestServer(t)
	dana := post(t, srv, "human", "Dana Example", "")

	status, body := do(t, srv, "PATCH", "/principals/"+dana.ID, map[string]any{"display_nmae": "Dana"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "display_nmae") {
		t.Fatalf("unknown field = %d: %s", status, body)
	}
	status, body = do(t, srv, "PATCH", "/principals/"+dana.ID, map[string]any{"kind": "group"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "fixed at creation") {
		t.Fatalf("kind = %d: %s", status, body)
	}
	status, body = do(t, srv, "PATCH", "/principals/"+dana.ID, map[string]any{"disabled_at": 1})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "/disable") {
		t.Fatalf("disabled_at = %d: %s", status, body)
	}
	status, body = do(t, srv, "PATCH", "/principals/"+dana.ID, map[string]any{"display_name": "Dana Example", "email": "dana@example.test"})
	if status != http.StatusOK || !strings.Contains(string(body), "Dana Example") {
		t.Fatalf("rename = %d: %s", status, body)
	}
	status, _ = do(t, srv, "PATCH", "/principals/principal_999999", map[string]any{"email": "x"})
	if status != http.StatusNotFound {
		t.Fatalf("patch missing = %d, want 404", status)
	}
}

func TestMembershipRoutes(t *testing.T) {
	srv, _ := newTestServer(t)
	priya := post(t, srv, "human", "Priya Raman", "")
	data := post(t, srv, "group", "Data Team", "")
	security := post(t, srv, "group", "Security", "")

	status, body := do(t, srv, "PUT", "/principals/"+data.ID+"/members/"+priya.ID, nil)
	if status != http.StatusCreated || !strings.Contains(string(body), `"created":true`) {
		t.Fatalf("first PUT = %d: %s", status, body)
	}
	status, body = do(t, srv, "PUT", "/principals/"+data.ID+"/members/"+priya.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"created":false`) {
		t.Fatalf("second PUT = %d: %s", status, body)
	}
	// Kind enforcement over HTTP.
	status, body = do(t, srv, "PUT", "/principals/"+priya.ID+"/members/"+data.ID, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "not a group") {
		t.Fatalf("human as group = %d: %s", status, body)
	}
	status, body = do(t, srv, "PUT", "/principals/"+data.ID+"/members/"+security.ID, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "nested groups") {
		t.Fatalf("nested group = %d: %s", status, body)
	}
	status, _ = do(t, srv, "GET", "/principals/"+priya.ID+"/members", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("members of a human = %d, want 400", status)
	}

	status, body = do(t, srv, "GET", "/principals/"+data.ID+"/members", nil)
	if status != http.StatusOK || !strings.Contains(string(body), priya.ID) {
		t.Fatalf("members = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals/"+priya.ID+"/groups", nil)
	if status != http.StatusOK || !strings.Contains(string(body), data.ID) {
		t.Fatalf("groups = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals/"+data.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"members":[{`) {
		t.Fatalf("get group should embed members = %d: %s", status, body)
	}

	status, _ = do(t, srv, "DELETE", "/principals/"+data.ID+"/members/"+priya.ID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", status)
	}
	status, _ = do(t, srv, "DELETE", "/principals/"+data.ID+"/members/"+priya.ID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", status)
	}
}

func TestDisableHidesFromListButNotFromGet(t *testing.T) {
	srv, _ := newTestServer(t)
	marcus := post(t, srv, "human", "Marcus Feld", "")
	post(t, srv, "human", "Helena Vos", "")

	status, body := do(t, srv, "POST", "/principals/"+marcus.ID+"/disable", nil)
	if status != http.StatusOK || strings.Contains(string(body), `"disabled_at":0`) {
		t.Fatalf("disable = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals", nil)
	if status != http.StatusOK || strings.Contains(string(body), marcus.ID) {
		t.Fatalf("default list shows a disabled principal: %s", body)
	}
	status, body = do(t, srv, "GET", "/principals?include_disabled=true", nil)
	if status != http.StatusOK || !strings.Contains(string(body), marcus.ID) {
		t.Fatalf("include_disabled list hides him: %s", body)
	}
	status, body = do(t, srv, "GET", "/principals/"+marcus.ID, nil)
	if status != http.StatusOK || strings.Contains(string(body), `"disabled_at":0`) {
		t.Fatalf("GET of a disabled principal = %d: %s", status, body)
	}
	status, body = do(t, srv, "POST", "/principals/"+marcus.ID+"/enable", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"disabled_at":0`) {
		t.Fatalf("enable = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/health", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"principals":2`) || !strings.Contains(string(body), `"disabled":0`) {
		t.Fatalf("health = %d: %s", status, body)
	}
}

func TestListSearchesAndFilters(t *testing.T) {
	srv, _ := newTestServer(t)
	post(t, srv, "human", "Priya Raman", "priya.raman@northwind-eng.example")
	post(t, srv, "human", "Dinesh Okonkwo", "dinesh.okonkwo@northwind-eng.example")
	post(t, srv, "group", "Data Team", "")

	status, body := do(t, srv, "GET", "/principals?q=pri", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "Priya Raman") || strings.Contains(string(body), "Dinesh") {
		t.Fatalf("q=pri = %d: %s", status, body)
	}
	// The list is a bare array, per the contract.
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		t.Fatalf("list should be a bare array: %s", body)
	}
	status, body = do(t, srv, "GET", "/principals?kind=group", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "Data Team") || strings.Contains(string(body), "Priya") {
		t.Fatalf("kind=group = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals?kind=robot", nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "human, group") {
		t.Fatalf("kind=robot = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals?limit=1&offset=1", nil)
	if status != http.StatusOK || strings.Count(string(body), `"id":`) != 1 {
		t.Fatalf("limit/offset = %d: %s", status, body)
	}
}

// TestPatchPromotesAnAdministratorOverHTTP pins the field through the HTTP
// layer's allowlist: the store accepted it while the handler refused it as an
// unknown field, so a promotion answered 400 and nobody was promoted.
func TestPatchPromotesAnAdministratorOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	human := post(t, srv, "human", "Dana Example", "dana@example.test")
	status, body := do(t, srv, "PATCH", "/principals/"+human.ID, map[string]any{"is_administrator": true})
	if status != http.StatusOK {
		t.Fatalf("promote = %d: %s", status, body)
	}
	var promoted Principal
	if err := json.Unmarshal(body, &promoted); err != nil {
		t.Fatal(err)
	}
	if !promoted.IsAdministrator {
		t.Fatalf("is_administrator did not take: %s", body)
	}
	status, body = do(t, srv, "GET", "/principals/"+human.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"is_administrator":true`) {
		t.Fatalf("read back = %d: %s", status, body)
	}
	group := post(t, srv, "group", "Operators", "")
	status, body = do(t, srv, "PATCH", "/principals/"+group.ID, map[string]any{"is_administrator": true})
	if status != http.StatusBadRequest {
		t.Fatalf("a group must be refused, got %d: %s", status, body)
	}
}
