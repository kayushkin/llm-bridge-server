package main

// Pins the dispatcher's single call into bridgesession.
//
// internal/bridgesession is thoroughly tested on its own. What nothing tested
// until now is that this binary still calls it. Reverting the line back to the
// fmt.Sprintf interpolation it replaced left the whole repo suite green,
// because cmd/kanban-dispatcher had no test file at all — the last of the three
// bare binaries named by the parent card.
//
//	getBridgeSession  GET /sessions/{id}  — reads a sibling session's state
//
// One site, and it is a guard rather than an action, which makes it easy to
// read as the harmless one of the three. It is not. decideReuse walks the
// sibling sub-cards under a parent, reads each linked session's state, and the
// FIRST one answering "idle" inside the revive window is returned as the
// session to reuse. main() then publishes the new card's prompt into that
// session id over NATS, links the card to it and moves the card to In Progress.
//
// So this read is the whole of the check standing between a queued card and an
// agent that is already busy. The failure that matters here is not an error —
// decideReuse skips a sibling on any error and on any state that is not "idle",
// so a refused or failed read costs one tick. It is a BORROWED "idle": an id
// that wanders reads some other session's state, and the dispatcher hands work
// to the session it did not ask about. The guard passes on one session's
// evidence and the prompt goes to another.
//
// Neighboring routes and their replies were measured against the
// llm-bridge-server running on this host on 2026-08-14 and are recorded in
// internal/bridgesession's package comment. That is why the stub below ROUTES
// instead of answering every path alike: against a stub that answers every path
// the same way, an id that reaches the wrong route reads as correct behavior,
// which is the fixture mistake the parent card was filed against.
//
// # What this file deliberately does NOT hold
//
// Named here rather than left for the next reader to discover by mutating them.
// Every row below survives sabotage of this file on purpose; each is somebody
// else's boundary, and a score that quietly dropped them would read higher and
// mean less.
//
//   - The 404-to-"not found" mapping. Which reply means "forgotten session" is
//     email-classifier's distinction, load-bearing there and merely an error
//     here — decideReuse skips the sibling either way.
//   - Any non-200 read being refused rather than decoded. Same reason: every
//     non-200 is one skipped sibling and one more fresh session.
//   - Trimming a trailing slash off the base URL. That belongs to
//     internal/bridgesession, which tests it directly.
//
// What this file DOES hold is the one thing no other suite can: that this
// binary still calls the helper, with the id it was given, on the route it
// named.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dispatcherBridgeRequest is one request the stub saw. It records the router's
// view (which route matched, what it pulled out as {id}) alongside the raw
// request target, because those two can disagree: RequestURI is the bytes that
// actually went over the wire, and it is the only one of the two that shows
// whether the id was escaped before it was sent.
type dispatcherBridgeRequest struct {
	route      string
	id         string
	method     string
	requestURI string
}

