package principalstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const (
	testTenant  = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	testSubject = "0f3c6a2e-5d4b-4c1a-9e8f-7a6b5c4d3e2f"
)

// TestAnIdentitySignsInAsTheOnePrincipalItWasLinkedTo pins the whole
// lifecycle: link, resolve, refuse a second link, unlink with a reason, stop
// resolving, link again as a new row.
func TestAnIdentitySignsInAsTheOnePrincipalItWasLinkedTo(t *testing.T) {
	s := newTestStore(t)
	admin := mustCreate(t, s, KindHuman, "Admin", "")
	priya := mustCreate(t, s, KindHuman, "Priya", "priya@example.com")
	marcus := mustCreate(t, s, KindHuman, "Marcus", "")

	if _, err := s.ResolveIdentity("entra", testTenant, testSubject); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an identity nobody linked must be unprovisioned, got %v", err)
	}

	// Capitals in, lowercase stored: the login writes the claim as Entra sends it.
	b, err := s.LinkIdentity(priya.ID, "Entra", "72F988BF-86F1-41AF-91AB-2D7CD011DB47", testSubject, admin.ID, "onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "identity_binding_000001" || b.TenantID != testTenant || b.LinkedBy != admin.ID {
		t.Fatalf("unexpected binding %+v", b)
	}

	resolved, err := s.ResolveIdentity("entra", testTenant, testSubject)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Principal.ID != priya.ID || resolved.Binding.ID != b.ID {
		t.Fatalf("resolved to %+v", resolved)
	}

	if _, err := s.LinkIdentity(marcus.ID, "entra", testTenant, testSubject, admin.ID, ""); !errors.Is(err, ErrIdentityAlreadyBound) {
		t.Fatalf("a bound identity must not be linked to a second principal, got %v", err)
	}
	if _, err := s.LinkIdentity(priya.ID, "entra", testTenant, testSubject, admin.ID, ""); !errors.Is(err, ErrIdentityAlreadyBound) {
		t.Fatalf("linking twice to the same principal is a mistake to show, got %v", err)
	}

	if _, err := s.UnlinkIdentity(b.ID, admin.ID, ""); !errors.Is(err, ErrInvalidIdentityBinding) {
		t.Fatalf("an unlink without a reason must be refused, got %v", err)
	}
	unlinked, err := s.UnlinkIdentity(b.ID, admin.ID, "left the support team")
	if err != nil {
		t.Fatal(err)
	}
	if unlinked.UnlinkedAt == 0 || unlinked.UnlinkedBy != admin.ID {
		t.Fatalf("unlink must stamp who and when: %+v", unlinked)
	}
	again, err := s.UnlinkIdentity(b.ID, marcus.ID, "second click")
	if err != nil {
		t.Fatal(err)
	}
	if again.UnlinkedAt != unlinked.UnlinkedAt || again.UnlinkReason != "left the support team" || again.UnlinkedBy != admin.ID {
		t.Fatalf("a second unlink must not move the stamp or the reason: %+v", again)
	}
	if _, err := s.ResolveIdentity("entra", testTenant, testSubject); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unlinked identity must stop resolving, got %v", err)
	}

	relinked, err := s.LinkIdentity(marcus.ID, "entra", testTenant, testSubject, admin.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if relinked.ID == b.ID {
		t.Fatal("linking again after an unlink must write a new row, keeping the old one as history")
	}
	all, err := s.ListIdentityBindings(IdentityBindingFilter{IncludeUnlinked: true})
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.ListIdentityBindings(IdentityBindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(active) != 1 || active[0].PrincipalID != marcus.ID {
		t.Fatalf("history %d rows, active %+v", len(all), active)
	}
}

// TestOnlyAnActiveHumanCanBeGivenASignIn: a group, a contact and a disabled
// human never sign in, and the audit fields must name an active human.
func TestOnlyAnActiveHumanCanBeGivenASignIn(t *testing.T) {
	s := newTestStore(t)
	admin := mustCreate(t, s, KindHuman, "Admin", "")
	group := mustCreate(t, s, KindGroup, "Support", "")
	contact := mustCreate(t, s, KindContact, "Requester", "r@example.com")
	gone := mustCreate(t, s, KindHuman, "Gone", "")
	if _, err := s.Disable(gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{group.ID, contact.ID, gone.ID} {
		if _, err := s.LinkIdentity(id, "entra", testTenant, testSubject, admin.ID, ""); !errors.Is(err, ErrInvalidIdentityBinding) {
			t.Fatalf("%s must be refused a sign-in, got %v", id, err)
		}
	}
	for _, actor := range []string{"", group.ID, gone.ID, "principal_999999"} {
		if _, err := s.LinkIdentity(admin.ID, "entra", testTenant, testSubject, actor, ""); !errors.Is(err, ErrInvalidIdentityBinding) {
			t.Fatalf("linked_by %q must be refused, got %v", actor, err)
		}
	}
}

// TestAnEmailIsNotAnIdentity: the mistake the GUID check exists for.
func TestAnEmailIsNotAnIdentity(t *testing.T) {
	s := newTestStore(t)
	admin := mustCreate(t, s, KindHuman, "Admin", "")
	cases := []struct{ provider, tenant, subject string }{
		{"entra", testTenant, "priya@example.com"},
		{"entra", "contoso.onmicrosoft.com", testSubject},
		{"google", testTenant, testSubject},
	}
	for _, c := range cases {
		if _, err := s.LinkIdentity(admin.ID, c.provider, c.tenant, c.subject, admin.ID, ""); !errors.Is(err, ErrInvalidIdentityBinding) {
			t.Fatalf("%+v must be refused, got %v", c, err)
		}
	}
}

// TestConcurrentLinksOfOneIdentityLeaveOneBinding: the unique index, not the
// Go check, is what holds when two administrators click at once.
func TestConcurrentLinksOfOneIdentityLeaveOneBinding(t *testing.T) {
	s := newTestStore(t)
	admin := mustCreate(t, s, KindHuman, "Admin", "")
	var targets []*Principal
	for i := 0; i < 8; i++ {
		targets = append(targets, mustCreate(t, s, KindHuman, "Person", ""))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	for i, p := range targets {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, errs[i] = s.LinkIdentity(id, "entra", testTenant, testSubject, admin.ID, "")
		}(i, p.ID)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	active, err := s.ListIdentityBindings(IdentityBindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || len(active) != 1 {
		t.Fatalf("%d links succeeded and %d are active; want 1 and 1 (errors %v)", succeeded, len(active), errs)
	}
}

// TestIdentityBindingRoutes checks the status codes a caller branches on.
func TestIdentityBindingRoutes(t *testing.T) {
	s := newTestStore(t)
	admin := mustCreate(t, s, KindHuman, "Admin", "")
	priya := mustCreate(t, s, KindHuman, "Priya", "")
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, &buf))
		return rec
	}
	link := map[string]string{"principal_id": priya.ID, "provider": "entra", "tenant_id": testTenant, "subject": testSubject, "linked_by": admin.ID}

	resolvePath := "/identity-bindings/resolve?provider=entra&tenant_id=" + testTenant + "&subject=" + testSubject
	if rec := call("GET", resolvePath, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unprovisioned resolve: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/identity-bindings", map[string]string{"principal_id": priya.ID, "provder": "entra"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled key must be a 400, got %d", rec.Code)
	}
	rec := call("POST", "/identity-bindings", link)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	var created IdentityBinding
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if rec := call("POST", "/identity-bindings", link); rec.Code != http.StatusConflict {
		t.Fatalf("second link: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", resolvePath, nil); rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/identity-bindings?principal_id="+priya.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if rec := call("GET", "/identity-bindings/identity_binding_999999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing binding: %d", rec.Code)
	}
	if rec := call("POST", "/identity-bindings/"+created.ID+"/unlink", map[string]string{"unlinked_by": admin.ID, "reason": "test"}); rec.Code != http.StatusOK {
		t.Fatalf("unlink: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/identity-providers", nil); rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"entra"`)) {
		t.Fatalf("providers: %d %s", rec.Code, rec.Body)
	}
}

func TestADiscordIdentityBindsByUserIDWithNoTenant(t *testing.T) {
	s := newTestStore(t)
	person := mustCreate(t, s, KindHuman, "Slava Kayushkin", "")
	binding, err := s.LinkIdentity(person.ID, IdentityProviderDiscord, "", "104357516833472512", person.ID, "linked while signed in")
	if err != nil {
		t.Fatalf("link a Discord id: %v", err)
	}
	resolution, err := s.ResolveIdentity(IdentityProviderDiscord, "", "104357516833472512")
	if err != nil || resolution.Principal.ID != person.ID || resolution.Binding.ID != binding.ID {
		t.Fatalf("resolve: %+v %v", resolution, err)
	}
	for _, bad := range []struct{ tenant, subject string }{{"", "nrmh"}, {"guild", "104357516833472512"}, {"", "123"}} {
		if _, err := s.LinkIdentity(person.ID, IdentityProviderDiscord, bad.tenant, bad.subject, person.ID, ""); !errors.Is(err, ErrInvalidIdentityBinding) {
			t.Errorf("tenant %q subject %q: got %v, want a refusal", bad.tenant, bad.subject, err)
		}
	}
}
