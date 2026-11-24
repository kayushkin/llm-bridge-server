package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests judge resolveFromAuthStore against auth-store's ACTUAL route
// rather than against its own doc comment. Derived from
// auth-store/internal/server/server.go and confirmed against a real auth-store
// binary on a throwaway database (2026-08-10):
//
//	GET /api/resolve/{provider}   keyAccess (bearer + X-Auth-App + X-Auth-Reason)
//
// keyAccess answers 400 with the plain-text body
// "X-Auth-App and X-Auth-Reason are required" when either header is missing —
// before the handler runs, so the reply mentions no provider at all. An
// unknown provider is 404 with a JSON {"error": ...} body; a bad bearer is 401.
//
// resolveFromAuthStore takes auth-store's URL and bearer when the resolver is
// constructed, so each test hands it the stub's.

type capture struct {
	method string
	path   string
	// rawURI is the request target exactly as it went over the wire. Go's
	// server decodes %2F back into a slash in URL.Path, so an assertion on
	// path alone cannot tell an escaped provider from an unescaped one.
	rawURI string
	query  string
	header http.Header
}

func newRecordingAuthStore(t *testing.T, status int, body string) (func(context.Context, string) (string, error), *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.rawURI = r.RequestURI
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return resolveFromAuthStore(srv.URL, "probe-token"), got
}

// resolveFixtureFromRealAuthStore is a byte-for-byte capture of what a real
// auth-store answered for GET /api/resolve/anthropic on an api_key credential.
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

func TestResolveUsesTheRouteAuthStoreServes(t *testing.T) {
	resolve, got := newRecordingAuthStore(t, 200, resolveFixtureFromRealAuthStore)

	secret, err := resolve(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.method != "GET" {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.path != "/api/resolve/anthropic" {
		t.Errorf("path = %q, want /api/resolve/anthropic", got.path)
	}
	if got.query != "" {
		t.Errorf("query = %q, want none — tool-store resolves without an account filter", got.query)
	}
	if secret != "sk-ant-SECRET-VALUE" {
		t.Errorf("secret = %q, want the api_key from the payload", secret)
	}
}

// TestResolveCarriesTheHeadersKeyAccessDemands is the one that matters most
// here: without these two headers every provision would fail with a 400 that
// names no provider, and the resolver's own error would report it as a
// provider problem.
func TestResolveCarriesTheHeadersKeyAccessDemands(t *testing.T) {
	resolve, got := newRecordingAuthStore(t, 200, resolveFixtureFromRealAuthStore)

	if _, err := resolve(context.Background(), "anthropic"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.header.Get("X-Auth-App") != "tool-store" {
		t.Errorf("X-Auth-App = %q, want tool-store; auth-store answers 400 without it",
			got.header.Get("X-Auth-App"))
	}
	if got.header.Get("X-Auth-Reason") != "provision:anthropic" {
		t.Errorf("X-Auth-Reason = %q, want provision:anthropic; auth-store answers 400 without it",
			got.header.Get("X-Auth-Reason"))
	}
	if got.header.Get("Authorization") != "Bearer probe-token" {
		t.Errorf("Authorization = %q", got.header.Get("Authorization"))
	}
}

// TestResolveEscapesTheProviderIntoThePath pins that a provider is a path
// segment. auth-store routes on {provider}, so a value containing a slash
// would otherwise address a different route entirely.
func TestResolveEscapesTheProviderIntoThePath(t *testing.T) {
	resolve, got := newRecordingAuthStore(t, 404, `{"error":"no credentials"}`)

	if _, err := resolve(context.Background(), "a/b"); err == nil {
		t.Fatal("expected the 404 to surface")
	}
	// Asserted on the raw request target: Go's server decodes %2F back into a
	// slash in URL.Path, so checking path alone would pass whether or not the
	// provider was escaped.
	if got.rawURI != "/api/resolve/a%2Fb" {
		t.Errorf("request target = %q, want /api/resolve/a%%2Fb; an unescaped "+
			"provider addresses a different route", got.rawURI)
	}
}

func TestResolvePicksTheSecretFieldThatMatchesTheAuthType(t *testing.T) {
	// auth-store's toResolved fills api_key for api_key credentials and
	// access_token for oauth and token ones, never both.
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"api_key", `{"auth_type":"api_key","api_key":"sk-key"}`, "sk-key"},
		{"oauth", `{"auth_type":"oauth","access_token":"tok-oauth"}`, "tok-oauth"},
		{"token", `{"auth_type":"token","access_token":"tok-plain"}`, "tok-plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve, _ := newRecordingAuthStore(t, 200, tc.body)
			got, err := resolve(context.Background(), "anthropic")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("secret = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveRefusesAnAuthTypeItCannotUse covers the shapes auth-store can
// legitimately return that this resolver has no secret string for. A password
// credential resolves with 200 and carries username/password/host instead.
func TestResolveRefusesAnAuthTypeItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"password credential", `{"auth_type":"password","username":"u","password":"p"}`, "unsupported auth_type"},
		{"api_key credential with an empty key", `{"auth_type":"api_key","api_key":""}`, "empty api_key"},
		{"oauth credential with an empty token", `{"auth_type":"oauth","access_token":""}`, "empty access_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve, _ := newRecordingAuthStore(t, 200, tc.body)
			_, err := resolve(context.Background(), "anthropic")
			if err == nil {
				t.Fatal("an unusable credential was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
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
		{"unknown provider", 404, `{"error":"no credentials for provider nosuch account=\"\" intended_app=\"\""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve, _ := newRecordingAuthStore(t, tc.status, tc.body)
			_, err := resolve(context.Background(), "nosuch")
			if err == nil {
				t.Fatalf("status %d was treated as success", tc.status)
			}
			// The provisioning caller sees only this string, so it has to
			// carry auth-store's own words rather than a generic failure.
			if !strings.Contains(err.Error(), tc.body[:10]) {
				t.Errorf("error = %q, want it to carry auth-store's response body", err)
			}
		})
	}
}

// TestResolveRefusesAnEmptyProvider pins the guard that stops the client
// building /api/resolve/ — a path auth-store does not route.
func TestResolveRefusesAnEmptyProvider(t *testing.T) {
	resolve, got := newRecordingAuthStore(t, 200, resolveFixtureFromRealAuthStore)

	if _, err := resolve(context.Background(), ""); err == nil {
		t.Fatal("an empty provider was accepted")
	}
	if got.path != "" {
		t.Errorf("a request was sent for an empty provider (path %q)", got.path)
	}
}