// stubDispatcherBridge answers like llm-bridge-server: Go 1.22 route patterns,
// one wildcard segment for the session id, and the neighboring routes that a
// session id carrying a slash would reach if it were interpolated raw.
//
// Deliberately not shared with cmd/autoworker-reviewer's stubReviewerBridge,
// cmd/kanban-curator's stubCuratorBridge or cmd/email-classifier's stubBridge —
// they all live in different packages, so the shape is copied and the symbol
// names are fresh.
func stubDispatcherBridge(t *testing.T, seen *[]dispatcherBridgeRequest, sessionBody string) *httptest.Server {
	t.Helper()
	record := func(route string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			*seen = append(*seen, dispatcherBridgeRequest{
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
		_, _ = io.Copy(io.Discard, r.Body)
		*seen = append(*seen, dispatcherBridgeRequest{
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

// The reply that matters. An idle sibling inside the revive window is the one
// answer that makes decideReuse hand a card to an existing session, so every
// row below reads a session that WOULD be reused if the id reached it.
const anIdleSiblingSession = `{"state":"idle","updated_at":"2026-08-14T12:00:00Z"}`

// A session id that spells a neighboring route must still address the session
// itself. The expected request targets are written out as literals rather than
// built with url.PathEscape, because a fixture that computes its expectation
// the way the code does agrees with the code however wrong the code is.
func TestASessionIDThatNamesANeighboringRouteDoesNotReachItOnTheReadSite(t *testing.T) {
	for _, tc := range []struct {
		sessionID  string
		requestURI string
		reaches    string
	}{
		{"tcl-1/attach-token", "/sessions/tcl-1%2Fattach-token", "a 404 the dispatcher reports as \"not found\""},
		{"tcl-1/send", "/sessions/tcl-1%2Fsend", "a 405 on the send route"},
		{"tcl-1/messages", "/sessions/tcl-1%2Fmessages", "a 200 object with no state field"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []dispatcherBridgeRequest
			srv := stubDispatcherBridge(t, &seen, anIdleSiblingSession)

			state, _, err := sessionStateOf(srv.URL, tc.sessionID)

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
			if err != nil {
				t.Errorf("reading the session: %v", err)
			}
			if state != "idle" {
				t.Errorf("state = %q, want %q", state, "idle")
			}
		})
	}
}

// A query separator inside the id is the quiet one: interpolated raw it cuts
// the id short, and the bridge answers 200 for a DIFFERENT session. There is no
// error to notice and no status code to assert on — only the bytes on the wire
// and the id the router recovered say which session was asked about.
//
// This is the borrowed "idle" in its purest form. decideReuse asks whether
// sibling tcl-1?... is free, a truncated id answers for tcl-1, and if THAT
// session happens to be idle the dispatcher publishes the new prompt into the
// session the card actually links to — which was never asked about at all.
func TestASessionIDIsNotCutShortByAQuerySeparator(t *testing.T) {
	const sessionID = "tcl-1?state=idle"
	var seen []dispatcherBridgeRequest
	srv := stubDispatcherBridge(t, &seen, anIdleSiblingSession)

	_, _, err := sessionStateOf(srv.URL, sessionID)
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/tcl-1%3Fstate=idle" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/tcl-1%3Fstate=idle")
	}
	if seen[0].id != sessionID {
		t.Errorf("the bridge was asked about session %q, but the card links %q — "+
			"the separator cut the id short and this reply describes another session",
			seen[0].id, sessionID)
	}
}

// The worst shape a read site can carry.
//
// Interpolated raw, "victim/../bystander" makes the path
// /sessions/victim/../bystander, which a router resolves to /sessions/bystander
// BEFORE it matches any route — so decideReuse reads the BYSTANDER's state and
// then, if that reads idle, returns the victim's id as the session to reuse.
// The check and the dispatch name two different sessions, and the busy victim
// is handed a second card on a bystander's evidence.
//
// Escaped, the dots stay inside one segment and there is nothing for the router
// to resolve, so the request stops at a session named by the whole ref.
func TestASessionIDWithADotSegmentDoesNotReadAnotherSessionsState(t *testing.T) {
	const sessionID = "tcl-victim/../tcl-bystander"
	var seen []dispatcherBridgeRequest
	srv := stubDispatcherBridge(t, &seen, anIdleSiblingSession)

	_, _, err := sessionStateOf(srv.URL, sessionID)

	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/tcl-victim%2F..%2Ftcl-bystander" {
		t.Errorf("request target = %q, want %q",
			seen[0].requestURI, "/sessions/tcl-victim%2F..%2Ftcl-bystander")
	}
	for _, got := range seen {
		if strings.Contains(got.requestURI, "/sessions/tcl-bystander") {
			t.Errorf("the state was read from session tcl-bystander (%q) — the dot segment was "+
				"resolved away and a card would be dispatched on another session's evidence",
				got.requestURI)
		}
	}
	if seen[0].id != sessionID {
		t.Errorf("the router read the id as %q, want the whole ref %q", seen[0].id, sessionID)
	}
	if err != nil {
		t.Errorf("reading the session: %v", err)
	}
}

// An ordinary id must pass through untouched, and both fields it returns have
// to arrive. Without this row every other row here is satisfied by an escaper
// that mangles all ids equally, and the suite stays green with the dispatcher
// unable to address any session at all — which reads as "no sibling is idle"
// and quietly turns every reuse into a fresh session.
//
// updated_at is asserted because it is half the guard: decideReuse compares it
// against reviveWindow, and an updated_at lost in decoding leaves an idle
// sibling of any age eligible for reuse.
func TestAnOrdinarySessionIDIsSentUnchangedOnTheReadSite(t *testing.T) {
	var seen []dispatcherBridgeRequest
	srv := stubDispatcherBridge(t, &seen, anIdleSiblingSession)

	state, updatedAt, err := sessionStateOf(srv.URL, "br_1777573132534000506")
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/br_1777573132534000506" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/br_1777573132534000506")
	}
	if seen[0].method != "GET" {
		t.Errorf("method = %q, the bridge routes this read as GET", seen[0].method)
	}
	if state != "idle" {
		t.Errorf("state = %q, want %q", state, "idle")
	}
	if updatedAt != "2026-08-14T12:00:00Z" {
		t.Errorf("updatedAt = %q, want %q — decideReuse compares it against reviveWindow, "+
			"and an empty one skips the age check entirely", updatedAt, "2026-08-14T12:00:00Z")
	}
}

// The ids no escaping can turn into a session name are refused before the
// request is built.
//
// The error text is asserted rather than just its presence. Dropping the error
// check entirely — url, _ := bridgesession.URL(...) — leaves an empty URL that
// http.Get also rejects, so the caller still sees a non-nil error and still
// sends nothing. Every other observation agrees, and the deliberate refusal is
// indistinguishable from an accidental one whose reason has been thrown away.
// Only the message says which happened. (Found by the 169th pass scoring the
// curator's equivalent file; that row SURVIVED until the reason was asserted.)
//
// The empty id is not hypothetical here. decideReuse takes sid from a card
// link's entity_ref and only skips the sibling when sid is ""; every other
// unusable value reaches this call. Unrefused, "" addresses the session
// COLLECTION — GET /sessions answers 200 with a JSON array, which decodes into
// an empty state, so the sibling is skipped for want of "idle" rather than
// because the id was unusable.
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
			var seen []dispatcherBridgeRequest
			srv := stubDispatcherBridge(t, &seen, anIdleSiblingSession)

			state, updatedAt, err := sessionStateOf(srv.URL, tc.sessionID)

			if err == nil {
				t.Fatalf("session id %q was accepted; it addresses the collection or a "+
					"dot-segment the router resolves away, never a session", tc.sessionID)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error for %q is %q, which does not say %q — the refusal's reason "+
					"was dropped and this error came from somewhere else",
					tc.sessionID, err, tc.reason)
			}
			if state != "" {
				t.Errorf("state = %q for a refused id; decideReuse reuses a session on state %q, "+
					"and a state read out of a refusal would authorise that reuse", state, "idle")
			}
			if updatedAt != "" {
				t.Errorf("updatedAt = %q for a refused id, want empty", updatedAt)
			}
			if len(seen) != 0 {
				t.Errorf("a refused id still reached the bridge: %+v", seen)
			}
		})
	}
}

// sessionStateOf reads the fields these tests pin off getBridgeSession, which
// replaced getBridgeSession.
func sessionStateOf(bridgeURL, sessionID string) (state, updatedAt string, err error) {
	sess, err := getBridgeSession(bridgeURL, sessionID)
	return sess.State, sess.UpdatedAt, err
}
