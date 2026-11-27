package main

// The two reader cases from 622572ea's protocol suite that main did not
// already have. main_test.go pins the 200, the 404 and the 500, and
// session_url_test.go pins the verb and the root-mounted path; these pin every
// other failure and the partial read.

import (
	"net/http"
	"testing"
)

// notFound means "this session is gone for good, reap the card to Failed".
// Every other failure means "I could not tell, look again next tick". Folding
// any of these into notFound would move every In Progress card to Failed while
// the sessions behind them were still running — one bad deploy of
// llm-bridge-server, or one expired service token, would do it. The 401 is not
// hypothetical: the bridge answers a request without its token with 401
// (measured 2026-09-25 against :8160).
func TestGetSessionReportsNoFailureButA404AsNotFound(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := sessionServer(t, status, `{"error":"upstream trouble"}`)
			_, notFound, err := getSession(srv.URL, "br_1")
			if notFound {
				t.Errorf("HTTP %d was reported as notFound; the curator would reap a "+
					"card whose session is very likely still alive", status)
			}
			if err == nil {
				t.Errorf("HTTP %d was read as success", status)
			}
		})
	}
}

// staleSince treats an empty timestamp as not stale on purpose, "so a card is
// never reaped on the basis of a missing timestamp alone". A getSession that
// refused a session without updated_at would turn that tolerance into an error
// and skip the card on every tick instead.
func TestGetSessionReadsASessionWithNoUpdatedAt(t *testing.T) {
	srv := sessionServer(t, http.StatusOK, `{"session_id":"br_1","state":"completed","purpose":"autoworker"}`)
	sess, notFound, err := getSession(srv.URL, "br_1")
	if err != nil {
		t.Fatalf("a session with no updated_at is readable, not an error: %v", err)
	}
	if notFound {
		t.Error("reported notFound for a session the bridge answered 200 for")
	}
	if sess.State != "completed" || sess.UpdatedAt != "" {
		t.Errorf("state/updated_at = %q/%q, want completed and empty", sess.State, sess.UpdatedAt)
	}
}
