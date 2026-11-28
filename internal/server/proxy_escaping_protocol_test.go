package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/httproutes"

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
			ts := httptest.NewServer(asInternalService(srv))
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
	for _, endpoint := range []string{"messages", "entries/42"} {
		t.Run(endpoint, func(t *testing.T) {
			up, got := recordUpstreamRequestLine(t)
			srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, up.URL)
			ts := httptest.NewServer(asInternalService(srv))
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

// A mount prefix that does not match forwards the path whole, so the miss
// surfaces as an upstream 404 rather than a quiet hit on a different resource.
func TestAPrefixMissForwardsThePathWhole(t *testing.T) {
	u, _ := url.Parse("/api/agent-store/agents/x")
	if got, want := httproutes.EscapedPathAfterPrefix(u, "/api/skill-store"), "/api/agent-store/agents/x"; got != want {
		t.Errorf("escapedPathAfterPrefix = %q, want %q", got, want)
	}
}

// A prefix segment written with an escape still routes: Go's mux unescapes
// each segment before matching it, so the prefix comparison here must too.
// A naive TrimPrefix on the escaped path reads this as a miss.
func TestAnEscapeInsideTheMountPrefixIsStillAMatch(t *testing.T) {
	u, _ := url.Parse("/api/skill%2Dstore/skills/a%2Fb")
	if got, want := httproutes.EscapedPathAfterPrefix(u, "/api/skill-store"), "/skills/a%2Fb"; got != want {
		t.Errorf("escapedPathAfterPrefix = %q, want %q — the escaped prefix was read as a miss", got, want)
	}
}

// A request to the bare mount has no remainder, and the empty string says so.
func TestTheBareMountHasNoRemainder(t *testing.T) {
	u, _ := url.Parse("/api/skill-store")
	if got := httproutes.EscapedPathAfterPrefix(u, "/api/skill-store"); got != "" {
		t.Errorf("escapedPathAfterPrefix = %q, want \"\"", got)
	}
}
