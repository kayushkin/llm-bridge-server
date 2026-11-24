package authstore

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests judge this client against auth-store's ACTUAL routes rather
// than against its own doc comments. Every expectation below is derived from
// auth-store/internal/server/server.go and confirmed against a real auth-store
// binary on a throwaway database (2026-08-10):
//
//	GET /api/credentials              protected  (bearer only) -> bare JSON array
//	GET /api/credentials/{id}/resolve keyAccess  (bearer + X-Auth-App + X-Auth-Reason)
//
// keyAccess answers 400 with the plain-text body
// "X-Auth-App and X-Auth-Reason are required" when either header is missing.
// An unknown id is 404 with a JSON {"error": ...} body; a bad bearer is 401.

type capture struct {
	method string
	path   string
	header http.Header
}

func newRecordingAuthStore(t *testing.T, status int, body string) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "probe-token", "usage-store"), got
}

// listFixtureFromRealAuthStore is a capture of what a real auth-store answered
// for GET /api/credentials. Note the bare array and the MASKED api_key — the
// list route never returns usable secret material.
const listFixtureFromRealAuthStore = `[
  {
    "id": "cred_90b0ca4b2f7b15bada54605c",
    "provider": "anthropic",
    "owner": "probe",
    "account": "default",
    "intended_app": "tool-store",
    "auth_type": "api_key",
    "refresh_mode": "server",
    "api_key": "sk-a...ALUE",
    "has_refresh_token": false,
    "leased": false,
    "priority": 100,
    "enabled": true,
    "created_at": "2026-08-10 12:33:26"
  }
]`

// resolveFixtureFromRealAuthStore is the same credential through the resolve
// route, where the api_key is unmasked.
const resolveFixtureFromRealAuthStore = `{
  "id": "cred_90b0ca4b2f7b15bada54605c",
  "provider": "anthropic",
  "owner": "probe",
  "account": "default",
  "auth_type": "api_key",
  "refresh_mode": "server",
  "api_key": "sk-ant-SECRET-VALUE",
  "leased": false,
  "intended_app": "tool-store"
}`

func TestListCredentialsUsesTheRouteAuthStoreServes(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, listFixtureFromRealAuthStore)

	creds, err := c.ListCredentials()
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if got.method != "GET" {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.path != "/api/credentials" {
		t.Errorf("path = %q, want /api/credentials", got.path)
	}
	if len(creds) != 1 {
		t.Fatalf("got %d credentials, want 1", len(creds))
	}
	if creds[0].ID != "cred_90b0ca4b2f7b15bada54605c" {
		t.Errorf("ID = %q", creds[0].ID)
	}
	if creds[0].AuthType != "api_key" {
		t.Errorf("AuthType = %q — the field name on the wire is auth_type", creds[0].AuthType)
	}
}

// TestListCredentialsRejectsAShapeAuthStoreDoesNotSend pins the removal of a
// fallback branch that could never fire. The client used to try a bare array
// and then fall back to {"credentials": [...]}; auth-store has only ever sent
// the array, so the fallback was unreachable code that would have turned a
// changed route into a silent empty list.
func TestListCredentialsRejectsAShapeAuthStoreDoesNotSend(t *testing.T) {
	c, _ := newRecordingAuthStore(t, 200, `{"credentials":[{"id":"cred_a"}]}`)

	_, err := c.ListCredentials()
	if err == nil {
		t.Fatal("an enveloped list was accepted; auth-store returns a bare array, " +
			"so accepting an envelope hides a route that changed shape")
	}
	if !strings.Contains(err.Error(), "parse list") {
		t.Errorf("error = %q, want it to name the parse failure", err)
	}
}

