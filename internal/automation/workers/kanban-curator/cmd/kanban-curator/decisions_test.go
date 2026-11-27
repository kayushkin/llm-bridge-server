package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// These tests pin what the curator DECIDES: which column a card goes to, which
// cards it leaves alone, and what it writes on the card when it closes one.
// degraded_run_test.go pins whether a tick can say it fell short; a curator that
// moved every card to the wrong column would pass all of that.
//
// Ported 2026-09-25 from test/first-tests-for-curator-and-dispatcher (6767efd1,
// c2e7390b, 86b6dc8f) onto main's stubs. Each test here was added because
// scripts/sabotage-kanban-curator.py found a mutation main's suite let through.

// ==================== the wire values, spelled out ====================
//
// The stubs build their board, links and tags from the same constants the
// curator reads, so a constant that drifts drifts in both places and every test
// stays green. The live rows do not drift with it: they were written by
// kanban-store's board setup, autoworker, the reviewer and llm-bridge-server.
// So this one test spells each value the way those programs write it.

func TestTheWireValuesAreTheOnesTheOtherProgramsWrite(t *testing.T) {
	for _, check := range []struct{ name, got, want string }{
		{"columnInProgress", columnInProgress, "In Progress"},
		{"columnDone", columnDone, "Done"},
		{"columnFailed", columnFailed, "Failed"},
		{"linkTypeSession", linkTypeSession, "session"},
		{"entityTypeSession", entityTypeSession, "session"},
		{"tagReviewSettled", tagReviewSettled, "review-settled"},
		{"purposeAutoworker", purposeAutoworker, "autoworker"},
	} {
		if check.got != check.want {
			t.Errorf("%s = %q, want %q: the live rows carry %q, and a curator reading anything else matches nothing and logs a quiet tick", check.name, check.got, check.want, check.want)
		}
	}
}

// ==================== where a card goes ====================

func TestEveryStoppedStateClosesToFailed(t *testing.T) {
	for _, state := range []string{"error", "aborted", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
			b := newStubBridge(t)
			b.states["s1"] = state

			captureLog(t, func() { run(testConfig(k, b)) })

			if k.moved["card-1"] != stubColFailed {
				t.Errorf("a %s session's card went to %q, want Failed", state, k.moved["card-1"])
			}
		})
	}
}

func TestARunningSessionIsLeftInProgress(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "running"

	captureLog(t, func() { run(testConfig(k, b)) })

	if moved, ok := k.moved["card-1"]; ok {
		t.Errorf("a running session's card was moved to %q", moved)
	}
}

func TestAClosedCardsNoteNamesTheStateItsSessionEndedIn(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", body: "original", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "completed"

	captureLog(t, func() { run(testConfig(k, b)) })

	if !strings.Contains(k.patched["card-1"], "bridge state=completed") {
		t.Errorf("the closing note does not say what state the session ended in: %q", k.patched["card-1"])
	}
}

// ==================== reaping a waiter ====================
//
// The reaping rules move cards to Failed on the curator's own initiative, so a
// gate that widens does not go quiet: it closes work that was still going. The
// dispatcher parks its own sessions idle on purpose to revive them later, which
// is why only autoworker's waiters are ever reaped.

func TestAnAbandonedAutoworkerWaiterIsReapedAndTheNoteSaysWhy(t *testing.T) {
	for _, state := range []string{"idle", "awaiting_permission", "awaiting_user"} {
		t.Run(state, func(t *testing.T) {
			k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", body: "original", sessionID: "s1"})
			b := newStubBridge(t)
			b.states["s1"] = state
			b.purposes["s1"] = purposeAutoworker
			b.updatedAt["s1"] = minutesAgo(3 * 60)

			captureLog(t, func() { run(testConfig(k, b)) })

			if k.moved["card-1"] != stubColFailed {
				t.Fatalf("an autoworker session %s for 3h was not reaped: moved to %q", state, k.moved["card-1"])
			}
			if !strings.Contains(k.patched["card-1"], "reaped: "+state) {
				t.Errorf("the reap note does not say why the card was reaped: %q", k.patched["card-1"])
			}
		})
	}
}

