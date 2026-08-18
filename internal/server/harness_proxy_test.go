package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// harnessProxyUpstream stands in for a harness backend and records the
// path each forwarded request actually arrived on. It is a bare
// HandlerFunc rather than a ServeMux on purpose: a mux would clean the
// path before the handler saw it, which is the very step that would
// hide a traversal from these tests.
type harnessProxyUpstream struct {
	server    *httptest.Server
	pathsSeen []string
}

func newHarnessProxyUpstream(t *testing.T) *harnessProxyUpstream {
	t.Helper()
	upstream := &harnessProxyUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.pathsSeen = append(upstream.pathsSeen, r.URL.Path)
		io.WriteString(w, "upstream reached")
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// serveHarnessProxyWithBackend wires the real route pattern onto a real
// ServeMux and points the inber harness at backendURL through the
// documented LLMBRIDGE_HARNESS_PROXY_<NAME> override.
//
// Going through a real mux is load-bearing. `PathValue("rest")` hands
// the handler a *decoded* path segment, so an encoded `..%2f` arrives as
// `../` — a fabricated PathValue in a direct handler call would skip the
// one step that makes this reachable.
func serveHarnessProxyWithBackend(t *testing.T, backendURL string) *httptest.Server {
	t.Helper()
	t.Setenv("LLMBRIDGE_HARNESS_PROXY_INBER", backendURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/harness-proxy/{harness}/{rest...}", (&Server{}).handleHarnessProxy)
	bridge := httptest.NewServer(mux)
	t.Cleanup(bridge.Close)
	return bridge
}

func getWithoutFollowingRedirects(t *testing.T, rawURL string) *http.Response {
	t.Helper()
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// Build the request from a pre-parsed URL with Opaque set so the
	// client transmits the percent-encoding verbatim instead of
	// normalising it on the way out.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	req := &http.Request{Method: http.MethodGet, URL: parsed}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The control. Everything below asserts that a request does NOT reach
// the upstream, and a proxy that reached nothing would pass all of them
// with the bug fully present. This test is what makes those verdicts
// mean something: an ordinary request under a PREFIXED backend must
// still arrive, on the prefix, with the body passed through.
//
// The prefix is the point. Both hardcoded backends
// (http://localhost:8200, http://localhost:8500) have no path prefix, so
// a test written against them has nothing for a `..` to escape and
// passes whether or not the traversal is refused.
func TestHarnessProxyForwardsBeneathAPrefixedBackend(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/sessions")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 — an ordinary request must still be proxied", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream reached" {
		t.Fatalf("body = %q; want the upstream response passed through unchanged", string(body))
	}
	if want := []string{"/api/v1/sessions"}; len(upstream.pathsSeen) != 1 || upstream.pathsSeen[0] != want[0] {
		t.Fatalf("upstream saw %q; want %q", upstream.pathsSeen, want)
	}
}

// The defect. A percent-encoded `..` is not cleaned by Go's ServeMux —
// it arrives at the handler decoded — and Go's HTTP client does not
// clean it on the way out either. Concatenated onto a backend that
// carries a path prefix, it addresses whatever sits above that prefix.
func TestHarnessProxyRefusesATraversalAboveAPrefixedBackend(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/..%2f..%2fsecret")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 — a rest that climbs above the backend prefix must be refused", resp.StatusCode)
	}
	if len(upstream.pathsSeen) != 0 {
		t.Errorf("upstream saw %q; want the request refused before it was sent", upstream.pathsSeen)
	}
}

// Refusal must not depend on the backend happening to carry a prefix.
// The two constants shipped today have none, and a guard that only fired
// under a prefix would leave the behaviour of the proxy dependent on
// which backend it was pointed at.
func TestHarnessProxyRefusesATraversalUnderAPrefixFreeBackendToo(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL)

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/..%2f..%2fsecret")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
	if len(upstream.pathsSeen) != 0 {
		t.Errorf("upstream saw %q; want the request refused before it was sent", upstream.pathsSeen)
	}
}

// A rest that is nothing but the climb. It cleans to exactly ".." and
// so is caught by a different arm of the guard than "../…" is —
// without a case of its own that arm can be deleted with every other
// test still green.
func TestHarnessProxyRefusesARestThatIsOnlyTheClimb(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/..%2f")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
	if len(upstream.pathsSeen) != 0 {
		t.Errorf("upstream saw %q; want the request refused before it was sent", upstream.pathsSeen)
	}
}

// An encoded LEADING slash makes rest absolute, and an absolute path
// cleans on its own terms: "/../secret" cleans to "/secret", which
// escapes nothing. Only after the leading slash is stripped does the
// climb become visible. The upstream would otherwise be sent
// "/api/v1/../secret" — measured, not reasoned.
func TestHarnessProxyRefusesATraversalHiddenBehindAnEncodedLeadingSlash(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/%2f..%2fsecret")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
	if len(upstream.pathsSeen) != 0 {
		t.Errorf("upstream saw %q; want the request refused before it was sent", upstream.pathsSeen)
	}
}

// A traversal that climbs part-way and lands back inside is not a
// containment failure — it resolves to a path under the backend prefix
// either way — so it must still be proxied. This pins the guard to
// "escapes", not to "contains a dot-segment".
func TestHarnessProxyForwardsATraversalThatStaysInsideTheBackendPrefix(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/sessions%2f..%2fmessages")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 — this resolves inside the prefix", resp.StatusCode)
	}
	if len(upstream.pathsSeen) != 1 {
		t.Fatalf("upstream saw %q; want exactly one forwarded request", upstream.pathsSeen)
	}
}

// "Escapes" is not "mentions dot-dot". A segment may legitimately
// contain two dots — a version suffix, a filename — and refusing it
// would break requests that work today while looking like a tightening.
// Without this case the guard can be swapped for a substring test on
// ".." with every other test still green.
func TestHarnessProxyForwardsASegmentThatMerelyContainsTwoDots(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api/v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/notes/draft..v2")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 — a dot-dot inside a segment name climbs nowhere", resp.StatusCode)
	}
	if want := "/api/v1/notes/draft..v2"; len(upstream.pathsSeen) != 1 || upstream.pathsSeen[0] != want {
		t.Fatalf("upstream saw %q; want %q", upstream.pathsSeen, want)
	}
}

// The guard must judge `rest` alone and leave the outgoing path
// otherwise untouched. Cleaning the whole target would collapse a
// legitimate empty segment in the operator's own backend URL and
// silently change requests that work today, so a double slash in the
// backend prefix has to survive to the upstream.
func TestHarnessProxyLeavesAnEmptySegmentInTheBackendPrefixAlone(t *testing.T) {
	upstream := newHarnessProxyUpstream(t)
	bridge := serveHarnessProxyWithBackend(t, upstream.server.URL+"/api//v1")

	resp := getWithoutFollowingRedirects(t, bridge.URL+"/api/harness-proxy/inber/sessions")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if want := "/api//v1/sessions"; len(upstream.pathsSeen) != 1 || upstream.pathsSeen[0] != want {
		t.Fatalf("upstream saw %q; want %q — the backend's own path must be forwarded unchanged", upstream.pathsSeen, want)
	}
}
