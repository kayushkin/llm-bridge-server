package main

// Pins the classifier's two calls into internal/bridgesession: getSession
// (bridgesession.URL) and getSessionPage (bridgesession.SubresourceURL).
//
// internal/bridgesession is tested on its own. What nothing tested until now is
// that these two functions still call it. Reverting either to fmt.Sprintf
// interpolation left cmd/kanban-classifier green, while the same revert fails
// 9-16 tests in each of the other four packages that call bridgesession.
//
// The stub ROUTES instead of answering every path alike, with the neighboring
// routes llm-bridge-server serves (recorded in internal/bridgesession's package
// comment). A stub that answers every path alike reports a session id that
// wandered onto a neighboring route as correct behavior.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// classifierBridgeRequest is one request the stub saw: the route the router
// matched, the {id} it pulled out, and the raw request target. RequestURI is
// the bytes that went over the wire, and it is the only one of these that shows
// whether the id was escaped before it was sent.
type classifierBridgeRequest struct {
	route      string
	id         string
	requestURI string
	query      string
}

const (
	aClassifierSessionBody = `{"sessionId":"from-the-bridge","state":"idle","purpose":"autoworker"}`
	aClassifierTurnPage    = `{"model":{"turns":[],"entries":{},"validator":{"eventCount":7}}}`
)

func stubClassifierBridge(t *testing.T, seen *[]classifierBridgeRequest) *httptest.Server {
	t.Helper()
	record := func(route string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*seen = append(*seen, classifierBridgeRequest{
				route:      route,
				id:         r.PathValue("id"),
				requestURI: r.RequestURI,
				query:      r.URL.RawQuery,
			})
			switch route {
			case "GET /sessions/{id}":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, aClassifierSessionBody)
			case "GET /sessions/{id}/messages":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, aClassifierTurnPage)
			case "GET /sessions/{id}/attach-token":
				http.Error(w, "session has no live pty hub", http.StatusNotFound)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
		}
	}
	mux := http.NewServeMux()
	for _, route := range []string{
		"GET /sessions/{id}",
		"GET /sessions/{id}/messages",
		"GET /sessions/{id}/attach-token",
		"POST /sessions/{id}/send",
	} {
		mux.HandleFunc(route, record(route))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, classifierBridgeRequest{
			route:      "unrouted " + r.URL.Path,
			requestURI: r.RequestURI,
			query:      r.URL.RawQuery,
		})
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The expected request targets are written out as literals rather than built
// with url.PathEscape: a fixture that computes its expectation the way the code
// does agrees with the code however wrong the code is.
var classifierSessionIDCases = []struct {
	sessionID       string
	sessionTarget   string
	messagesTarget  string
	reachedIfUnsent string
}{
	{"br_1790000000", "/sessions/br_1790000000", "/sessions/br_1790000000/messages?limit=15&payload=preview", "nothing: an ordinary id must pass through unchanged"},
	{"s-1/messages", "/sessions/s-1%2Fmessages", "/sessions/s-1%2Fmessages/messages?limit=15&payload=preview", "the messages route of session s-1"},
	{"s-1/attach-token", "/sessions/s-1%2Fattach-token", "/sessions/s-1%2Fattach-token/messages?limit=15&payload=preview", "a 404 for a session that exists"},
	{"s-1?state=idle", "/sessions/s-1%3Fstate=idle", "/sessions/s-1%3Fstate=idle/messages?limit=15&payload=preview", "session s-1, cut short by the query separator"},
}

func TestGetSessionAddressesTheSessionItsIDNames(t *testing.T) {
	for _, tc := range classifierSessionIDCases {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []classifierBridgeRequest
			srv := stubClassifierBridge(t, &seen)

			s, err := getSession(srv.URL, tc.sessionID)

			if len(seen) != 1 {
				t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
			}
			if seen[0].requestURI != tc.sessionTarget {
				t.Errorf("request target = %q, want %q — unescaped, the id reaches %s",
					seen[0].requestURI, tc.sessionTarget, tc.reachedIfUnsent)
			}
			if seen[0].route != "GET /sessions/{id}" {
				t.Errorf("id %q reached route %q, not the session route", tc.sessionID, seen[0].route)
			}
			if seen[0].id != tc.sessionID {
				t.Errorf("the bridge was asked about session %q, but the classifier asked for %q", seen[0].id, tc.sessionID)
			}
			if err != nil {
				t.Fatalf("reading the session: %v", err)
			}
			if s.State != "idle" {
				t.Errorf("state = %q, want %q from the session route", s.State, "idle")
			}
		})
	}
}

// The page URL carries a query string after the subresource, so a revert has
// two places to go wrong: the id segment, and the limit/payload parameters that
// keep the read bounded (see bounded_fetch_test.go for why they matter).
func TestGetSessionPageAddressesTheMessagesOfTheSessionItsIDNames(t *testing.T) {
	for _, tc := range classifierSessionIDCases {
		t.Run(tc.sessionID, func(t *testing.T) {
			var seen []classifierBridgeRequest
			srv := stubClassifierBridge(t, &seen)

			page, err := getSessionPage(srv.URL, tc.sessionID)

			if len(seen) != 1 {
				t.Fatalf("expected exactly 1 bridge request, got %d: %+v", len(seen), seen)
			}
			if seen[0].requestURI != tc.messagesTarget {
				t.Errorf("request target = %q, want %q — unescaped, the id reaches %s",
					seen[0].requestURI, tc.messagesTarget, tc.reachedIfUnsent)
			}
			if seen[0].route != "GET /sessions/{id}/messages" {
				t.Errorf("id %q reached route %q, not the messages route", tc.sessionID, seen[0].route)
			}
			if seen[0].id != tc.sessionID {
				t.Errorf("the bridge was asked for the messages of %q, but the classifier asked for %q", seen[0].id, tc.sessionID)
			}
			if seen[0].query != "limit=15&payload=preview" {
				t.Errorf("query = %q, want the bounded page %q", seen[0].query, "limit=15&payload=preview")
			}
			if err != nil {
				t.Fatalf("reading the page: %v", err)
			}
			if page.eventCount != 7 {
				t.Errorf("eventCount = %d, want 7 from the messages route", page.eventCount)
			}
		})
	}
}

// The ids no escaping can turn into a session name are refused before any
// request is sent. A revert to interpolation sends them anyway.
func TestBothSessionReadsRefuseAnIDThatNamesNoSession(t *testing.T) {
	for _, sessionID := range []string{"", ".", ".."} {
		t.Run(fmt.Sprintf("%q", sessionID), func(t *testing.T) {
			var seen []classifierBridgeRequest
			srv := stubClassifierBridge(t, &seen)

			if _, err := getSession(srv.URL, sessionID); err == nil {
				t.Errorf("getSession(%q) returned no error", sessionID)
			}
			if _, err := getSessionPage(srv.URL, sessionID); err == nil {
				t.Errorf("getSessionPage(%q) returned no error", sessionID)
			}
			if len(seen) != 0 {
				t.Errorf("sent %d request(s) for an id that names no session: %+v", len(seen), seen)
			}
		})
	}
}
