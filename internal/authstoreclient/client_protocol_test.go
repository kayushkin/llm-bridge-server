package authstoreclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests judge this client against auth-store's ACTUAL routes rather than
// against its own doc comments. A fake written from the client cannot disagree
// with the client, so every expectation below is derived from
// auth-store/internal/server/server.go and confirmed against a real auth-store
// binary running on a throwaway database (2026-08-10):
//
//	GET    /api/credentials                 protected  (bearer only)
//	GET    /api/credentials/{id}            protected
//	POST   /api/credentials                 protected  -> 201
//	PUT    /api/credentials/{id}            protected
//	DELETE /api/credentials/{id}            protected  -> 204
//	GET    /api/credentials/{id}/resolve    keyAccess
//	GET    /api/resolve/{provider}          keyAccess
//
// protected = bearer token (when auth-store has one configured).
// keyAccess = bearer PLUS X-Auth-App and X-Auth-Reason; missing either is a
// 400 with the plain-text body "X-Auth-App and X-Auth-Reason are required".
// Unknown id or provider is 404 with a JSON {"error": ...} body; a bad bearer
// is 401 with a plain-text body.
//
// The list route returns a BARE JSON ARRAY, not an object with a
// "credentials" key.

// capture records the single request a client made.
type capture struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

// newRecordingAuthStore returns a client pointed at a server that records the
// request and replies with status and body.
func newRecordingAuthStore(t *testing.T, status int, body string) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(buf)
		}
		got.body = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "probe-token", "llm-bridge-server"), got
}

