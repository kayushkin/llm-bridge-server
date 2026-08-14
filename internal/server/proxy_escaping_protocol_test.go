package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"testing"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge/msg"
)

// These cases drive the real mux and assert on r.RequestURI at a stand-in
// upstream — the raw request line.
//
// ⚠️ Asserting on r.URL.Path here would pin nothing. Go's server percent-
// decodes the path before a handler sees it, so an escaped %2F and a literal /
// arrive byte-identical, and the case would stay green with the escaping
// removed. RequestURI is the only field that still holds what was sent.

// recordUpstreamRequestLine returns a stand-in upstream and a pointer to the
// raw request line it last received.
func recordUpstreamRequestLine(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// A session id is one path SEGMENT. r.PathValue returns it decoded, so pasting
// it into the log-store URL hands any separator inside it its meaning back.
//
// This runs through the real mux on purpose: it is also the evidence that such
// an id ROUTES here at all. Go matches patterns against the escaped path, so
// /sessions/a%2Fb/messages is three segments to the mux and {id} captures the
// whole of a%2Fb. A reader who assumes the request 404s instead would conclude
// this defect is unreachable.
func TestTheSessionIdSurvivesAsOneSegmentOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"encoded slash", "a%2Fb", "/api/v1/sessions/a%2Fb/messages"},
		{"encoded question mark", "a%3Fx=1", "/api/v1/sessions/a%3Fx=1/messages"},
		{"encoded hash", "a%23frag", "/api/v1/sessions/a%23frag/messages"},
		{"an ordinary id is untouched", "br-archived", "/api/v1/sessions/br-archived/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, got := recordUpstreamRequestLine(t)
			srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, up.URL)
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			resp, err := http.Get(ts.URL + "/sessions/" + tc.id + "/messages")
			if err != nil {
				t.Fatalf("GET messages: %v", err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d; want 200 — the request did not even reach the handler",
					resp.StatusCode)
			}
			if *got != tc.want {
				t.Errorf("log-store received %q, want %q\n"+
					"  the caller sent id %q; a handler that pastes the DECODED id into the\n"+
					"  target addresses a different session, or a truncated path",
					*got, tc.want, tc.id)
			}
		})
	}
}

// The endpoint literal is the route's own last segment. It is read from the
// escaped path so it cannot be moved by whatever the id decodes to.
func TestTheEndpointLiteralIsNotMovedByTheId(t *testing.T) {
	for _, endpoint := range []string{"messages", "history"} {
		t.Run(endpoint, func(t *testing.T) {
			up, got := recordUpstreamRequestLine(t)
			srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, up.URL)
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			resp, err := http.Get(ts.URL + "/sessions/a%2Fb%3Fx/" + endpoint)
			if err != nil {
				t.Fatalf("GET %s: %v", endpoint, err)
			}
			resp.Body.Close()

			want := "/api/v1/sessions/a%2Fb%3Fx/" + endpoint
			if *got != want {
				t.Errorf("log-store received %q, want %q", *got, want)
			}
		})
	}
}

// ⚠️ The escaping belongs at the point of USE, not the point of READ. The
// handler's id feeds two consumers that want opposite forms: the manager's
// FlushLogStoreWrites indexes sessions by the REAL decoded id, and only the URL
// wants it escaped. Escaping once at the read site satisfies the wire test
// above and quietly hands the manager an id no session has — and the manager is
// a concrete type here, so nothing in this package can observe that.
//
// logStoreSessionURL exists so the decision is somewhere a test can call it. It
// takes the decoded id and escapes it itself, which is what makes "the caller
// keeps the real id" a property of the signature rather than of a comment.
func TestLogStoreSessionURLEscapesTheIdItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sessionID string
		want      string
	}{
		{"a slash", "a/b", "http://ls/api/v1/sessions/a%2Fb/messages"},
		{"a question mark", "a?x=1", "http://ls/api/v1/sessions/a%3Fx=1/messages"},
		{"a hash", "a#frag", "http://ls/api/v1/sessions/a%23frag/messages"},
		{"an ordinary id", "br-archived", "http://ls/api/v1/sessions/br-archived/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := logStoreSessionURL("http://ls", tc.sessionID, "messages")
			if got != tc.want {
				t.Errorf("logStoreSessionURL(%q) = %q, want %q", tc.sessionID, got, tc.want)
			}
		})
	}
}

