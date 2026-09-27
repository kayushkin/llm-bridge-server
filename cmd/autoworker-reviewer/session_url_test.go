package main

// Pins the reviewer's TWO calls into bridgesession.
//
// internal/bridgesession is thoroughly tested on its own. What nothing tested
// until now is that this binary still calls it. Reverting either line back to
// the fmt.Sprintf interpolation it replaced left the whole repo suite green,
// because cmd/autoworker-reviewer had no test file at all.
//
// The two sites do not carry the same risk, and the second is the reason this
// binary was pinned before cmd/kanban-dispatcher:
//
//	getSession   GET  /sessions/{id}         — reads a state
//	sendMessage  POST /sessions/{id}/send    — DELIVERS A PROMPT TO AN AGENT
//
// A session id that wanders on the read site produces a wrong answer that the
// caller can at least act cautiously on: every non-200 becomes an error, and an
// error only skips the card until the next tick. A session id that wanders on
// the send site posts a self-review nudge into a session nobody asked to
// resume. The bridge respawns that harness with `claude --resume` and the wrong
// agent starts working (llm-bridge-claudecode handler.go). Nothing replies to
// say the message went to the wrong place, and this caller is best-effort by
// design — main() logs a send failure and settles the card so the curator
// closes it — so a delivery to the wrong session is indistinguishable here from
// a delivery to the right one.
//
// Neighbouring routes and their replies were measured against the
// llm-bridge-server running on this host on 2026-08-14 and are recorded in
// internal/bridgesession's package comment. That is why the stub below ROUTES
// instead of answering every path alike: against a stub that answers every path
// the same way, an id that reaches the wrong route reads as correct behaviour,
// which is the fixture mistake the parent card was filed against.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reviewerBridgeRequest is one request the stub saw. It records the router's
// view (which route matched, what it pulled out as {id}) alongside the raw
// request target, because those two can disagree: RequestURI is the bytes that
// actually went over the wire, and it is the only one of the two that shows
// whether the id was escaped before it was sent.
type reviewerBridgeRequest struct {
	route      string
	id         string
	method     string
	requestURI string
	body       string
}