// resolvedFixtureFromRealAuthStore is a byte-for-byte capture of what a real
// auth-store answered for GET /api/resolve/anthropic on an api_key credential.
// Keeping the real bytes is the point: a hand-written fixture would only prove
// this package agrees with itself.
const resolvedFixtureFromRealAuthStore = `{
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

func TestResolveByIDUsesTheRouteAuthStoreServes(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, resolvedFixtureFromRealAuthStore)

	if _, err := c.Resolve(context.Background(), "cred_abc", "bind-instance"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.method != "GET" {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.path != "/api/credentials/cred_abc/resolve" {
		t.Errorf("path = %q, want /api/credentials/cred_abc/resolve", got.path)
	}
	if got.query != "" {
		t.Errorf("query = %q, want none", got.query)
	}
}

func TestResolveByProviderUsesTheOtherRouteAuthStoreServes(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, resolvedFixtureFromRealAuthStore)

	if _, err := c.ResolveByProvider(context.Background(), "anthropic", "", "", "spawn"); err != nil {
		t.Fatalf("ResolveByProvider: %v", err)
	}
	if got.path != "/api/resolve/anthropic" {
		t.Errorf("path = %q, want /api/resolve/anthropic", got.path)
	}
	// auth-store treats an absent account as "any account" and an absent
	// intended_app as "the calling app", so omitting them is meaningful and
	// must not become account=&intended_app=.
	if got.query != "" {
		t.Errorf("query = %q, want no query when account and intended_app are empty", got.query)
	}
}

func TestResolveByProviderSpellsTheQueryParametersAuthStoreReads(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, resolvedFixtureFromRealAuthStore)

	if _, err := c.ResolveByProvider(context.Background(), "google", "default", "dash", "calendar"); err != nil {
		t.Fatalf("ResolveByProvider: %v", err)
	}
	q, err := parseQuery(got.query)
	if err != nil {
		t.Fatalf("parse query %q: %v", got.query, err)
	}
	// handleResolveByProvider reads exactly these two spellings.
	if q["account"] != "default" {
		t.Errorf("account = %q, want default (auth-store reads ?account=)", q["account"])
	}
	if q["intended_app"] != "dash" {
		t.Errorf("intended_app = %q, want dash (auth-store reads ?intended_app=)", q["intended_app"])
	}
}

func TestResolveRoutesCarryTheHeadersKeyAccessDemands(t *testing.T) {
	// This is the whole reason a resolve can fail with a 400 that mentions no
	// credential at all: keyAccess rejects the request before the handler runs.
	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"by id", func(c *Client) error {
			_, err := c.Resolve(context.Background(), "cred_abc", "bind-instance")
			return err
		}},
		{"by provider", func(c *Client) error {
			_, err := c.ResolveByProvider(context.Background(), "anthropic", "", "", "spawn")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, got := newRecordingAuthStore(t, 200, resolvedFixtureFromRealAuthStore)
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.header.Get("X-Auth-App") == "" {
				t.Error("X-Auth-App is absent; auth-store answers 400 without it")
			}
			if got.header.Get("X-Auth-Reason") == "" {
				t.Error("X-Auth-Reason is absent; auth-store answers 400 without it")
			}
			if got.header.Get("Authorization") != "Bearer probe-token" {
				t.Errorf("Authorization = %q", got.header.Get("Authorization"))
			}
		})
	}
}

// TestEveryRequestNamesItsCaller is the regression guard for the defect this
// file was written to find. auth-store's logCRUD records X-Auth-App for
// create, update and delete. Those routes use `protected`, so they do NOT
// reject a request that omits it — they log a blank app instead. This client
// used to send the header only when a reason was supplied, and no CRUD call
// supplies one, so every credential llm-bridge-server created or deleted was
// anonymous in the credential vault's audit log. Confirmed against a real
// auth-store: two rows, action=create and action=delete, both app=''.
//
// list and get are covered too. auth-store does not audit them today, so
// those two cases pin a cheap consistency rather than a known defect: one
// client, one identity, on every request it makes.
func TestEveryRequestNamesItsCaller(t *testing.T) {
	const oneCredential = `{"id":"cred_abc","provider":"anthropic","auth_type":"api_key"}`
	for _, tc := range []struct {
		name string
		body string
		call func(c *Client) error
	}{
		{"create", oneCredential, func(c *Client) error {
			_, err := c.Create(context.Background(), CredentialInput{Provider: "anthropic", AuthType: "api_key"})
			return err
		}},
		{"delete", "", func(c *Client) error {
			return c.Delete(context.Background(), "cred_abc")
		}},
		{"list", `[]`, func(c *Client) error {
			_, err := c.List(context.Background(), ListFilter{})
			return err
		}},
		{"get", oneCredential, func(c *Client) error {
			_, err := c.Get(context.Background(), "cred_abc")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, got := newRecordingAuthStore(t, 200, tc.body)
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.header.Get("X-Auth-App") != "llm-bridge-server" {
				t.Errorf("X-Auth-App = %q, want llm-bridge-server; auth-store logs this "+
					"field for every credential mutation and a blank one makes the "+
					"audit trail useless", got.header.Get("X-Auth-App"))
			}
		})
	}
}

func TestCreateAcceptsThe201AuthStoreActuallyReturns(t *testing.T) {
	// Measured: POST /api/credentials answers 201, not 200. A client that
	// accepted only 200 would fail after the credential was already written,
	// and a retry would create a second copy.
	c, _ := newRecordingAuthStore(t, 201, `{"id":"cred_new","provider":"anthropic","auth_type":"api_key"}`)

	cred, err := c.Create(context.Background(), CredentialInput{Provider: "anthropic", AuthType: "api_key"})
	if err != nil {
		t.Fatalf("Create rejected the 201 auth-store returns: %v", err)
	}
	if cred.ID != "cred_new" {
		t.Errorf("ID = %q, want cred_new", cred.ID)
	}
}

func TestDeleteAcceptsThe204AuthStoreActuallyReturns(t *testing.T) {
	c, _ := newRecordingAuthStore(t, 204, "")
	if err := c.Delete(context.Background(), "cred_abc"); err != nil {
		t.Fatalf("Delete rejected the 204 auth-store returns: %v", err)
	}
}

func TestListSendsTheFilterSpellingsAuthStoreReads(t *testing.T) {
	c, got := newRecordingAuthStore(t, 200, `[]`)

	_, err := c.List(context.Background(), ListFilter{Provider: "anthropic", Owner: "me", IntendedApp: "dash"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.path != "/api/credentials" {
		t.Errorf("path = %q, want /api/credentials", got.path)
	}
	q, err := parseQuery(got.query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	// handleListCredentials reads exactly these three.
	for k, want := range map[string]string{"provider": "anthropic", "owner": "me", "intended_app": "dash"} {
		if q[k] != want {
			t.Errorf("%s = %q, want %q", k, q[k], want)
		}
	}
}

func TestListDecodesTheBareArrayAuthStoreReturns(t *testing.T) {
	// handleListCredentials writes []credentialView directly — a bare array.
	// If it ever grows an envelope this test is how we find out.
	c, _ := newRecordingAuthStore(t, 200, `[{"id":"cred_a","provider":"anthropic","auth_type":"api_key","api_key":"sk-a...ALUE","enabled":true,"priority":100}]`)

	creds, err := c.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(creds) != 1 || creds[0].ID != "cred_a" {
		t.Fatalf("creds = %+v, want one credential cred_a", creds)
	}
	// The list route masks secrets. This value is NOT usable and the only
	// correct use of it is display.
	if creds[0].APIKey != "sk-a...ALUE" {
		t.Errorf("APIKey = %q, want the masked form auth-store sends", creds[0].APIKey)
	}
}

func TestResolvedDecodesEveryFieldTheRealPayloadCarries(t *testing.T) {
	var r Resolved
	if err := json.Unmarshal([]byte(resolvedFixtureFromRealAuthStore), &r); err != nil {
		t.Fatalf("decode real auth-store payload: %v", err)
	}
	if r.APIKey != "sk-ant-SECRET-VALUE" {
		t.Errorf("APIKey = %q — the field name on the wire is api_key", r.APIKey)
	}
	if r.AuthType != "api_key" {
		t.Errorf("AuthType = %q", r.AuthType)
	}
	if r.Account != "default" {
		t.Errorf("Account = %q — the field name on the wire is account", r.Account)
	}
	if r.RefreshMode != "server" {
		t.Errorf("RefreshMode = %q", r.RefreshMode)
	}
	if got := r.Secret(); got != "sk-ant-SECRET-VALUE" {
		t.Errorf("Secret() = %q, want the api_key for an api_key credential", got)
	}
}

func TestSecretPicksTheFieldThatMatchesTheAuthType(t *testing.T) {
	// toResolved fills api_key for api_key credentials and access_token for
	// oauth and token ones. It never fills both, so reading the wrong one
	// yields an empty secret rather than a wrong one.
	for _, tc := range []struct {
		authType string
		r        Resolved
		want     string
	}{
		{"api_key", Resolved{AuthType: "api_key", APIKey: "sk-key"}, "sk-key"},
		{"oauth", Resolved{AuthType: "oauth", AccessToken: "tok-oauth"}, "tok-oauth"},
		{"token", Resolved{AuthType: "token", AccessToken: "tok-plain"}, "tok-plain"},
		// auth-store's toResolved fills username/password/host for these and
		// no single secret string exists.
		{"password", Resolved{AuthType: "password"}, ""},
	} {
		t.Run(tc.authType, func(t *testing.T) {
			if got := tc.r.Secret(); got != tc.want {
				t.Errorf("Secret() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRefusesAnEmptyReasonBeforeAuthStoreCanReject(t *testing.T) {
	// An empty reason would earn a 400 from keyAccess. Both resolve methods
	// refuse locally, which turns a confusing server error into a clear one.
	c, _ := newRecordingAuthStore(t, 200, resolvedFixtureFromRealAuthStore)

	if _, err := c.Resolve(context.Background(), "cred_abc", ""); err == nil {
		t.Error("Resolve accepted an empty reason; auth-store answers 400")
	}
	if _, err := c.ResolveByProvider(context.Background(), "anthropic", "", "", ""); err == nil {
		t.Error("ResolveByProvider accepted an empty reason; auth-store answers 400")
	}
}

func TestTheStatusesAuthStoreUsesForFailureAllBecomeErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"missing audit headers", 400, "X-Auth-App and X-Auth-Reason are required", "400"},
		{"bad bearer", 401, "unauthorized", "401"},
		{"unknown provider", 404, `{"error":"no credentials for provider nosuch account=\"\" intended_app=\"\""}`, "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newRecordingAuthStore(t, tc.status, tc.body)
			_, err := c.ResolveByProvider(context.Background(), "nosuch", "", "", "probe")
			if err == nil {
				t.Fatalf("status %d was treated as success", tc.status)
			}
			// The status must survive into the message; "auth-store said no"
			// without the code is not actionable.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not carry the status %s", err, tc.want)
			}
		})
	}
}

// parseQuery avoids net/url so a mis-encoded query cannot be silently
// normalised by the same package that built it.
func parseQuery(raw string) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, "&") {
		k, v, found := strings.Cut(pair, "=")
		if !found {
			out[k] = ""
			continue
		}
		out[k] = strings.ReplaceAll(v, "%2F", "/")
	}
	return out, nil
}
