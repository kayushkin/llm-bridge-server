package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

func TestIdentityBindingRoutesAreForAdministratorsOnly(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	cookie := gated.loginAs(t, firstTestPrincipalID)
	for _, route := range []struct{ method, path string }{
		{"GET", "/admin/identity-bindings"},
		{"POST", "/admin/identity-bindings"},
		{"POST", "/admin/identity-bindings/identity_binding_000001/unlink"},
	} {
		response := gated.requestAs(t, cookie, route.method, route.path, map[string]string{})
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator_route") {
			t.Errorf("%s %s as a non-administrator = %d %s, want 403 operator_route", route.method, route.path, response.Code, response.Body.String())
		}
	}
	if got := gated.principals.recordedIdentityBindingRequests(); len(got) != 0 {
		t.Fatalf("principal-store received %d binding requests from a non-administrator", len(got))
	}
}

func TestLinkingSetsLinkedByFromTheCallerAndPassesTheAnswerThrough(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	gated.principals.setIdentityBindings(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Principal-Store-Note", "kept")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"identity entra/t/o is already bound to principal_000002 by identity_binding_000004"}`))
	})

	forged := gated.requestAs(t, administratorCookie, "POST", "/admin/identity-bindings", map[string]string{
		"principal_id": firstTestPrincipalID, "provider": "entra", "tenant_id": testEntraTenantID, "subject": testEntraObjectIDOfFirst,
		"linked_by": "principal_000001",
	})
	if forged.Code != http.StatusBadRequest || !strings.Contains(forged.Body.String(), "linked_by_not_accepted") {
		t.Fatalf("a body naming linked_by = %d %s, want 400 linked_by_not_accepted", forged.Code, forged.Body.String())
	}
	if got := gated.principals.recordedIdentityBindingRequests(); len(got) != 0 {
		t.Fatalf("a refused body reached principal-store")
	}

	response := gated.requestAs(t, administratorCookie, "POST", "/admin/identity-bindings", map[string]string{
		"principal_id": firstTestPrincipalID, "provider": "entra", "tenant_id": testEntraTenantID, "subject": testEntraObjectIDOfFirst, "note": "from the attempts list",
	})
	if response.Code != http.StatusConflict ||
		response.Body.String() != `{"error":"identity entra/t/o is already bound to principal_000002 by identity_binding_000004"}` ||
		response.Header().Get("X-Principal-Store-Note") != "kept" {
		t.Fatalf("principal-store's 409 came back as %d %v %s, want it unchanged", response.Code, response.Header(), response.Body.String())
	}
	sent := gated.principals.recordedIdentityBindingRequests()
	if len(sent) != 1 || sent[0].Method != "POST" || sent[0].EscapedPath != "/identity-bindings" {
		t.Fatalf("principal-store received %+v", sent)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(sent[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["linked_by"] != administratorTestPrincipalID || body["principal_id"] != firstTestPrincipalID || body["subject"] != testEntraObjectIDOfFirst ||
		body["note"] != "from the attempts list" {
		t.Fatalf("principal-store was sent %v, want linked_by %s and the body's fields", body, administratorTestPrincipalID)
	}

	// The service token acting as nobody cannot stamp a binding.
	request := httptest.NewRequest("POST", "/admin/identity-bindings", strings.NewReader(`{"principal_id":"principal_000001"}`))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	if refused := serve(gated.server, request); refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "identity_binding_change_requires_a_principal") {
		t.Fatalf("service token linking as nobody = %d %s", refused.Code, refused.Body.String())
	}
}

func TestListingBindingsPassesTheQueryThrough(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	gated.principals.setIdentityBindings(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"identity_binding_000001"}]`))
	})
	response := gated.requestAsService(t, "GET", "/admin/identity-bindings?principal_id=principal_000001&include_unlinked=true")
	if response.Code != http.StatusOK || response.Body.String() != `[{"id":"identity_binding_000001"}]` {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	sent := gated.principals.recordedIdentityBindingRequests()
	if len(sent) != 1 || sent[0].EscapedPath != "/identity-bindings" || sent[0].RawQuery != "principal_id=principal_000001&include_unlinked=true" {
		t.Fatalf("principal-store received %+v", sent)
	}
}

func TestUnlinkingRevokesTheLoginSessionsOfThatBindingOnly(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	administratorCookie := gated.loginAs(t, administratorTestPrincipalID)
	unlinkStatus := http.StatusOK
	gated.principals.setIdentityBindings(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(unlinkStatus)
		if unlinkStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"id":"identity_binding_000001","unlinked_by":"principal_000003","unlink_reason":"left"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"not found: identity binding identity_binding_000001"}`))
		}
	})
	now := time.Now().UTC()
	insert := func(tokenHash, bindingID string) store.LoginSession {
		session := store.LoginSession{TokenHash: tokenHash, CSRFToken: "c", PrincipalID: firstTestPrincipalID, Method: store.LoginSessionMethodEntra,
			IdentityBindingID: bindingID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
		if bindingID == "" {
			session.Method = store.LoginSessionMethodDemo
		}
		if err := gated.store.InsertLoginSession(&session); err != nil {
			t.Fatal(err)
		}
		return session
	}
	bound := insert(sha256Hex("bound-token"), "identity_binding_000001")
	otherBinding := insert(sha256Hex("other-token"), "identity_binding_000002")
	demo := insert(sha256Hex("demo-token"), "")

	forged := gated.requestAs(t, administratorCookie, "POST", "/admin/identity-bindings/identity_binding_000001/unlink", map[string]string{"reason": "left", "unlinked_by": "principal_000001"})
	if forged.Code != http.StatusBadRequest || !strings.Contains(forged.Body.String(), "unlinked_by_not_accepted") {
		t.Fatalf("a body naming unlinked_by = %d %s", forged.Code, forged.Body.String())
	}

	// A failed unlink revokes nothing, and its answer comes back unchanged.
	unlinkStatus = http.StatusNotFound
	missing := gated.requestAs(t, administratorCookie, "POST", "/admin/identity-bindings/identity_binding_000001/unlink", map[string]string{"reason": "left"})
	if missing.Code != http.StatusNotFound || missing.Body.String() != `{"error":"not found: identity binding identity_binding_000001"}` {
		t.Fatalf("principal-store's 404 came back as %d %s", missing.Code, missing.Body.String())
	}
	if row, _ := gated.store.GetLoginSession(bound.ID); row.RevokedAt != nil {
		t.Fatalf("a failed unlink revoked a session")
	}

	unlinkStatus = http.StatusOK
	response := gated.requestAs(t, administratorCookie, "POST", "/admin/identity-bindings/identity_binding_000001/unlink", map[string]string{"reason": "left"})
	if response.Code != http.StatusOK || response.Body.String() != `{"id":"identity_binding_000001","unlinked_by":"principal_000003","unlink_reason":"left"}` {
		t.Fatalf("unlink = %d %s, want principal-store's answer unchanged", response.Code, response.Body.String())
	}
	sent := gated.principals.recordedIdentityBindingRequests()
	last := sent[len(sent)-1]
	if last.EscapedPath != "/identity-bindings/identity_binding_000001/unlink" || !strings.Contains(last.Body, `"unlinked_by":"principal_000003"`) ||
		!strings.Contains(last.Body, `"reason":"left"`) {
		t.Fatalf("principal-store was sent %s %s", last.EscapedPath, last.Body)
	}

	for _, check := range []struct {
		session     store.LoginSession
		wantRevoked bool
	}{{bound, true}, {otherBinding, false}, {demo, false}} {
		row, err := gated.store.GetLoginSession(check.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (row.RevokedAt != nil) != check.wantRevoked {
			t.Errorf("%s (binding %q): revoked %v, want %v", row.ID, row.IdentityBindingID, row.RevokedAt != nil, check.wantRevoked)
		}
		if check.wantRevoked && (row.RevokedBy != administratorTestPrincipalID || row.RevokeReason != "identity unlinked: left") {
			t.Errorf("%s revoked by %q for %q", row.ID, row.RevokedBy, row.RevokeReason)
		}
	}
	whoBound := httptest.NewRequest("GET", "/auth/principal", nil)
	whoBound.AddCookie(&http.Cookie{Name: loginSessionCookieName, Value: "bound-token"})
	if answer := serve(gated.server, whoBound); answer.Code != http.StatusUnauthorized || !strings.Contains(answer.Body.String(), "login_session_revoked") {
		t.Fatalf("the unlinked identity's cookie = %d %s, want 401 login_session_revoked", answer.Code, answer.Body.String())
	}
}
