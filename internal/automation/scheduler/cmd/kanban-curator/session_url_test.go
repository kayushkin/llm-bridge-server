package main

// Pins the curator's ONE call into bridgesession.URL.
//
// internal/bridgesession is thoroughly tested on its own. What nothing tested
// until now is that getSession still calls it. Reverting that call to the
// fmt.Sprintf interpolation it replaced left the whole suite green, because
// cmd/kanban-curator had no test file at all.
//
// The stakes are higher here than at the other four call sites. getSession
// reports notFound on a 404, and the caller treats notFound as a REAPABLE
// TERMINAL CONDITION: the card is moved to Failed with "reaped: bridge session
// not found (archived/pruned)". Every other error only skips the card until the
// next tick. So a session id that wanders off its route does not fail loudly —
// it lands on a neighboring route, collects that route's 404, and the curator
// closes a card whose agent is still working.
//
// Neighboring routes and their replies were measured against the
// llm-bridge-server running on this host on 2026-08-14 and are recorded in
// internal/bridgesession's package comment. The one that matters:
//
//	GET /sessions/{id}/attach-token  404 — for a session that EXISTS and may be running
//
// That is why the stub below ROUTES instead of answering every path alike. A
// stub that 404s everything reports this bug as correct behavior, which is the
// fixture mistake the parent card was filed against.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// curatorBridgeRequest is one request the stub saw. It records the router's
// view (which route matched, what it pulled out as {id}) alongside the raw
// request target, because those two can disagree: RequestURI is the bytes that
// actually went over the wire, and it is the only one of the two that shows
// whether the id was escaped before it was sent.
type curatorBridgeRequest struct {
	route      string
	id         string
	method     string
	requestURI string
}

// stubCuratorBridge answers like llm-bridge-server: Go 1.22 route patterns, one
// wildcard segment for the session id, and the neighboring routes that a
// session id carrying a slash would reach if it were interpolated raw.
//
// Deliberately not shared with cmd/email-classifier's stubBridge — that one
// lives in a different package.
func stubCuratorBridge(t *testing.T, seen *[]curatorBridgeRequest, sessionBody string) *httptest.Server {
	t.Helper()
	record := func(route string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*seen = append(*seen, curatorBridgeRequest{
				route:      route,
				id:         r.PathValue("id"),
				method:     r.Method,
				requestURI: r.RequestURI,
			})
			switch route {
			case "GET /sessions/{id}":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, sessionBody)
			case "GET /sessions/{id}/messages":
				fmt.Fprint(w, `{"model":{},"stream":{"head":0}}`)
			case "GET /sessions":
				fmt.Fprint(w, `[]`)
			case "GET /sessions/{id}/attach-token":
				http.Error(w, "session has no live pty hub", http.StatusNotFound)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
		}
	}
	mux := http.NewServeMux()
	for _, route := range []string{
		"GET /sessions",
		"GET /sessions/{id}",
		"GET /sessions/{id}/messages",
		"GET /sessions/{id}/attach-token",
		"POST /sessions/{id}/send",
	} {
		mux.HandleFunc(route, record(route))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, curatorBridgeRequest{
			route:      "unrouted " + r.URL.Path,
			method:     r.Method,
			requestURI: r.RequestURI,
		})
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const aCompletedSession = `{"state":"completed","updated_at":"2026-08-14T12:00:00Z","purpose":"autoworker"}`

// A session id that spells a neighboring route must still address the session
// itself. The expected request targets are written out as literals rather than
// built with url.PathEscape, because a fixture that computes its expectation
// the way the code does agrees with the code however wrong the code is.
func TestASessionIDThatNamesANeighboringRouteDoesNotReachIt(t *testing.T) {
	for _, tc := range []struct {
		sessionID  string
		requestURI string
		reaches    string
	}{
		{"s-1/attach-token", "/sessions/s-1%2Fattach-token", "a 404 that reaps a live card to Failed"},
		{"s-1/send", "/sessions/s-1%2Fsend", "a 405 on the send route"},
		{"s-1/messages", "/sessions/s-1%2Fmessages", "a 200 object with no state field"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []curatorBridgeRequest
			srv := stubCuratorBridge(t, &seen, aCompletedSession)

			state, _, notFound, err := sessionStateOf(srv.URL, tc.sessionID)

			if len(seen) != 1 {
				t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
			}
			if seen[0].requestURI != tc.requestURI {
				t.Errorf("request target = %q, want %q — the id was sent unescaped and reached %s",
					seen[0].requestURI, tc.requestURI, tc.reaches)
			}
			if seen[0].route != "GET /sessions/{id}" {
				t.Errorf("id %q reached route %q, not the session route", tc.sessionID, seen[0].route)
			}
			if seen[0].id != tc.sessionID {
				t.Errorf("the router read the id as %q, want the whole ref %q", seen[0].id, tc.sessionID)
			}
			if notFound {
				t.Errorf("notFound is the reap signal, and this id is a session the bridge answered 200 for; "+
					"the curator would move the card to Failed under %q", tc.sessionID)
			}
			if err != nil {
				t.Errorf("reading the session: %v", err)
			}
			if state != "completed" {
				t.Errorf("state = %q, want %q", state, "completed")
			}
		})
	}
}