// The id it is given must be the DECODED one — hand it an already-escaped id
// and it escapes the escape. This is what a read-site escape would produce, and
// it addresses a session whose id contains a literal "%2F".
func TestLogStoreSessionURLIsGivenTheDecodedId(t *testing.T) {
	doubled := logStoreSessionURL("http://ls", url.PathEscape("a/b"), "messages")
	if want := "http://ls/api/v1/sessions/a%252Fb/messages"; doubled != want {
		t.Fatalf("pre-escaped id produced %q, want %q", doubled, want)
	}
	if once := logStoreSessionURL("http://ls", "a/b", "messages"); once == doubled {
		t.Error("escaping twice is indistinguishable from escaping once; " +
			"a read-site escape would no longer be detectable")
	}
}

// storeProxyRequest drives one of the two seed store proxies through the real
// mux with a valid runner token, and returns what the upstream received.
func storeProxyRequest(t *testing.T, envKey, requestPath string) string {
	t.Helper()
	up, got := recordUpstreamRequestLine(t)
	t.Setenv(envKey, up.URL)

	srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, up.URL)
	const token = "runner-token-for-this-test"
	if err := srv.harnessStore.SetMachineRunnerTokenHash("m_test",
		harnessstore.HashRunnerToken(token)); err != nil {
		t.Fatalf("set runner token: %v", err)
	}

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodGet, ts.URL+requestPath, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", requestPath, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 — the request never reached the proxy", resp.StatusCode)
	}
	return *got
}

// The two seed store proxies are the same shape dash and llmux carried: a
// mount prefix sliced off r.URL.Path and pasted onto the upstream base.
func TestTheStoreProxiesForwardAnEscapedSegment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		request string
		want    string
	}{
		{"skill-store", "SKILL_STORE_URL", "/api/skill-store/skills/a%2Fb/files", "/skills/a%2Fb/files"},
		{"agent-store", "AGENT_STORE_URL", "/api/agent-store/agents/a%2Fb", "/agents/a%2Fb"},
		{"encoded question mark", "SKILL_STORE_URL", "/api/skill-store/skills/a%3Fx=1", "/skills/a%3Fx=1"},
		{"encoded hash", "SKILL_STORE_URL", "/api/skill-store/skills/a%23frag", "/skills/a%23frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := storeProxyRequest(t, tc.env, tc.request); got != tc.want {
				t.Errorf("upstream received %q, want %q", got, tc.want)
			}
		})
	}
}

// ⚠️ The store proxies normalise the remainder with path.Clean, and cleaning
// the DECODED path is worse than the plain misrouting: an escaped %2F..%2F
// decodes into real separators that Clean then RESOLVES, so a single segment
// the caller sent walks up the upstream's path instead.
//
// Measured on the decoded read: /api/skill-store/a%2F..%2F..%2Fadmin reached
// the upstream as /admin. On the escaped form a %2F is an ordinary character
// and stays inside its segment, so it arrives whole.
func TestCleanDoesNotResolveAnEscapedDotDotIntoATraversal(t *testing.T) {
	got := storeProxyRequest(t, "SKILL_STORE_URL", "/api/skill-store/a%2F..%2F..%2Fadmin")

	if want := "/a%2F..%2F..%2Fadmin"; got != want {
		t.Errorf("upstream received %q, want %q\n"+
			"  the caller sent ONE segment; Clean over the decoded path turned it into\n"+
			"  a walk up the upstream's path", got, want)
	}
}

