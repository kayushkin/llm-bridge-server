package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
)

// fakePeopleDirectory answers as principal-store, grant-store and
// kanban-store at once, from maps a test edits between calls.
type fakePeopleDirectory struct {
	mu            sync.Mutex
	principals    map[string]PrincipalFacts
	grants        map[string][][3]string // principal → [relation, resource_type, resource_id]
	ticketVisible map[string]map[string]bool
	down          map[string]bool // "grant-store" / "kanban-store" cannot answer
	kanbanCalls   int
}

func newFakePeopleDirectory() *fakePeopleDirectory {
	return &fakePeopleDirectory{
		principals: map[string]PrincipalFacts{
			"principal_000001": {Kind: "human"},
			"principal_000002": {Kind: "human"},
			"principal_000003": {Kind: "human", IsAdministrator: true},
			"principal_000004": {Kind: "human", Disabled: true},
			"principal_000005": {Kind: "contact"},
			"principal_000006": {Kind: "human"},
		},
		grants:        map[string][][3]string{},
		ticketVisible: map[string]map[string]bool{},
		down:          map[string]bool{},
	}
}

func (f *fakePeopleDirectory) Principal(ctx context.Context, principalID string) (PrincipalFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	facts, known := f.principals[principalID]
	if !known {
		return PrincipalFacts{}, ErrPrincipalUnknown
	}
	return facts, nil
}

func (f *fakePeopleDirectory) MailGrants(ctx context.Context, principalID string) ([]MailGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down["grant-store"] {
		return nil, errors.New("grant-store is down")
	}
	if _, known := f.principals[principalID]; !known {
		return nil, ErrPrincipalUnknown
	}
	var out []MailGrant
	for _, g := range f.grants[principalID] {
		if g[1] == resourceTypeMail {
			out = append(out, MailGrant{Relation: g[0], AccountID: g[2]})
		}
	}
	return out, nil
}

func (f *fakePeopleDirectory) MessagesVisibleThroughTickets(ctx context.Context, principalID string, locators []string) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kanbanCalls++
	if f.down["kanban-store"] {
		return nil, errors.New("kanban-store is down")
	}
	out := make([]bool, len(locators))
	for i, locator := range locators {
		out[i] = f.ticketVisible[principalID][locator]
	}
	return out, nil
}

func (f *fakePeopleDirectory) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// accessTestServer has two local accounts, each with the two exported
// messages, and the fake stores behind it.
func accessTestServer(t *testing.T, withStores bool) (*Server, *fakePeopleDirectory, map[string]string) {
	t.Helper()
	store, local := newTestStores(t)
	for _, id := range []string{"desk-a", "desk-b"} {
		if err := store.Save(&accounts.Account{ID: id, Label: id, Provider: accounts.ProviderLocal, Address: id + "@demo.invalid", Enabled: true}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	fake := newFakePeopleDirectory()
	parts := ServerParts{}
	if withStores {
		parts.People = fake
	}
	s := newTestServer(t, store, local, parts)
	ids := map[string]string{}
	for _, account := range []string{"desk-a", "desk-b"} {
		got := importInto(t, s, account, twoExportedMessages)
		ids[account+"/0"], ids[account+"/1"] = got.Outcomes[0].ID, got.Outcomes[1].ID
	}
	return s, fake, ids
}

func as(principal string) map[string]string { return map[string]string{testPrincipalHeader: principal} }

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
		"/api/ingress/receipts":                                                 403,
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
	if w := serve(t, s, http.MethodGet, path, "", map[string]string{testPrincipalHeader: "principal_000001", "If-None-Match": etag}); w.Code != http.StatusNotFound {
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
