package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

func entityTags(tags ...string) []kanbanclient.EntityTag {
	out := make([]kanbanclient.EntityTag, 0, len(tags))
	for _, t := range tags {
		out = append(out, kanbanclient.EntityTag{EntityType: entityTypeSession, EntityRef: "br_test", Tag: t})
	}
	return out
}

// sessionServer serves one canned session document at /sessions/{id}.
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

// TestCreatedByAutoworkerIgnoresTheSessionID is the regression pin for the
// origin gate. It used to read strings.HasPrefix(sessionID, "autoworker-").
// Measured 2026-08-08 on the live bridge: 613 of 4,988 sessions with
// purpose="autoworker" carry a "br_"-prefixed id, so that gate silently classed
// 12.3% of the autoworker population as somebody else's session.
//
// The first case below is exactly that population. Reintroducing a prefix check
// inside createdByAutoworker turns it red.
func TestCreatedByAutoworkerIgnoresTheSessionID(t *testing.T) {
	cases := []struct {
		name string
		sess bridgeSession
		want bool
	}{
		{
			// The 613. Real shape, taken from a live session document.
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
			// The converse trap: an id that LOOKS like an autoworker session but
			// which the bridge says something else created. The old gate admitted
			// this; the purpose gate must not.
			name: "autoworker- prefix on the id but the bridge says otherwise",
			sess: bridgeSession{SessionID: "autoworker-lookalike-1", Purpose: "chat"},
			want: false,
		},
		{
			name: "a scheduled-task session is not an autoworker session",
			sess: bridgeSession{SessionID: "scheduler-cl-1", Purpose: "scheduled-task"},
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
		`{"session_id":"br_1785764840313992417","state":"completed","updated_at":"2026-08-08T01:02:03Z","purpose":"autoworker"}`)

	sess, err := getSession(srv.URL, "br_1785764840313992417")
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if sess.Purpose != "autoworker" {
		t.Errorf("Purpose = %q, want autoworker", sess.Purpose)
	}
	if sess.State != "completed" {
		t.Errorf("State = %q, want completed", sess.State)
	}
	if sess.SessionID != "br_1785764840313992417" {
		t.Errorf("SessionID = %q", sess.SessionID)
	}
	if !createdByAutoworker(sess) {
		t.Error("a completed purpose=autoworker session must pass the origin gate")
	}
}

// TestGetSessionRefusesASessionWithNoPurpose pins the fail-loud choice. An
// empty purpose compares unequal to purposeAutoworker, so returning it would
// read downstream as "somebody else's session" — indistinguishable from a real
// negative, which is the failure mode the purpose gate replaced.
func TestGetSessionRefusesASessionWithNoPurpose(t *testing.T) {
	srv := sessionServer(t, http.StatusOK, `{"session_id":"br_2","state":"completed"}`)

	_, err := getSession(srv.URL, "br_2")
	if err == nil {
		t.Fatal("want an error for a session with no purpose field, got nil")
	}
	if !strings.Contains(err.Error(), "no purpose") {
		t.Errorf("error should name the missing field, got %q", err)
	}
}

func TestGetSessionReportsAMissingSession(t *testing.T) {
	srv := sessionServer(t, http.StatusNotFound, `not found`)
	if _, err := getSession(srv.URL, "br_3"); err == nil {
		t.Fatal("want an error for a 404, got nil")
	}
}

func TestGetSessionReportsAServerError(t *testing.T) {
	srv := sessionServer(t, http.StatusInternalServerError, `boom`)
	_, err := getSession(srv.URL, "br_4")
	if err == nil {
		t.Fatal("want an error for a 500, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the server's body, got %q", err)
	}
}

// TestSkipCountsBreakdownNamesEveryReason is the pin for the accounting half of
// this change. A single "skipped" total is what let the binary log
// "skipped=139 errors=0" every five minutes for eleven days while its origin
// gate rejected every card on the board: that line reads identically whether
// the board is full of attended cards or the gate is broken.
func TestSkipCountsBreakdownNamesEveryReason(t *testing.T) {
	c := skipCounts{noItem: 1, noSessionLink: 2, notAutoworker: 3, notCompleted: 4, alreadySettled: 5}
	if got, want := c.total(), 15; got != want {
		t.Fatalf("total() = %d, want %d", got, want)
	}
	got := c.breakdown()
	for _, want := range []string{"no-item=1", "no-session-link=2", "not-autoworker=3", "not-completed=4", "already-settled=5"} {
		if !strings.Contains(got, want) {
			t.Errorf("breakdown() = %q, missing %q", got, want)
		}
	}
}

// Every field must reach the breakdown on its own. A single populated struct
// can pass the test above while one reason is silently never rendered.
func TestSkipCountsBreakdownRendersEachReasonAlone(t *testing.T) {
	cases := []struct {
		set  func(*skipCounts)
		want string
	}{
		{func(c *skipCounts) { c.noItem = 1 }, "no-item=1"},
		{func(c *skipCounts) { c.noSessionLink = 1 }, "no-session-link=1"},
		{func(c *skipCounts) { c.notAutoworker = 1 }, "not-autoworker=1"},
		{func(c *skipCounts) { c.notCompleted = 1 }, "not-completed=1"},
		{func(c *skipCounts) { c.alreadySettled = 1 }, "already-settled=1"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			var c skipCounts
			tc.set(&c)
			if c.total() != 1 {
				t.Fatalf("total() = %d, want 1 — a reason that does not reach total() is invisible", c.total())
			}
			if got := c.breakdown(); got != tc.want {
				t.Fatalf("breakdown() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkipCountsBreakdownOfNothingIsNotEmpty(t *testing.T) {
	var c skipCounts
	if got := c.breakdown(); got != "none" {
		t.Fatalf("breakdown() = %q, want %q — an empty parenthesis in the tick line reads as a truncated log", got, "none")
	}
}

// passCount counts review-pass tags rather than parsing the N out of them, so
// the number in the tag is decorative. Pin that, and pin that the prefix does
// not swallow the neighboring review-settled tag.
func TestPassCountCountsTagsAndIgnoresTheNumber(t *testing.T) {
	tags := entityTags("review-pass-1", "review-pass-7", "review-settled", "kanban-do-not-track")
	if got, want := passCount(tags), 2; got != want {
		t.Fatalf("passCount = %d, want %d", got, want)
	}
	if got := passCount(entityTags("review-settled")); got != 0 {
		t.Fatalf("review-settled must not match the review-pass- prefix, got %d", got)
	}
}
