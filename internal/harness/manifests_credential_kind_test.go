package harness

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// These two tests pin the premise that pickAnthropicKey's doc comment now
// states: the value shipped in ANTHROPIC_API_KEY is whatever secret the
// chosen credential carries, with no filtering by kind on either path.
//
// They are premise pins, not endorsements. noteboard todo 0ba576b0 asks
// whether this call site should prefer, rank or refuse a non-api_key
// credential; the day any of those three lands, both tests go red and the
// comment above pickAnthropicKey has to be rewritten with them. That is
// the point of pinning it.

func envValue(t *testing.T, svc msg.HarnessService, key string) string {
	t.Helper()
	prefix := key + "="
	for _, kv := range svc.Env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	t.Fatalf("no %s in Env %q", key, svc.Env)
	return ""
}

// A bound oauth credential reaches ANTHROPIC_API_KEY as its access token.
// Nothing on the bound-credential path inspects auth_type.
func TestABoundOAuthCredentialShipsItsAccessTokenAsTheAnthropicAPIKey(t *testing.T) {
	ctx := ManifestContext{
		ServerURL: "https://bridge.example",
		OS:        "linux",
		Arch:      "amd64",
		Credential: &authstoreclient.Resolved{
			Provider:    "anthropic",
			AuthType:    "oauth",
			AccessToken: "oauth-access-token-not-an-api-key",
		},
	}

	svcs, err := BuildProvision(msg.HarnessInber, ctx)
	if err != nil {
		t.Fatalf("BuildProvision: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("want 1 service, got %d", len(svcs))
	}

	got := envValue(t, svcs[0], "ANTHROPIC_API_KEY")
	if got != "oauth-access-token-not-an-api-key" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the oauth access token verbatim; "+
			"if this call site now filters by credential kind, rewrite "+
			"pickAnthropicKey's doc comment with it", got)
	}
}

// The auth-store fallback path applies no kind filter of its own either: it
// ships whatever /api/resolve/anthropic answered with.
func TestTheAuthStoreFallbackShipsANonAPIKeyCredentialUnfiltered(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		// Answer only the resolve route. A stub that served every path
		// alike could not show which request was actually made.
		if r.URL.Path != "/api/resolve/anthropic" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"provider":"anthropic","auth_type":"oauth",` +
			`"access_token":"fallback-oauth-token","api_key":""}`))
	}))
	defer srv.Close()

	ctx := ManifestContext{
		ServerURL:  "https://bridge.example",
		OS:         "linux",
		Arch:       "amd64",
		AuthClient: authstoreclient.New(srv.URL, "test-token", "llm-bridge-server"),
		Reason:     "test:manifest",
	}

	svcs, err := BuildProvision(msg.HarnessInber, ctx)
	if err != nil {
		t.Fatalf("BuildProvision: %v", err)
	}

	if gotPath != "/api/resolve/anthropic" {
		t.Errorf("resolve path = %q, want /api/resolve/anthropic", gotPath)
	}
	if !strings.Contains(gotQuery, "intended_app=llm-bridge-server") {
		t.Errorf("resolve query = %q, want it to carry intended_app=llm-bridge-server", gotQuery)
	}

	got := envValue(t, svcs[0], "ANTHROPIC_API_KEY")
	if got != "fallback-oauth-token" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the resolved oauth token verbatim; "+
			"if pickAnthropicKey now picks by kind, rewrite its doc comment with it", got)
	}
}
