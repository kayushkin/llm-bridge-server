package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// bridgeRequest is one request the stub bridge saw: which of its routes matched,
// what the router pulled out as {id}, and the method. A stub that answers every
// path alike cannot record any of this, and a path typo then looks exactly like
// a finished session.
type bridgeRequest struct {
	route  string
	id     string
	method string
}

// stubBridge answers like llm-bridge-server: Go 1.22 patterns, one wildcard
// segment for the session id, and the neighbouring routes a session ref
// carrying a slash would otherwise reach. Every one of those neighbours is a
// real route on the running server, and each was measured 2026-08-14:
//
//	GET  /sessions/{id}/send          405 — registered POST-only
//	GET  /sessions/{id}/messages      200, an object {"model","stream"} (since llm-bridge-server db4265d)
//	GET  /sessions/{id}/attach-token  404, for a session that EXISTS and may be running
//
// The last one is why this stub has to route. A 404 is the single reply that
// frees the card, so a ref that lands on attach-token releases a card an agent
// is holding — the exact failure fetchSessionState's comment says must not
// happen. Against a stub that 404s every path, that bug reads as correct.
func stubBridge(t *testing.T, seen *[]bridgeRequest, body func() string) *httptest.Server {
	t.Helper()
	record := func(route string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			*seen = append(*seen, bridgeRequest{route: route, id: r.PathValue("id"), method: r.Method})
			switch route {
			case "GET /sessions/{id}":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body())
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
		*seen = append(*seen, bridgeRequest{route: "unrouted " + r.URL.Path, method: r.Method})
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gateAgainst builds a gate pointed at a stub bridge that answers the session
// route with the given state and updated_at, and freezes "now" so the dormancy
// window is testable without sleeping.
func gateAgainst(t *testing.T, state string, updatedAt time.Time) *sessionGate {
	t.Helper()
	var seen []bridgeRequest
	return gateAgainstRecording(t, state, updatedAt, &seen)
}

func gateAgainstRecording(t *testing.T, state string, updatedAt time.Time, seen *[]bridgeRequest) *sessionGate {
	t.Helper()
	srv := stubBridge(t, seen, func() string {
		return fmt.Sprintf(`{"state":%q,"updated_at":%q}`, state, updatedAt.Format(time.RFC3339))
	})

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

// ==================== the session ref reaches the right route ====================

// The sharpest case, because both the old code and the new one get a 200 here
// and only the id differs. A ref carrying "?" is cut short by the query
// separator, so /sessions/s-1?state=completed asks about "s-1" — a different
// session — and reports its state as this card's. Nothing logs it.
func TestTheGateAsksAboutTheWholeRefAndNotThePrefixBeforeAQuestionMark(t *testing.T) {
	const ref = "s-1?state=completed"
	var seen []bridgeRequest
	g := gateAgainstRecording(t, "tool_running", g0, &seen)

	if owned, _ := g.agentOwns(cardWithSession(ref)); !owned {
		t.Fatal("the session is tool_running, so the card must be held")
	}
	if len(seen) != 1 {
		t.Fatalf("want one request, got %d: %+v", len(seen), seen)
	}
	if seen[0].id != ref {
		t.Fatalf("the bridge was asked about %q, not the ref %q on the card", seen[0].id, ref)
	}
}

// A ref containing a slash names a neighbouring route. attach-token is the one
// that matters: it 404s for a session that exists, and a 404 frees the card.
func TestASessionRefThatNamesANeighbouringRouteDoesNotReachIt(t *testing.T) {
	for _, ref := range []string{"s-1/send", "s-1/messages", "s-1/attach-token", "s-1/git"} {
		t.Run(ref, func(t *testing.T) {
			var seen []bridgeRequest
			g := gateAgainstRecording(t, "tool_running", g0, &seen)

			owned, _ := g.agentOwns(cardWithSession(ref))
			if len(seen) != 1 {
				t.Fatalf("want one request, got %d: %+v", len(seen), seen)
			}
			if seen[0].route != "GET /sessions/{id}" {
				t.Fatalf("ref %q reached %q instead of the session route", ref, seen[0].route)
			}
			if seen[0].id != ref {
				t.Fatalf("ref %q arrived as %q", ref, seen[0].id)
			}
			if !owned {
				t.Fatalf("ref %q released a card whose session is tool_running", ref)
			}
		})
	}
}

// The gate reads state, so it must read it. A write method against the real
// bridge would reach a different route or none, and the reply to the wrong
// route is a well-formed reply.
func TestTheGateReadsTheSessionWithGET(t *testing.T) {
	var seen []bridgeRequest
	g := gateAgainstRecording(t, "idle", g0, &seen)

	g.agentOwns(cardWithSession("s-1"))
	if len(seen) != 1 || seen[0].method != http.MethodGet {
		t.Fatalf("want a single GET, got %+v", seen)
	}
}

// A dot-segment is resolved by the router before any route matches, so no
// escaping can make it name a session: /sessions/.. redirects to "/", Go's
// client follows, and the 404 there frees the card. The gate must hold instead,
// which is what it already does for a bridge it cannot reach.
func TestAnUnaddressableSessionRefHoldsTheCardAndIsNeverSent(t *testing.T) {
	for _, ref := range []string{".", ".."} {
		t.Run(ref, func(t *testing.T) {
			var seen []bridgeRequest
			g := gateAgainstRecording(t, "completed", g0, &seen)

			if owned, _ := g.agentOwns(cardWithSession(ref)); !owned {
				t.Fatalf("ref %q must fail safe and hold the card", ref)
			}
			if len(seen) != 0 {
				t.Fatalf("ref %q was sent to the bridge anyway: %+v", ref, seen)
			}
		})
	}
}

var g0 = time.Date(2026, 8, 13, 11, 59, 0, 0, time.UTC)
