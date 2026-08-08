package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sessionServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/sessions/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCreatedByAutoworkerIgnoresTheSessionID is the regression pin for both
// autoworker gates in this binary — the reviewer handshake that holds a
// completed card out of Done, and the reap scope that moves an abandoned
// waiter to Failed. Both used to read
// strings.HasPrefix(sessionID, "autoworker-").
//
// Measured 2026-08-08 on the live bridge: 613 of 4,988 purpose="autoworker"
// sessions carry a "br_"-prefixed id. Under the old gate every one of them
// closed to Done with no review pass, and none could ever be reaped out of a
// wedged wait state. Reintroducing a prefix check here turns this red.
func TestCreatedByAutoworkerIgnoresTheSessionID(t *testing.T) {
	cases := []struct {
		name string
		sess bridgeSession
		want bool
	}{
		{
			name: "purpose autoworker but no autoworker- prefix on the id",
			sess: bridgeSession{SessionID: "br_1785764840313992417", Purpose: "autoworker"},
			want: true,
		},
		{
			name: "purpose autoworker and the id agrees",
			sess: bridgeSession{SessionID: "autoworker-anthropic-1786176000535089039", Purpose: "autoworker"},
			want: true,
		},
		{
			name: "autoworker- prefix on the id but the bridge says otherwise",
			sess: bridgeSession{SessionID: "autoworker-lookalike-1", Purpose: "chat"},
			want: false,
		},
		{
			// The dispatcher parks these idle on purpose to revive later. Reaping
			// one pulls a live session out from under it, which is why the reap
			// branch is scoped at all.
			name: "a dispatcher session must stay out of the reap scope",
			sess: bridgeSession{SessionID: "br_9", Purpose: "kanban-dispatcher"},
			want: false,
		},
		{
			name: "empty purpose is never an autoworker session",
			sess: bridgeSession{SessionID: "br_1", Purpose: ""},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createdByAutoworker(tc.sess); got != tc.want {
				t.Fatalf("createdByAutoworker(%+v) = %v, want %v", tc.sess, got, tc.want)
			}
		})
	}
}

func TestGetSessionDecodesPurpose(t *testing.T) {
	srv := sessionServer(t, http.StatusOK,
		`{"session_id":"br_1785764840313992417","state":"idle","updated_at":"2026-08-08T01:02:03Z","purpose":"autoworker"}`)

	sess, notFound, err := getSession(srv.URL, "br_1785764840313992417")
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if notFound {
		t.Fatal("notFound must be false for a 200")
	}
	if sess.Purpose != "autoworker" {
		t.Errorf("Purpose = %q, want autoworker", sess.Purpose)
	}
	if sess.State != "idle" || sess.UpdatedAt != "2026-08-08T01:02:03Z" {
		t.Errorf("state/updated_at not carried through: %+v", sess)
	}
	if !createdByAutoworker(sess) {
		t.Error("this session must be inside the reap scope")
	}
}

// A 404 is a reapable terminal condition, not a transient error. The caller
// distinguishes them by the notFound flag, so it has to survive.
func TestGetSessionReportsAMissingSessionAsNotFound(t *testing.T) {
	srv := sessionServer(t, http.StatusNotFound, `not found`)
	_, notFound, err := getSession(srv.URL, "br_gone")
	if !notFound {
		t.Fatal("notFound must be true for a 404 — the caller reaps the card on it")
	}
	if err == nil {
		t.Fatal("want an error alongside notFound")
	}
}

// A 500 must NOT read as notFound: the caller would reap a live card to Failed
// on a transient bridge outage.
func TestGetSessionDoesNotReportAServerErrorAsNotFound(t *testing.T) {
	srv := sessionServer(t, http.StatusInternalServerError, `boom`)
	_, notFound, err := getSession(srv.URL, "br_5")
	if notFound {
		t.Fatal("a 500 must not set notFound — that would reap live cards during an outage")
	}
	if err == nil {
		t.Fatal("want an error for a 500")
	}
}

func TestGetSessionRefusesASessionWithNoPurpose(t *testing.T) {
	srv := sessionServer(t, http.StatusOK, `{"session_id":"br_2","state":"completed"}`)
	_, notFound, err := getSession(srv.URL, "br_2")
	if err == nil {
		t.Fatal("want an error for a session with no purpose field, got nil")
	}
	if notFound {
		t.Fatal("a purposeless session is not a missing session — reaping it would be wrong")
	}
	if !strings.Contains(err.Error(), "no purpose") {
		t.Errorf("error should name the missing field, got %q", err)
	}
}

func TestStaleSinceTreatsAnUnparseableTimestampAsNotStale(t *testing.T) {
	if staleSince("", time.Minute) {
		t.Error("an empty timestamp must not make a card reapable")
	}
	if staleSince("not-a-time", time.Minute) {
		t.Error("an unparseable timestamp must not make a card reapable")
	}
	if !staleSince("2020-01-01T00:00:00Z", time.Minute) {
		t.Error("a genuinely old timestamp is stale")
	}
}
