package grantstore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWith(t, newFakeDirectory(), &fakeResourceChecker{})
}

func newTestServerWith(t *testing.T, directory PrincipalDirectory, checker ResourceChecker) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mountForTest(NewHandlers(newTestStore(t), directory, checker)))
	t.Cleanup(srv.Close)
	return srv
}

// The headers a test sends to say who its request acts as. They exist only in
// these tests: the mounting server puts a verified caller on the context, and
// nothing a client sends reaches it. mountForTest plays that server's part.
const (
	testInternalServiceHeader = "X-Test-Internal-Service"
	testPrincipalHeader       = "X-Test-Principal-Id"
)

// mountForTest registers every route at the paths the standalone store served
// and sets the caller from the test headers.
func mountForTest(h *Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /relations", h.Relations)
	mux.HandleFunc("GET /resource-types", h.ResourceTypes)
	mux.HandleFunc("GET /grants", h.ListGrants)
	mux.HandleFunc("POST /grants", h.CreateGrant)
	mux.HandleFunc("GET /grants/{id}", h.GetGrant)
	mux.HandleFunc("POST /grants/{id}/revoke", h.RevokeGrant)
	mux.HandleFunc("GET /principals/{id}/effective", h.Effective)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internal := r.Header.Get(testInternalServiceHeader) != ""
		principalID := r.Header.Get(testPrincipalHeader)
		if internal || principalID != "" {
			r = r.WithContext(ContextWithCaller(r.Context(), Caller{InternalService: internal, PrincipalID: principalID}))
		}
		mux.ServeHTTP(w, r)
	})
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
	// Every route is gated, so the plain helper acts as the internal service.
	// The per-principal rules have their own helper in
	// principal_enforcement_test.go.
	req.Header.Set(testInternalServiceHeader, "yes")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func postGrant(t *testing.T, srv *httptest.Server, principalID, relation, resourceType, resourceID string) Grant {
	t.Helper()
	status, body := do(t, srv, "POST", "/grants", map[string]any{
		"principal_id": principalID, "relation": relation, "resource_type": resourceType, "resource_id": resourceID,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /grants = %d: %s", status, body)
	}
	var g Grant
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return g
}

func decodeGrants(t *testing.T, body []byte) []Grant {
	t.Helper()
	var out []Grant
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return out
}

func TestVocabularyRoutes(t *testing.T) {
	srv := newTestServer(t)
	status, body := do(t, srv, "GET", "/resource-types", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `["agent","instance","machine","skill","tool","board","operation_type","principal","peer_connection","reference_type","reference","mail_account"]` {
		t.Fatalf("GET /resource-types = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/relations", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /relations = %d: %s", status, body)
	}
	var relations []RelationDefinition
	if err := json.Unmarshal(body, &relations); err != nil {
		t.Fatal(err)
	}
	if len(relations) != 14 || relations[12].Name != "can_read_mail" || relations[13].Name != "can_import_mail" || relations[12].AcceptsAllResources ||
		relations[13].ResourceTypes[0] != "mail_account" || relations[0].Name != "can_use" || !relations[0].Enforced || relations[11].Name != "works_with" || relations[11].Enforced || relations[7].Name != "can_observe_sessions_on" ||
		relations[10].Name != "can_see_references" || !relations[10].AcceptsAllResources || relations[10].ResourceTypes[0] != "reference_type" || relations[10].ResourceTypes[1] != "reference" ||
		relations[8].Name != "can_start_sessions_as" || relations[8].AcceptsAllResources || relations[8].ResourceTypes[0] != "principal" ||
		relations[9].Name != "can_call_through" || !relations[9].AcceptsAllResources || relations[9].ResourceTypes[0] != "peer_connection" ||
		relations[6].Name != "can_run_operation" || relations[6].ResourceTypes[0] != "operation_type" ||
		relations[3].Name != "can_view" || relations[4].Name != "can_edit" || relations[5].Name != "can_administer" || relations[3].ResourceTypes[0] != "board" {
		t.Fatalf("relations = %+v", relations)
	}
	status, body = do(t, srv, "GET", "/health", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"grants":0`) {
		t.Fatalf("GET /health = %d: %s", status, body)
	}
}

func TestPostThenGetListRevoke(t *testing.T) {
	srv := newTestServer(t)
	g := postGrant(t, srv, "principal_000001", "can_use", "tool", "13")
	if g.ID != "grant_000001" || g.RevokedAt != 0 {
		t.Fatalf("created %+v", g)
	}

	// Repeat is 200 with the same row.
	status, body := do(t, srv, "POST", "/grants", map[string]any{
		"principal_id": "principal_000001", "relation": "can_use", "resource_type": "tool", "resource_id": "13",
	})
	if status != http.StatusOK || !strings.Contains(string(body), `"id":"grant_000001"`) {
		t.Fatalf("repeat POST = %d: %s", status, body)
	}

	status, body = do(t, srv, "GET", "/grants/"+g.ID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"resource_id":"13"`) {
		t.Fatalf("GET = %d: %s", status, body)
	}
	if status, _ := do(t, srv, "GET", "/grants/grant_000404", nil); status != http.StatusNotFound {
		t.Fatalf("GET missing = %d", status)
	}

	status, body = do(t, srv, "GET", "/grants?principal_id=principal_000001&resource_type=tool", nil)
	if status != http.StatusOK || len(decodeGrants(t, body)) != 1 {
		t.Fatalf("list = %d: %s", status, body)
	}

	status, body = do(t, srv, "POST", "/grants/"+g.ID+"/revoke", nil)
	if status != http.StatusOK || strings.Contains(string(body), `"revoked_at":0`) {
		t.Fatalf("revoke = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/grants", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("list after revoke = %d: %s (want [] not null)", status, body)
	}
	status, body = do(t, srv, "GET", "/grants?include_revoked=true", nil)
	if len(decodeGrants(t, body)) != 1 {
		t.Fatalf("include_revoked = %d: %s", status, body)
	}
	if status, _ := do(t, srv, "POST", "/grants/grant_000404/revoke", nil); status != http.StatusNotFound {
		t.Fatalf("revoke missing = %d", status)
	}
}

func TestPostRefusalsMapToTheRightStatus(t *testing.T) {
	srv := newTestServer(t)
	for _, c := range []struct {
		name string
		body map[string]any
		want int
		text string
	}{
		{"unknown field", map[string]any{"principal_id": "principal_000001", "relation": "can_use", "resource_type": "tool", "resource_id": "13", "resouce_id": "13"}, 400, "unknown field"},
		{"bad relation", map[string]any{"principal_id": "principal_000001", "relation": "may_use", "resource_type": "tool", "resource_id": "13"}, 400, "can_use, can_run_as"},
		{"relation/type mismatch", map[string]any{"principal_id": "principal_000001", "relation": "can_run_as", "resource_type": "tool", "resource_id": "13"}, 400, "does not apply"},
		{"unknown principal", map[string]any{"principal_id": "principal_000099", "relation": "can_use", "resource_type": "tool", "resource_id": "13"}, 400, "does not exist in principal-store"},
	} {
		status, body := do(t, srv, "POST", "/grants", c.body)
		if status != c.want || !strings.Contains(string(body), c.text) {
			t.Fatalf("%s: %d %s (want %d saying %q)", c.name, status, body, c.want, c.text)
		}
	}

	missing := &fakeResourceChecker{missing: map[string]bool{"tool/13": true}}
	srv = newTestServerWith(t, newFakeDirectory(), missing)
	status, body := do(t, srv, "POST", "/grants", map[string]any{"principal_id": "principal_000001", "relation": "can_use", "resource_type": "tool", "resource_id": "13"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "tool 13 does not exist in tool-store") {
		t.Fatalf("missing resource: %d %s", status, body)
	}

	down := newFakeDirectory()
	down.failure = io.ErrUnexpectedEOF
	srv = newTestServerWith(t, down, &fakeResourceChecker{})
	status, body = do(t, srv, "POST", "/grants", map[string]any{"principal_id": "principal_000001", "relation": "can_use", "resource_type": "tool", "resource_id": "13"})
	if status != http.StatusBadGateway {
		t.Fatalf("directory down: %d %s", status, body)
	}
	status, body = do(t, srv, "GET", "/principals/principal_000001/effective", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("effective with directory down: %d %s (want 502, never an empty list)", status, body)
	}
}

func TestEffectiveRoute(t *testing.T) {
	srv := newTestServer(t)
	postGrant(t, srv, "principal_000001", "can_use", "tool", "13")
	postGrant(t, srv, "principal_000006", "can_use", "tool", "14")
	postGrant(t, srv, "principal_000006", "can_run_as", "agent", "65")

	status, body := do(t, srv, "GET", "/principals/principal_000001/effective", nil)
	if status != http.StatusOK {
		t.Fatalf("effective = %d: %s", status, body)
	}
	grants := decodeGrants(t, body)
	// Ordered by resource_type first, so the group's agent grant leads, then
	// tool 13 (own) before tool 14 (inherited).
	if len(grants) != 3 || grants[0].ResourceType != "agent" || grants[1].PrincipalID != "principal_000001" || grants[2].PrincipalID != "principal_000006" {
		t.Fatalf("effective = %+v", grants)
	}
	status, body = do(t, srv, "GET", "/principals/principal_000001/effective?relation=can_use&resource_type=tool", nil)
	if len(decodeGrants(t, body)) != 2 {
		t.Fatalf("filtered effective = %d: %s", status, body)
	}
	if status, _ := do(t, srv, "GET", "/principals/principal_000099/effective", nil); status != http.StatusNotFound {
		t.Fatalf("unknown principal = %d", status)
	}
	if status, _ := do(t, srv, "GET", "/principals/principal_000001/effective?relation=nope", nil); status != http.StatusBadRequest {
		t.Fatalf("bad relation filter = %d", status)
	}
}

func TestNewHandlersRefusesANilOwner(t *testing.T) {
	for _, c := range []struct {
		name      string
		directory PrincipalDirectory
		checker   ResourceChecker
	}{{"nil directory", nil, &fakeResourceChecker{}}, {"nil checker", newFakeDirectory(), nil}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: NewHandlers did not panic", c.name)
				}
			}()
			NewHandlers(New(nil), c.directory, c.checker)
		}()
	}
}