// A query separator inside the id is the quiet one: interpolated raw it cuts the
// id short, and the bridge answers 200 for a DIFFERENT session. There is no
// error to notice and no status code to assert on — only the bytes on the wire
// and the id the router recovered say which session was asked about.
func TestASessionIDIsNotCutShortByAQuerySeparator(t *testing.T) {
	const sessionID = "s-1?state=completed"
	var seen []curatorBridgeRequest
	srv := stubCuratorBridge(t, &seen, aCompletedSession)

	_, _, _, err := sessionStateOf(srv.URL, sessionID)
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/s-1%3Fstate=completed" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/s-1%3Fstate=completed")
	}
	if seen[0].id != sessionID {
		t.Errorf("the bridge was asked about session %q, but the card names %q — "+
			"the separator cut the id short and this reply describes another session",
			seen[0].id, sessionID)
	}
}

// An ordinary id must pass through untouched. Without this row the two above
// are satisfied by an escaper that mangles every id equally, and the suite would
// still be green with the curator unable to address any session at all.
func TestAnOrdinarySessionIDIsSentUnchanged(t *testing.T) {
	var seen []curatorBridgeRequest
	srv := stubCuratorBridge(t, &seen, aCompletedSession)

	state, updatedAt, notFound, err := sessionStateOf(srv.URL, "autoworker-1755230000")
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/autoworker-1755230000" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/autoworker-1755230000")
	}
	if seen[0].method != "GET" {
		t.Errorf("method = %q, the bridge routes this as GET", seen[0].method)
	}
	if notFound {
		t.Error("a 200 was reported as notFound")
	}
	if state != "completed" {
		t.Errorf("state = %q, want %q", state, "completed")
	}
	if updatedAt != "2026-08-14T12:00:00Z" {
		t.Errorf("updatedAt = %q, want %q", updatedAt, "2026-08-14T12:00:00Z")
	}
}

// The ids no escaping can turn into a session name are refused before the
// request is built. Three things have to hold.
//
// The second protects live work: the refusal must NOT be reported as notFound.
// notFound means "this session is gone for good, reap the card"; a refusal
// means "this ref never named a session", and reaping on it would move a card
// to Failed on the strength of a malformed link.
//
// The third is why the error text is asserted rather than just its presence.
// Dropping the error check entirely — url, _ := bridgesession.URL(...) — leaves
// an empty URL that http.Get also rejects, so the caller still sees a non-nil
// error, still sees notFound false, and still sends nothing. All three of the
// other observations agree, and the deliberate refusal is indistinguishable
// from an accidental one whose reason has been thrown away. Only the message
// says which happened.
func TestAnUnaddressableSessionIDIsRefusedAndNeverReachesTheBridge(t *testing.T) {
	for _, tc := range []struct {
		sessionID string
		reason    string
	}{
		{"", "collection"},
		{".", "dot-segment"},
		{"..", "dot-segment"},
	} {
		t.Run(fmt.Sprintf("%q", tc.sessionID), func(t *testing.T) {
			var seen []curatorBridgeRequest
			srv := stubCuratorBridge(t, &seen, aCompletedSession)

			_, _, notFound, err := sessionStateOf(srv.URL, tc.sessionID)

			if err == nil {
				t.Fatalf("session id %q was accepted; it addresses the collection or a "+
					"dot-segment the router resolves away, never a session", tc.sessionID)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error for %q is %q, which does not say %q — the refusal's reason "+
					"was dropped and this error came from somewhere else",
					tc.sessionID, err, tc.reason)
			}
			if notFound {
				t.Errorf("session id %q was refused as notFound, which reaps the card to "+
					"Failed; a ref that never named a session is not evidence the session is gone",
					tc.sessionID)
			}
			if len(seen) != 0 {
				t.Errorf("a refused id still reached the bridge: %+v", seen)
			}
		})
	}
}

// sessionStateOf reads the fields these tests pin off getSession, which
// replaced getSessionState when the curator started gating on purpose.
func sessionStateOf(bridgeURL, sessionID string) (state, updatedAt string, notFound bool, err error) {
	sess, notFound, err := getSession(bridgeURL, sessionID)
	return sess.State, sess.UpdatedAt, notFound, err
}