// stubReviewerBridge answers like llm-bridge-server: Go 1.22 route patterns, one
// wildcard segment for the session id, and the neighbouring routes that a
// session id carrying a slash would reach if it were interpolated raw.
//
// Deliberately not shared with cmd/kanban-curator's stubCuratorBridge or
// cmd/email-classifier's stubBridge — both live in different packages, so the
// shape is copied and the symbol names are fresh.
func stubReviewerBridge(t *testing.T, seen *[]reviewerBridgeRequest, sessionBody string) *httptest.Server {
	t.Helper()
	record := func(route string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sent, _ := io.ReadAll(r.Body)
			*seen = append(*seen, reviewerBridgeRequest{
				route:      route,
				id:         r.PathValue("id"),
				method:     r.Method,
				requestURI: r.RequestURI,
				body:       string(sent),
			})
			switch route {
			case "GET /sessions/{id}":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, sessionBody)
			case "POST /sessions/{id}/send":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
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
		sent, _ := io.ReadAll(r.Body)
		*seen = append(*seen, reviewerBridgeRequest{
			route:      "unrouted " + r.URL.Path,
			method:     r.Method,
			requestURI: r.RequestURI,
			body:       string(sent),
		})
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const aCompletedAutoworkerSession = `{"state":"completed","updated_at":"2026-08-14T12:00:00Z","purpose":"autoworker"}`

// ======================= the read site: GET /sessions/{id} =======================

// A session id that spells a neighbouring route must still address the session
// itself. The expected request targets are written out as literals rather than
// built with url.PathEscape, because a fixture that computes its expectation
// the way the code does agrees with the code however wrong the code is.
func TestASessionIDThatNamesANeighbouringRouteDoesNotReachItOnTheReadSite(t *testing.T) {
	for _, tc := range []struct {
		sessionID  string
		requestURI string
		reaches    string
	}{
		{"s-1/attach-token", "/sessions/s-1%2Fattach-token", "a 404 the reviewer reports as \"not found\""},
		{"s-1/send", "/sessions/s-1%2Fsend", "a 405 on the send route"},
		{"s-1/messages", "/sessions/s-1%2Fmessages", "a 200 object with no state field"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []reviewerBridgeRequest
			srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

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
//
// The stakes here are not just a wrong state. main() reads this state, and only
// a session reading "completed" is nudged; the id it then hands to sendMessage
// is the card's id, not the one this reply describes. So a state borrowed from
// a neighbour is what authorises the message the next test is about.
func TestASessionIDIsNotCutShortByAQuerySeparator(t *testing.T) {
	const sessionID = "autoworker-1?state=completed"
	var seen []reviewerBridgeRequest
	srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

	_, _, err := sessionStateOf(srv.URL, sessionID)
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/autoworker-1%3Fstate=completed" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/autoworker-1%3Fstate=completed")
	}
	if seen[0].id != sessionID {
		t.Errorf("the bridge was asked about session %q, but the card names %q — "+
			"the separator cut the id short and this reply describes another session",
			seen[0].id, sessionID)
	}
}

// An ordinary id must pass through untouched. Without this row the two above
// are satisfied by an escaper that mangles every id equally, and the suite would
// still be green with the reviewer unable to address any session at all.
func TestAnOrdinarySessionIDIsSentUnchangedOnTheReadSite(t *testing.T) {
	var seen []reviewerBridgeRequest
	srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

	state, updatedAt, err := sessionStateOf(srv.URL, "autoworker-nightly-1755230000")
	if err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/autoworker-nightly-1755230000" {
		t.Errorf("request target = %q, want %q", seen[0].requestURI, "/sessions/autoworker-nightly-1755230000")
	}
	if seen[0].method != "GET" {
		t.Errorf("method = %q, the bridge routes this read as GET", seen[0].method)
	}
	if state != "completed" {
		t.Errorf("state = %q, want %q", state, "completed")
	}
	if updatedAt != "2026-08-14T12:00:00Z" {
		t.Errorf("updatedAt = %q, want %q", updatedAt, "2026-08-14T12:00:00Z")
	}
}

// ======================= the send site: POST /sessions/{id}/send =======================

// The plain case, and it has to be asserted for the same reason the read site's
// plain case does: without it, an escaper that mangles every id equally passes
// every other row here while the reviewer nudges nobody.
//
// The subresource is asserted through the route rather than the path text
// alone. "send" and "messages" are both real routes on the bridge, but
// "messages" is GET-only, so a nudge POSTed there is a 405 and never reaches
// the agent.
func TestAMessageIsPostedToTheSendRouteOfTheSessionNamed(t *testing.T) {
	var seen []reviewerBridgeRequest
	srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

	if err := sendMessage(srv.URL, "autoworker-nightly-1755230000", "review pass 1"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/autoworker-nightly-1755230000/send" {
		t.Errorf("request target = %q, want %q",
			seen[0].requestURI, "/sessions/autoworker-nightly-1755230000/send")
	}
	if seen[0].route != "POST /sessions/{id}/send" {
		t.Errorf("the nudge reached route %q, not the send route — a POST to the messages "+
			"route appends to the transcript and never wakes the agent", seen[0].route)
	}
	if seen[0].method != "POST" {
		t.Errorf("method = %q, want POST", seen[0].method)
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(seen[0].body), &payload); err != nil {
		t.Fatalf("send body %q is not the JSON the bridge reads: %v", seen[0].body, err)
	}
	if payload.Message != "review pass 1" {
		t.Errorf("message = %q, want %q", payload.Message, "review pass 1")
	}
}

// The send site's own version of the neighbouring-route row. An id carrying a
// slash appends its tail to the path BEFORE "/send", so the nudge is posted at
// a path the reviewer never named.
func TestASessionIDWithASlashDoesNotMoveTheNudgeToAnotherRoute(t *testing.T) {
	for _, tc := range []struct {
		sessionID  string
		requestURI string
	}{
		{"autoworker-1/messages", "/sessions/autoworker-1%2Fmessages/send"},
		{"autoworker-1/attach-token", "/sessions/autoworker-1%2Fattach-token/send"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []reviewerBridgeRequest
			srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

			if err := sendMessage(srv.URL, tc.sessionID, "review pass 1"); err != nil {
				t.Fatalf("sendMessage: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
			}
			if seen[0].requestURI != tc.requestURI {
				t.Errorf("request target = %q, want %q — the id was sent unescaped and its "+
					"tail became path of its own", seen[0].requestURI, tc.requestURI)
			}
			if seen[0].route != "POST /sessions/{id}/send" {
				t.Errorf("the nudge reached route %q, not the send route", seen[0].route)
			}
			if seen[0].id != tc.sessionID {
				t.Errorf("the router read the id as %q, want the whole ref %q", seen[0].id, tc.sessionID)
			}
		})
	}
}

// The worst shape this file holds, and the reason the reviewer was pinned ahead
// of the dispatcher.
//
// A dot-segment inside the id does not merely miss the route. Interpolated raw,
// "victim/../other" makes the path /sessions/victim/../other/send, which a
// router resolves to /sessions/other/send BEFORE it matches any route — so the
// self-review nudge is delivered to session "other", the bridge respawns THAT
// agent with --resume, and it starts working on a review it was never part of.
// The reviewer sees a 2xx and records a successful nudge.
//
// Escaped, the dots stay inside one segment and there is nothing for the router
// to resolve, so the request stops at a session named by the whole ref.
func TestASessionIDWithADotSegmentDoesNotDeliverTheNudgeToAnotherSession(t *testing.T) {
	const sessionID = "autoworker-victim/../autoworker-bystander"
	var seen []reviewerBridgeRequest
	srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

	err := sendMessage(srv.URL, sessionID, "review pass 1")

	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
	}
	if seen[0].requestURI != "/sessions/autoworker-victim%2F..%2Fautoworker-bystander/send" {
		t.Errorf("request target = %q, want %q",
			seen[0].requestURI, "/sessions/autoworker-victim%2F..%2Fautoworker-bystander/send")
	}
	for _, got := range seen {
		if strings.Contains(got.requestURI, "/sessions/autoworker-bystander") {
			t.Errorf("the nudge was delivered to session autoworker-bystander (%q) — the dot "+
				"segment was resolved away and another agent has been resumed", got.requestURI)
		}
	}
	if seen[0].id != sessionID {
		t.Errorf("the router read the id as %q, want the whole ref %q", seen[0].id, sessionID)
	}
	if err != nil {
		t.Errorf("sendMessage: %v", err)
	}
}

// ======================= refusals, on both sites =======================

// The ids no escaping can turn into a session name are refused before the
// request is built. Three things have to hold on each site.
//
// The error text is asserted rather than just its presence. Dropping the error
// check entirely — url, _ := bridgesession.URL(...) — leaves an empty URL that
// http.Get and http.Post also reject, so the caller still sees a non-nil error
// and still sends nothing. Every other observation agrees, and the deliberate
// refusal is indistinguishable from an accidental one whose reason has been
// thrown away. Only the message says which happened. (Found by the 169th pass
// scoring the curator's equivalent file; that row SURVIVED until the reason was
// asserted.)
//
// On the send site the refusal carries its own weight. main() treats a send
// error as best-effort — it logs, settles the session and lets the curator
// close the card — so a refusal here costs one unreviewed card. Sending the
// message anyway costs a resumed agent that nobody can trace back to a card.
func TestAnUnaddressableSessionIDIsRefusedAndNeverReachesTheBridge(t *testing.T) {
	for _, tc := range []struct {
		sessionID string
		reason    string
	}{
		{"", "collection"},
		{".", "dot-segment"},
		{"..", "dot-segment"},
	} {
		t.Run(fmt.Sprintf("read/%q", tc.sessionID), func(t *testing.T) {
			var seen []reviewerBridgeRequest
			srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

			state, _, err := sessionStateOf(srv.URL, tc.sessionID)

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
				t.Errorf("state = %q for a refused id; main() nudges on state %q, and a state "+
					"read out of a refusal would authorise that nudge", state, "completed")
			}
			if len(seen) != 0 {
				t.Errorf("a refused id still reached the bridge: %+v", seen)
			}
		})

		t.Run(fmt.Sprintf("send/%q", tc.sessionID), func(t *testing.T) {
			var seen []reviewerBridgeRequest
			srv := stubReviewerBridge(t, &seen, aCompletedAutoworkerSession)

			err := sendMessage(srv.URL, tc.sessionID, "review pass 1")

			if err == nil {
				t.Fatalf("session id %q was accepted; a nudge was posted at a path that "+
					"names no session", tc.sessionID)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error for %q is %q, which does not say %q — the refusal's reason "+
					"was dropped and this error came from somewhere else",
					tc.sessionID, err, tc.reason)
			}
			if len(seen) != 0 {
				t.Errorf("a refused id still reached the bridge: %+v", seen)
			}
		})
	}
}

// sessionStateOf reads the fields these tests pin off getSession, which
// replaced getSessionState when the reviewer started gating on purpose.
func sessionStateOf(bridgeURL, sessionID string) (state, updatedAt string, err error) {
	sess, err := getSession(bridgeURL, sessionID)
	return sess.State, sess.UpdatedAt, err
}