func TestResolveByIDUsesTheRouteAuthStoreServes(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, resolveFixtureFromRealAuthStore)

	r, err := c.ResolveByID("cred_90b0ca4b2f7b15bada54605c", "discover:admin-key")
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	if got.path != "/api/credentials/cred_90b0ca4b2f7b15bada54605c/resolve" {
		t.Errorf("path = %q, want /api/credentials/{id}/resolve", got.path)
	}
	if r.APIKey != "sk-ant-SECRET-VALUE" {
		t.Errorf("APIKey = %q — the field name on the wire is api_key", r.APIKey)
	}
}

// TestResolveCarriesTheHeadersKeyAccessDemands is the sharpest thing this file
// pins. /api/credentials/{id}/resolve sits behind keyAccess, which rejects the
// request with a 400 before the handler runs if either audit header is absent.
// A resolve that quietly stopped sending them would fail with a message about
// headers and no mention of the credential.
func TestResolveCarriesTheHeadersKeyAccessDemands(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, resolveFixtureFromRealAuthStore)

	if _, err := c.ResolveByID("cred_abc", "fetch:anthropic-spend-per-key"); err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	if got.header.Get("X-Auth-App") != "usage-store" {
		t.Errorf("X-Auth-App = %q, want usage-store; auth-store answers 400 without it",
			got.header.Get("X-Auth-App"))
	}
	if got.header.Get("X-Auth-Reason") != "fetch:anthropic-spend-per-key" {
		t.Errorf("X-Auth-Reason = %q, want the caller's reason; auth-store answers 400 without it",
			got.header.Get("X-Auth-Reason"))
	}
	if got.header.Get("Authorization") != "Bearer probe-token" {
		t.Errorf("Authorization = %q", got.header.Get("Authorization"))
	}
}

// TestListDoesNotNeedTheAuditHeaders records the asymmetry deliberately: the
// list route is `protected`, not `keyAccess`, so it works with the bearer
// alone. If auth-store ever moves it behind keyAccess this client starts
// getting 400s, and this test is where the reason is written down.
func TestListDoesNotNeedTheAuditHeaders(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, `[]`)

	if _, err := c.ListCredentials(); err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if got.header.Get("Authorization") != "Bearer probe-token" {
		t.Errorf("Authorization = %q", got.header.Get("Authorization"))
	}
}

func TestTheStatusesAuthStoreUsesForFailureAllBecomeErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing audit headers", 400, "X-Auth-App and X-Auth-Reason are required"},
		{"bad bearer", 401, "unauthorized"},
		{"unknown id", 404, `{"error":"credential \"cred_nope\" not found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newRecordingAuthStore(t, tc.status, tc.body)
			_, err := c.ResolveByID("cred_nope", "probe")
			if err == nil {
				t.Fatalf("status %d was treated as success", tc.status)
			}
			// Asserting the status reaches the message is what gives this
			// test teeth. auth-store's 404 body is VALID JSON that decodes
			// to a credential with an empty api_key, so a client that
			// stopped checking the status would still fail — with "has no
			// api_key" instead of "404", blaming the credential for a
			// lookup that never found one. Confirmed by mutation: without
			// this assertion, deleting the status check goes unnoticed.
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("error = %q, want it to carry the status %d", err, tc.status)
			}
		})
	}
}

// TestResolveRefusesACredentialWithNoAPIKey pins the one place this client
// reads past the status code. auth-store's toResolved fills api_key only for
// api_key credentials — an oauth credential resolves fine and carries
// access_token instead, which usage-store cannot use for a provider cost API.
func TestResolveRefusesACredentialWithNoAPIKey(t *testing.T) {
	c, _ := newRecordingAuthStore(t, 200,
		`{"id":"cred_o","provider":"anthropic","auth_type":"oauth","access_token":"tok","leased":false}`)

	_, err := c.ResolveByID("cred_o", "probe")
	if err == nil {
		t.Fatal("an oauth credential was accepted as an api_key one")
	}
	if !strings.Contains(err.Error(), "auth_type=oauth") {
		t.Errorf("error = %q, want it to name the auth_type it got", err)
	}
}