func TestAFreshAutoworkerWaiterIsLeftAlone(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "idle"
	b.purposes["s1"] = purposeAutoworker
	b.updatedAt["s1"] = minutesAgo(5)

	captureLog(t, func() { run(testConfig(k, b)) })

	if moved, ok := k.moved["card-1"]; ok {
		t.Errorf("a waiter idle for 5 minutes was reaped to %q; the stale window is 2h", moved)
	}
}

func TestAnIdleSessionThatAutoworkerDidNotCreateIsNeverReaped(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "idle" // purpose left to the stub's "interactive"
	b.updatedAt["s1"] = minutesAgo(3 * 60)

	captureLog(t, func() { run(testConfig(k, b)) })

	if moved, ok := k.moved["card-1"]; ok {
		t.Errorf("an idle session another program owns was reaped to %q", moved)
	}
}

// ==================== the reviewer's sign-off ====================

func TestATagThatIsNotTheSignOffDoesNotReleaseTheCard(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	k.otherTags = map[string][]string{"s1": {"reviewed-once", "needs-rework"}}
	b := newStubBridge(t)
	b.states["s1"] = "completed"
	b.purposes["s1"] = purposeAutoworker
	b.updatedAt["s1"] = minutesAgo(1)

	captureLog(t, func() { run(testConfig(k, b)) })

	if moved, ok := k.moved["card-1"]; ok {
		t.Errorf("a card with no review-settled tag was closed to %q", moved)
	}
}

// staleCompleted is the fail-safe that closes a card the reviewer never signed
// off. A timestamp it cannot read must keep the card waiting: closing on it
// would skip the review exactly when the bridge sent something odd. The stub
// bridge always sends a timestamp, so this is asked of the function directly.
func TestACompletedSessionWithNoUsableTimestampStillWaitsForTheReviewer(t *testing.T) {
	for _, updatedAt := range []string{"", "yesterday", "2026-09-25 10:00:00"} {
		if staleCompleted(updatedAt) {
			t.Errorf("staleCompleted(%q) = true, want false: an unreadable timestamp must not skip the review", updatedAt)
		}
	}
	if !staleCompleted(time.Now().Add(-reviewGrace - time.Minute).UTC().Format(time.RFC3339)) {
		t.Errorf("staleCompleted is false past the grace window; the check above proves nothing if it is always false")
	}
}

// ==================== the pinned board ====================

func TestABoardThatNoLongerExistsIsRefusedNotReplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/boards/board-gone" {
			t.Errorf("unexpected %s %s: a missing board must not be created or searched for", r.Method, r.URL.Path)
		}
		http.Error(w, "no such board", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	board, err := getPinnedBoard(kanbanclient.New(srv.URL, ""), "board-gone")
	if err == nil {
		t.Fatalf("getPinnedBoard returned board %+v for an id kanban-store does not have", board)
	}
	if !strings.Contains(err.Error(), "board-gone") {
		t.Errorf("the error does not name the board id: %v", err)
	}
}

// The curator passes position 0 on every move, which files the card it just
// closed at the top of Done or Failed: the most recently closed card is the one
// worth seeing first, and those columns run to hundreds of cards. The position
// rides on the same request as the column id, and a test that reads only the
// column would pass with the card filed anywhere. Ported from 63a7f704.
func TestAClosedCardIsFiledAtTheTopOfItsColumn(t *testing.T) {
	k := newStubKanban(t, "Agent runs", stubCard{cardID: "card-1", title: "a dispatch", sessionID: "s1"})
	b := newStubBridge(t)
	b.states["s1"] = "completed"

	captureLog(t, func() { run(testConfig(k, b)) })

	position, moved := k.movedPos["card-1"]
	if !moved {
		t.Fatal("the card was never moved, so there is no position to check")
	}
	if position != 0 {
		t.Errorf("filed at position %v, want 0: a closed card belongs at the top of its column", position)
	}
}
