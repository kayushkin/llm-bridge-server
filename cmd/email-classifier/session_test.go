package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// gateAgainst builds a gate pointed at a stub bridge that answers every session
// with the given state and updated_at, and freezes "now" so the dormancy window
// is testable without sleeping.
func gateAgainst(t *testing.T, state string, updatedAt time.Time) *sessionGate {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"state":%q,"updated_at":%q}`, state, updatedAt.Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)

	g := newSessionGate(srv.URL)
	g.now = func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }
	return g
}

func cardWithSession(sessionID string) *cardState {
	return &cardState{
		ID:    "card-1",
		Title: "A card",
		AllLinks: []kanbanclient.CardLink{
			{EntityType: linkTypeSession, EntityRef: sessionID},
		},
	}
}

// A card with no session link at all is always the classifier's to edit. This is
// the overwhelmingly common case and it must not cost a bridge call.
func TestCardWithoutSessionLinkIsNeverAgentOwned(t *testing.T) {
	g := newSessionGate("http://127.0.0.1:1") // would fail if ever dialled
	card := &cardState{
		ID: "card-1",
		AllLinks: []kanbanclient.CardLink{
			{EntityType: linkTypeEmail, EntityRef: "gmail-personal:abc"},
			{EntityType: linkTypeEmailSender, EntityRef: "sender@example.com"},
		},
	}
	if owned, _ := g.agentOwns(card); owned {
		t.Fatal("a card with only email links must not read as agent-owned")
	}
}

func TestWorkingStatesOwnTheCard(t *testing.T) {
	// Every state that means the harness is mid-turn. rate_limited is in here
	// deliberately: the agent is waiting on a quota, not finished.
	for _, state := range []string{
		"starting", "model_generating", "tool_running", "compacting", "running", "rate_limited",
	} {
		t.Run(state, func(t *testing.T) {
			g := gateAgainst(t, state, g0)
			if owned, _ := g.agentOwns(cardWithSession("s-1")); !owned {
				t.Fatalf("state %q must count as agent-owned", state)
			}
		})
	}
}

func TestTerminalStatesReleaseTheCard(t *testing.T) {
	// Mirrors kanban-curator's terminal list. If these did not release, a card
	// the curator has already moved to Done would stay frozen forever.
	for _, state := range []string{"completed", "error", "aborted", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			g := gateAgainst(t, state, g0)
			if owned, _ := g.agentOwns(cardWithSession("s-1")); owned {
				t.Fatalf("terminal state %q must release the card", state)
			}
		})
	}
}

// The dormancy window is the whole reason this gate does not deadlock the board:
// thousands of long-finished sessions sit in "idle", and an unbounded idle check
// would lock their cards away from the classifier permanently.
func TestDormantSessionOwnsTheCardOnlyInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	for _, state := range []string{"idle", "awaiting_permission", "awaiting_user", "paused"} {
		t.Run(state+"/fresh", func(t *testing.T) {
			g := gateAgainst(t, state, now.Add(-dormantSessionWindow/2))
			if owned, _ := g.agentOwns(cardWithSession("s-1")); !owned {
				t.Fatalf("%s inside the window must still own the card", state)
			}
		})
		t.Run(state+"/stale", func(t *testing.T) {
			g := gateAgainst(t, state, now.Add(-2*dormantSessionWindow))
			if owned, _ := g.agentOwns(cardWithSession("s-1")); owned {
				t.Fatalf("%s past the window must release the card", state)
			}
		})
	}
}

// A 404 and an unreachable bridge mean opposite things, and collapsing them is
// exactly the bug that would let the classifier edit live cards during an
// outage. These two tests pin the distinction.
func TestMissingSessionReleasesTheCard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	g := newSessionGate(srv.URL)
	if owned, _ := g.agentOwns(cardWithSession("s-gone")); owned {
		t.Fatal("a session the bridge has forgotten must release the card")
	}
}

func TestUnreachableBridgeHoldsTheCard(t *testing.T) {
	// Port 1 refuses instantly, standing in for a bridge that is down.
	g := newSessionGate("http://127.0.0.1:1")
	if owned, _ := g.agentOwns(cardWithSession("s-1")); !owned {
		t.Fatal("an unresolvable session must fail safe and hold the card")
	}
}

func TestUnknownStateHoldsTheCard(t *testing.T) {
	// A state this binary has never heard of is more likely a new working state
	// than a new terminal one, and guessing wrong costs an agent's work.
	g := gateAgainst(t, "reticulating_splines", g0)
	if owned, _ := g.agentOwns(cardWithSession("s-1")); !owned {
		t.Fatal("an unrecognised state must fail safe and hold the card")
	}
}

// The gate resolves each session once per tick. One board view routinely carries
// the same session on several cards, and re-asking the bridge per card would
// turn a merge pass into hundreds of HTTP calls.
func TestSessionStateIsResolvedOncePerTick(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"state":"tool_running","updated_at":""}`)
	}))
	defer srv.Close()

	g := newSessionGate(srv.URL)
	for i := 0; i < 5; i++ {
		if owned, _ := g.agentOwns(cardWithSession("s-shared")); !owned {
			t.Fatal("expected the card to be agent-owned")
		}
	}
	if calls != 1 {
		t.Fatalf("bridge calls: got %d, want 1 — the per-tick cache is not holding", calls)
	}
}

var g0 = time.Date(2026, 8, 13, 11, 59, 0, 0, time.UTC)