// ⚠️ A dot segment the caller LITERALLY sent never reaches the handler, so a
// case that sends one through the mux and finds it normalised is vacuous — it
// is watching the mux work, not the handler.
//
// Measured: net/http's ServeMux answers 307 to the cleaned path and the request
// is never routed. Which means the only dot segments the handler's path.Clean
// ever saw were ones the DECODE manufactured out of an escaped %2F.., and
// resolving those was the traversal the case above pins. Cleaning the escaped
// form makes Clean a no-op for everything arriving this way, which is what it
// should always have been.
//
// So this is stated at two layers, both named:
//   - through the mux, the MUX normalises and the handler is not involved;
//   - called directly, the helper still cleans, for a caller that is not the mux.
func TestRealDotSegmentsAreNormalisedByTheMuxNotTheHandler(t *testing.T) {
	srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, "http://unused")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(ts.URL + "/api/skill-store/skills/./x/../y")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want 307 — a literal dot segment is supposed to be "+
			"redirected by the mux before any handler sees it", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Location"), "/api/skill-store/skills/y"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	// The other layer: called directly, the helper does clean. This is the half
	// that a caller which is not the mux relies on.
	u, _ := url.Parse("/api/skill-store/skills/./x/../y")
	if got, want := cleanEscapedPathAfterPrefix(u, "/api/skill-store"), "/skills/y"; got != want {
		t.Errorf("cleanEscapedPathAfterPrefix = %q, want %q", got, want)
	}
	if path.Clean("/skills/./x/../y") != "/skills/y" {
		t.Fatal("path.Clean no longer resolves dot segments; this case rests on it")
	}
}

// ⚠️ This is the fixture trap, pinned so the next reader does not fall into it.
// A space witnesses nothing: Go re-escapes %20 when http.NewRequest parses the
// forwarded target, so the upstream sees a%20b either way.
//
// The claim is a WIRE-level one. At the string level the two reads visibly
// differ ("a b" against "a%20b"); they converge only after the target is
// parsed back. The assertion is two-sided so that if that ever stops being
// true, this case fails and says the guidance is stale rather than quietly
// becoming a real test.
func TestASpaceDoesNotWitnessTheEscapingDefect(t *testing.T) {
	fixed := storeProxyRequest(t, "SKILL_STORE_URL", "/api/skill-store/skills/a%20b")
	if fixed != "/skills/a%20b" {
		t.Errorf("upstream received %q, want /skills/a%%20b", fixed)
	}

	// The same value forwarded the broken way, through the decoded path.
	up, got := recordUpstreamRequestLine(t)
	u, _ := url.Parse("/api/skill-store/skills/a%20b")
	resp, err := http.Get(up.URL + path.Clean("/"+u.Path[len("/api/skill-store"):]))
	if err != nil {
		t.Fatalf("probe the broken read: %v", err)
	}
	resp.Body.Close()

	if *got != fixed {
		t.Errorf("a space now DOES discriminate: fixed read put %q on the wire, the decoded read put %q.\n"+
			"  A space fixture is no longer vacuous — update the guidance in proxy_path.go",
			*got, fixed)
	}
}

// A mount prefix that does not match forwards the path whole, so the miss
// surfaces as an upstream 404 rather than a quiet hit on a different resource.
func TestAPrefixMissForwardsThePathWhole(t *testing.T) {
	u, _ := url.Parse("/api/agent-store/agents/x")
	if got, want := escapedPathAfterPrefix(u, "/api/skill-store"), "/api/agent-store/agents/x"; got != want {
		t.Errorf("escapedPathAfterPrefix = %q, want %q", got, want)
	}
}

// A prefix segment written with an escape still routes: Go's mux unescapes
// each segment before matching it, so the prefix comparison here must too.
// A naive TrimPrefix on the escaped path reads this as a miss.
func TestAnEscapeInsideTheMountPrefixIsStillAMatch(t *testing.T) {
	u, _ := url.Parse("/api/skill%2Dstore/skills/a%2Fb")
	if got, want := escapedPathAfterPrefix(u, "/api/skill-store"), "/skills/a%2Fb"; got != want {
		t.Errorf("escapedPathAfterPrefix = %q, want %q — the escaped prefix was read as a miss", got, want)
	}
}

// A request to the bare mount has no remainder, and the empty string says so.
func TestTheBareMountHasNoRemainder(t *testing.T) {
	u, _ := url.Parse("/api/skill-store")
	if got := escapedPathAfterPrefix(u, "/api/skill-store"); got != "" {
		t.Errorf("escapedPathAfterPrefix = %q, want \"\"", got)
	}
	// The store proxies clean that into a bare "/", which is the upstream root.
	if got := cleanEscapedPathAfterPrefix(u, "/api/skill-store"); got != "/" {
		t.Errorf("cleanEscapedPathAfterPrefix = %q, want \"/\"", got)
	}
}
