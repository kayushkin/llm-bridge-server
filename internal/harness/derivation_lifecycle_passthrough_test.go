package harness

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The EventSessionState arm of derive() is a pass-through for
// manager-injected lifecycle signals — harness-emitted ones are dropped at
// intake (manager.readEvents), so the only events that reach it are ones
// the server itself chose to send. A state missing from that list is not
// rejected and not logged: next stays equal to prev, no transition is
// emitted, and the caller in deriveAndBroadcast never writes the session
// row. The signal disappears in silence.
//
// That is how the reopen half of mark-done was lost, so these tests assert
// the pass-through by state rather than by scenario.

// TestDerivationPassesThroughEveryStateMarkDoneCanSend covers the two
// states handleMarkSessionDone injects: completed for done=true, idle for
// done=false. Both must survive the arm.
func TestDerivationPassesThroughEveryStateMarkDoneCanSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start msg.SessionState
		send  msg.SessionState
	}{
		{"mark done from idle", msg.SessionIdle, msg.SessionCompleted},
		{"reopen from completed", msg.SessionCompleted, msg.SessionIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDerivationStateSeeded(tc.start)
			got := d.derive(&msg.Event{
				Type:  msg.EventSessionState,
				State: &msg.StateEvent{State: tc.send},
			})
			ev := firstSessionState(got)
			if ev == nil {
				t.Fatalf("injecting %q at %q derived no session_state event — the transition was dropped in silence",
					tc.send, tc.start)
			}
			if ev.State.State != tc.send {
				t.Fatalf("derived state = %q; want %q", ev.State.State, tc.send)
			}
			if ev.State.Previous != tc.start {
				t.Errorf("derived previous = %q; want %q", ev.State.Previous, tc.start)
			}
			if d.sessionState != tc.send {
				t.Errorf("derivation state = %q after the transition; want %q", d.sessionState, tc.send)
			}
		})
	}
}

// TestDerivationMarkDoneRoundTripReturnsToIdle drives both halves through
// one derivation, which is the shape the live handler produces: the second
// injection has to read the state the first one left behind.
func TestDerivationMarkDoneRoundTripReturnsToIdle(t *testing.T) {
	d := newDerivationState()

	if got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventSessionState, State: &msg.StateEvent{State: msg.SessionCompleted}},
		{Type: msg.EventSessionState, State: &msg.StateEvent{State: msg.SessionIdle}},
	}); len(got) != 2 || got[0] != msg.SessionCompleted || got[1] != msg.SessionIdle {
		t.Fatalf("mark-done round trip emitted %v; want [completed idle]", got)
	}
	if d.sessionState != msg.SessionIdle {
		t.Fatalf("state after round trip = %q; want idle", d.sessionState)
	}
}

// TestDerivationSuppressesALifecycleStateItIsAlreadyIn guards the other
// direction: adding idle to the pass-through must not make a redundant
// injection emit a no-op transition. Marking an already-archived session
// done again is exactly this case, and it happens on every double click.
func TestDerivationSuppressesALifecycleStateItIsAlreadyIn(t *testing.T) {
	d := newDerivationStateSeeded(msg.SessionIdle)
	if got := d.derive(&msg.Event{
		Type:  msg.EventSessionState,
		State: &msg.StateEvent{State: msg.SessionIdle},
	}); firstSessionState(got) != nil {
		t.Fatalf("idle injected at idle emitted a transition: %+v; want none", got)
	}
}
