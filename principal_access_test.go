package mailstack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/mailstack/accounts"
)

// fakeAccessStores answers as principal-store, grant-store and kanban-store
// at once, from maps a test edits between calls.
type fakeAccessStores struct {
	mu            sync.Mutex
	principals    map[string]map[string]any // id → record
	grants        map[string][][3]string    // principal → [relation, resource_type, resource_id]
	ticketVisible map[string]map[string]bool
	down          map[string]bool // "grant-store" / "kanban-store" answer 500
	kanbanCalls   int
}

func newFakeAccessStores(t *testing.T) (*fakeAccessStores, PrincipalAccessStores) {
	fake := &fakeAccessStores{
		principals: map[string]map[string]any{
			"principal_000001": {"kind": "human", "disabled_at": 0, "is_administrator": false},
			"principal_000002": {"kind": "human", "disabled_at": 0, "is_administrator": false},
			"principal_000003": {"kind": "human", "disabled_at": 0, "is_administrator": true},
			"principal_000004": {"kind": "human", "disabled_at": 1790000000, "is_administrator": false},
			"principal_000005": {"kind": "contact", "disabled_at": 0, "is_administrator": false},
			"principal_000006": {"kind": "human", "disabled_at": 0, "is_administrator": false},
		},
		grants:        map[string][][3]string{},
		ticketVisible: map[string]map[string]bool{},
		down:          map[string]bool{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/principals/") && strings.HasSuffix(r.URL.Path, "/effective"):
			if r.Header.Get("X-Grant-Store-Service-Token") != "grant-token" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			if fake.down["grant-store"] {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/principals/"), "/effective")
			out := []map[string]string{}
			for _, g := range fake.grants[id] {
				out = append(out, map[string]string{"relation": g[0], "resource_type": g[1], "resource_id": g[2]})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/principals/"):
			record, ok := fake.principals[strings.TrimPrefix(r.URL.Path, "/principals/")]
			if !ok {
				http.Error(w, "{}", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(record)
		case r.URL.Path == "/api/entities/visible-to-principal":
			fake.kanbanCalls++
			if r.Header.Get("X-Kanban-Store-Service-Token") != "kanban-token" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			if fake.down["kanban-store"] {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			var request struct {
				PrincipalID string `json:"principal_id"`
				Entities    []struct {
					EntityType string `json:"entity_type"`
					EntityRef  string `json:"entity_ref"`
				} `json:"entities"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			var results []map[string]any
			for _, e := range request.Entities {
				results = append(results, map[string]any{"entity_type": e.EntityType, "entity_ref": e.EntityRef,
					"visible": e.EntityType == "email" && fake.ticketVisible[request.PrincipalID][e.EntityRef], "card_ids": []string{}})
			}
			json.NewEncoder(w).Encode(map[string]any{"results": results})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	return fake, PrincipalAccessStores{PrincipalStoreURL: server.URL, GrantStoreURL: server.URL, GrantStoreServiceToken: "grant-token",
		KanbanStoreURL: server.URL, KanbanStoreServiceToken: "kanban-token"}
}

func (f *fakeAccessStores) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// accessTestServer has two local accounts, each with the two exported
// messages, and the fake stores behind it.
func accessTestServer(t *testing.T, withStores bool) (*Server, *fakeAccessStores, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	store, err := accounts.Open(filepath.Join(dir, "accounts.db"))
	if err != nil {
		t.Fatalf("open accounts: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	for _, id := range []string{"desk-a", "desk-b"} {
		if err := store.Save(&accounts.Account{ID: id, Label: id, Provider: accounts.ProviderLocal, Address: id + "@demo.invalid", Enabled: true}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	fake, stores := newFakeAccessStores(t)
	options := []ServerOption{WithLocalMailDB(filepath.Join(dir, "local-mail.db"))}
	if withStores {
		options = append(options, WithPrincipalAccess(stores))
	}
	s := NewServer(store, nil, testSettings(t, ""), options...)
	t.Cleanup(func() { s.Shutdown(nil) })
	ids := map[string]string{}
	for _, account := range []string{"desk-a", "desk-b"} {
		got := importInto(t, s, account, twoExportedMessages)
		ids[account+"/0"], ids[account+"/1"] = got.Outcomes[0].ID, got.Outcomes[1].ID
	}
	return s, fake, ids
}

func as(principal string) map[string]string { return map[string]string{PrincipalIDHeader: principal} }

func TestAMailGrantOpensItsAccountAndNothingElse(t *testing.T) {
	s, fake, ids := accessTestServer(t, true)
	fake.set(func() { fake.grants["principal_000001"] = [][3]string{{"can_read_mail", "mail_account", "desk-a"}} })
	who := as("principal_000001")

	for path, want := range map[string]int{
		"/api/messages/" + ids["desk-a/0"] + "/metadata?account=desk-a":         200,
		"/api/messages/" + ids["desk-a/0"] + "/body?account=desk-a&format=html": 200,
		"/api/messages?account=desk-a":                                          200,
		"/api/accounts/desk-a":                                                  200,
		"/api/messages/" + ids["desk-b/0"] + "/metadata?account=desk-b":         404,
		"/api/messages?account=desk-b":                                          403,
		"/api/accounts/desk-a/import-sources":                                   403,
		"/api/search?q=pr%C3%BCfen":                                             403,
		"/settings":                                                             403,
	} {
		if w := serve(t, s, http.MethodGet, path, "", who); w.Code != want {
			t.Errorf("GET %s: %d, want %d: %s", path, w.Code, want, w.Body)
		}
	}
	w := serve(t, s, http.MethodGet, "/api/accounts", "", who)
	if !strings.Contains(w.Body.String(), `"desk-a"`) || strings.Contains(w.Body.String(), `"desk-b"`) {
		t.Fatalf("account list for a desk-a reader: %s", w.Body)
	}
	if w := serve(t, s, http.MethodPost, "/api/accounts/desk-a/messages/import", twoExportedMessages, who); w.Code != http.StatusForbidden {
		t.Fatalf("import with only can_read_mail: %d, want 403", w.Code)
	}
}

func TestImportIncludesRead(t *testing.T) {
	s, fake, ids := accessTestServer(t, true)
	fake.set(func() { fake.grants["principal_000001"] = [][3]string{{"can_import_mail", "mail_account", "desk-a"}} })
	who := as("principal_000001")
	if w := serve(t, s, http.MethodPost, "/api/accounts/desk-a/messages/import", twoExportedMessages, who); w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	if w := serve(t, s, http.MethodGet, "/api/messages/"+ids["desk-a/1"]+"/metadata?account=desk-a", "", who); w.Code != http.StatusOK {
		t.Fatalf("read with can_import_mail: %d", w.Code)
	}
}

func TestATicketOpensItsOwnMessagesOnly(t *testing.T) {
	s, fake, ids := accessTestServer(t, true)
	linked := "desk-b:" + ids["desk-b/0"]
	fake.set(func() { fake.ticketVisible["principal_000002"] = map[string]bool{linked: true} })
	who := as("principal_000002")

	for path, want := range map[string]int{
		"/api/messages/" + ids["desk-b/0"] + "/metadata?account=desk-b":         200,
		"/api/messages/" + ids["desk-b/0"] + "/body?account=desk-b&format=text": 200,
		"/api/messages/" + ids["desk-b/0"] + "/revisions?account=desk-b":        200,
		"/api/messages/" + ids["desk-b/1"] + "/metadata?account=desk-b":         404,
		"/api/messages/" + ids["desk-b/0"] + "/thread?account=desk-b":           403,
		"/api/messages?account=desk-b":                                          403,
	} {
		if w := serve(t, s, http.MethodGet, path, "", who); w.Code != want {
			t.Errorf("GET %s: %d, want %d: %s", path, w.Code, want, w.Body)
		}
	}

	before := fake.kanbanCalls
	body := fmt.Sprintf(`{"refs":[{"locator":%q},{"locator":"desk-b:%s"},{"locator":"desk-a:%s"},{"locator":"desk-z:nope"},{"account_id":"desk-b","provider_message_id":"AAMk-1"}]}`,
		linked, ids["desk-b/1"], ids["desk-a/0"])
	w := serve(t, s, http.MethodPost, "/api/messages/resolve", body, who)
	var got struct {
		Results []struct {
			Status string          `json:"status"`
			Error  string          `json:"error"`
			Msg    json.RawMessage `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	want := []string{"found", "not_found", "not_found", "not_found", "found"}
	for i, status := range want {
		if got.Results[i].Status != status {
			t.Fatalf("result %d: %s, want %s (%s)", i, got.Results[i].Status, status, w.Body)
		}
		if status == "not_found" && (got.Results[i].Error != "" || len(got.Results[i].Msg) > 0) {
			t.Fatalf("result %d leaks %s", i, w.Body)
		}
	}
	if fake.kanbanCalls-before != 1 {
		t.Fatalf("resolve asked kanban-store %d times, want once for the whole batch", fake.kanbanCalls-before)
	}
}

func TestAccessIsReadLiveSoARevokedGrantStopsTheNextRead(t *testing.T) {
	s, fake, ids := accessTestServer(t, true)
	path := "/api/messages/" + ids["desk-a/0"] + "/body?account=desk-a&format=text"
	fake.set(func() { fake.grants["principal_000001"] = [][3]string{{"can_read_mail", "mail_account", "desk-a"}} })
	w := serve(t, s, http.MethodGet, path, "", as("principal_000001"))
	if w.Code != http.StatusOK {
		t.Fatalf("with the grant: %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	fake.set(func() { delete(fake.grants, "principal_000001") })
	// A conditional read is still a read: it must not answer 304 to a person
	// who has lost access.
	if w := serve(t, s, http.MethodGet, path, "", map[string]string{PrincipalIDHeader: "principal_000001", "If-None-Match": etag}); w.Code != http.StatusNotFound {
		t.Fatalf("after revoking: %d, want 404", w.Code)
	}
}

func TestRefusedPeopleAndUnreachableStores(t *testing.T) {
	s, fake, ids := accessTestServer(t, true)
	path := "/api/messages/" + ids["desk-a/0"] + "/metadata?account=desk-a"
	for principal, want := range map[string]int{
		"principal_000003": 200, // administrator
		"principal_000004": 403, // disabled
		"principal_000005": 403, // a contact, not a person
		"principal_999999": 403, // unknown
		"not-a-principal":  403,
	} {
		if w := serve(t, s, http.MethodGet, path, "", as(principal)); w.Code != want {
			t.Errorf("%s: %d, want %d: %s", principal, w.Code, want, w.Body)
		}
	}
	if w := serve(t, s, http.MethodGet, "/api/index/stats", "", as("principal_000003")); w.Code != http.StatusOK {
		t.Errorf("administrator on a services-only route: %d", w.Code)
	}

	fake.set(func() { fake.down["grant-store"] = true })
	if w := serve(t, s, http.MethodGet, path, "", as("principal_000006")); w.Code != http.StatusBadGateway {
		t.Fatalf("grant-store down: %d, want 502 — never read as holding nothing", w.Code)
	}
	fake.set(func() { fake.down["grant-store"], fake.down["kanban-store"] = false, true })
	if w := serve(t, s, http.MethodGet, path, "", as("principal_000006")); w.Code != http.StatusBadGateway {
		t.Fatalf("kanban-store down on a ticket check: %d, want 502", w.Code)
	}
	// A service is not touched by any of it.
	if w := serve(t, s, http.MethodGet, path, "", nil); w.Code != http.StatusOK {
		t.Fatalf("service read with the stores down: %d", w.Code)
	}
}

func TestAPersonIsRefusedWhenAccessCannotBeChecked(t *testing.T) {
	s, _, ids := accessTestServer(t, false)
	w := serve(t, s, http.MethodGet, "/api/messages/"+ids["desk-a/0"]+"/metadata?account=desk-a", "", as("principal_000003"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d, want 503", w.Code)
	}
}
