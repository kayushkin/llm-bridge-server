package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fixture below is derived from auth-store, not from this client.
//
// Every behavior it encodes was read out of ~/repos/auth-store and then
// confirmed against the service running on 127.0.0.1:8303 on 2026-08-29:
//
//   - the route is GET /api/resolve/{provider}
//     (auth-store internal/server/server.go, s.keyAccess)
//   - keyAccess is bearer THEN requireAppReason, so a request with neither the
//     token nor the headers is 401, never 400
//     (internal/server/middleware.go:27-64; measured: 401 with no headers)
//   - the bearer and header refusals are http.Error, so their bodies are PLAIN
//     TEXT; the resolve refusals are writeError, so theirs are JSON {"error":…}
//     (measured: "unauthorized", "X-Auth-App and X-Auth-Reason are required",
//     and {"error":"no credentials for provider …"} with 404)
//   - the success body is internal/server/resolve.go's resolvedView, whose
//     access_token and api_key are omitempty and whose leased is not
//   - auth-store has FOUR auth types, not three: api_key, oauth, token and
//     password (store.go:35-38)
//
// A fixture written from this client instead could not disagree with it, which
// is the whole point of the exercise (noteboard card b9b8dafc).

type authStoreStub struct {
	token string // when non-empty the stub enforces a bearer, as auth-store does

	// resolve answers for one provider; anything else is auth-store's 404.
	provider string
	body     map[string]any

	lastRequestURI string
	lastApp        string
	lastReason     string
	lastAuth       string
	calls          int
}

func (a *authStoreStub) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.calls++
		// RequestURI, not URL.Path: Go's server decodes %2F back into a real
		// separator in URL.Path, so a test written against URL.Path passes
		// whether or not the client escaped anything.
		a.lastRequestURI = r.RequestURI
		a.lastApp = r.Header.Get("X-Auth-App")
		a.lastReason = r.Header.Get("X-Auth-Reason")
		a.lastAuth = r.Header.Get("Authorization")

		if a.token != "" && r.Header.Get("Authorization") != "Bearer "+a.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Auth-App") == "" || r.Header.Get("X-Auth-Reason") == "" {
			http.Error(w, "X-Auth-App and X-Auth-Reason are required", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/resolve/"+a.provider {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{
				"error": `no credentials for provider ` + strings.TrimPrefix(r.URL.Path, "/api/resolve/"),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(a.body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// resolved is auth-store's resolvedView for a healthy api_key credential.
func resolvedAPIKey(key string) map[string]any {
	return map[string]any{
		"id": "cred-1", "provider": "brave", "auth_type": "api_key",
		"refresh_mode": "server", "api_key": key, "leased": false,
	}
}

func newResolver(t *testing.T, base, token string) func(context.Context, string) (string, error) {
	t.Helper()
	return resolveFromAuthStore(base, token)
}

func TestResolveSendsTheAppAndReasonHeadersAuthStoreRequires(t *testing.T) {
	stub := &authStoreStub{provider: "brave", body: resolvedAPIKey("bk-live")}
	got, err := newResolver(t, stub.start(t), "")(context.Background(), "brave")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "bk-live" {
		t.Errorf("value = %q, want %q", got, "bk-live")
	}
	if stub.lastRequestURI != "/api/resolve/brave" {
		t.Errorf("RequestURI = %q, want /api/resolve/brave", stub.lastRequestURI)
	}
	// auth-store refuses the request outright without these two, so they are
	// part of the protocol and not decoration.
	if stub.lastApp != "tool-store" {
		t.Errorf("X-Auth-App = %q, want tool-store", stub.lastApp)
	}
	if stub.lastReason != "provision:brave" {
		t.Errorf("X-Auth-Reason = %q, want provision:brave", stub.lastReason)
	}
}

func TestResolveSendsTheBearerAuthStoreEnforces(t *testing.T) {
	stub := &authStoreStub{token: "s3cret", provider: "brave", body: resolvedAPIKey("bk-live")}
	if _, err := newResolver(t, stub.start(t), "s3cret")(context.Background(), "brave"); err != nil {
		t.Fatalf("resolve with matching token: %v", err)
	}
	if stub.lastAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", stub.lastAuth)
	}
}

// auth-store's bearer middleware runs BEFORE requireAppReason, so a client with
// no AUTH_STORE_TOKEN against an auth-store that has one gets 401 — and the
// status has to reach the message, or a refused read reads as a malformed one.
func TestAMissingBearerIs401AndTheStatusReachesTheMessage(t *testing.T) {
	stub := &authStoreStub{token: "s3cret", provider: "brave", body: resolvedAPIKey("bk-live")}
	_, err := newResolver(t, stub.start(t), "")(context.Background(), "brave")
	if err == nil {
		t.Fatal("want an error when auth-store enforces a bearer this client does not send")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not name the 401 status", err)
	}
}

func TestAnUnknownProviderIs404AndTheStatusReachesTheMessage(t *testing.T) {
	stub := &authStoreStub{provider: "brave", body: resolvedAPIKey("bk-live")}
	_, err := newResolver(t, stub.start(t), "")(context.Background(), "no-such-provider")
	if err == nil {
		t.Fatal("want an error for a provider auth-store does not hold")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not name the 404 status", err)
	}
}

// auth-store's toResolved fills a DIFFERENT field per auth type, and both are
// omitempty — so reading the wrong one yields "" and not a decode failure.
func TestEachAuthTypeIsReadOutOfTheFieldAuthStoreFillsForIt(t *testing.T) {
	cases := []struct {
		authType string
		body     map[string]any
		want     string
	}{
		{"api_key", map[string]any{"auth_type": "api_key", "api_key": "k", "leased": false}, "k"},
		{"oauth", map[string]any{"auth_type": "oauth", "access_token": "t", "leased": false}, "t"},
		{"token", map[string]any{"auth_type": "token", "access_token": "t", "leased": false}, "t"},
	}
	for _, c := range cases {
		t.Run(c.authType, func(t *testing.T) {
			stub := &authStoreStub{provider: "p", body: c.body}
			got, err := newResolver(t, stub.start(t), "")(context.Background(), "p")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != c.want {
				t.Errorf("value = %q, want %q", got, c.want)
			}
		})
	}
}

// auth-store has a fourth auth type. toResolved fills username/password/host/
// port/tls for it and leaves both of the fields this client reads empty, so
// without the explicit refusal the caller would be handed "".
func TestAPasswordCredentialIsRefusedRatherThanResolvedToTheEmptyString(t *testing.T) {
	stub := &authStoreStub{provider: "mail", body: map[string]any{
		"auth_type": "password", "username": "slava", "password": "pw",
		"host": "mail.example.com", "port": 993, "tls": true, "leased": false,
	}}
	got, err := newResolver(t, stub.start(t), "")(context.Background(), "mail")
	if err == nil {
		t.Fatalf("want a refusal, got value %q", got)
	}
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("error %q does not name the auth_type it refused", err)
	}
}

func TestAnEmptyProviderNeverReachesAuthStore(t *testing.T) {
	stub := &authStoreStub{provider: "brave", body: resolvedAPIKey("bk-live")}
	if _, err := newResolver(t, stub.start(t), "")(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty provider")
	}
	if stub.calls != 0 {
		t.Errorf("client made %d request(s) for an empty provider; auth-store would answer 404", stub.calls)
	}
}

// ⚠️ CHARACTERISATION. auth-store publishes two staleness signals on the same
// body and this client reads neither. resolvedView's own doc comment:
//
//	"For credentials in client_lease mode that are currently leased, returns
//	 the last known access_token with leased=true so callers know it may be
//	 stale; the lease holder is the authoritative source."
//
// and maybeRefresh (credentials.go:191) returns without refreshing whenever the
// credential is leased or its refresh_mode is not "server", so an expired
// access_token is returned with a 200.
//
// These two tests assert what the client does TODAY, so they pass against
// unmodified code. When the repair lands — whatever it is, see the noteboard
// card — delete them; they exist so the gap cannot close or widen unnoticed.
func TestALeasedCredentialIsHandedBackWithNoSignalThatItMayBeStale(t *testing.T) {
	stub := &authStoreStub{provider: "anthropic", body: map[string]any{
		"auth_type": "oauth", "refresh_mode": "client_lease",
		"access_token": "last-known-and-possibly-stale", "leased": true,
	}}
	got, err := newResolver(t, stub.start(t), "")(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("today the client ignores leased entirely: %v", err)
	}
	if got != "last-known-and-possibly-stale" {
		t.Fatalf("value = %q", got)
	}
	t.Log("characterisation: leased=true is dropped; the caller cannot tell a live token from the last known one")
}

func TestAnExpiredOauthTokenIsHandedBackWhenAuthStoreDidNotRefreshIt(t *testing.T) {
	expired := time.Now().Add(-24 * time.Hour).UnixMilli()
	stub := &authStoreStub{provider: "somebody", body: map[string]any{
		"auth_type": "oauth", "refresh_mode": "none",
		"access_token": "expired-yesterday", "expires_at": expired, "leased": false,
	}}
	got, err := newResolver(t, stub.start(t), "")(context.Background(), "somebody")
	if err != nil {
		t.Fatalf("today the client ignores expires_at entirely: %v", err)
	}
	if got != "expired-yesterday" {
		t.Fatalf("value = %q", got)
	}
	t.Log("characterisation: expires_at in the past is dropped; provisioning embeds a dead token in the MCP env")
}
