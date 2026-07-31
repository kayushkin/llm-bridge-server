package harness

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// applyExternalState is how the turn-end signal classifier writes a verdict
// that arrived after the turn's own events. The bound on it is the whole
// safety story: an advisory pass must never overwrite a state the raw event
// stream has since moved past.

func TestApplyExternalStatePromotesASettledTurn(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "All set."}})
	if got := d.currentState(); got != msg.SessionIdle {
		t.Fatalf("state after a plain turn-end = %q, want idle", got)
	}

	ev := d.applyExternalState(msg.SessionAwaitingUser, "turn_complete_signal_question", msg.SessionIdle, msg.SessionAwaitingUser)
	if ev == nil {
		t.Fatal("no transition event; the classifier's promotion was dropped")
	}
	if ev.State.State != msg.SessionAwaitingUser || ev.State.Previous != msg.SessionIdle {
		t.Errorf("transition = %q → %q, want idle → awaiting_user", ev.State.Previous, ev.State.State)
	}
	if got := d.currentState(); got != msg.SessionAwaitingUser {
		t.Errorf("state = %q, want awaiting_user", got)
	}
}

func TestApplyExternalStateDemotesAHeuristicFalsePositive(t *testing.T) {
	d := newDerivationState()
	// "does that make sense?" ends in a question mark, so the heuristic parks
	// the session even though nothing was asked.
	d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "I renamed the field. Does that make sense?"}})
	if got := d.currentState(); got != msg.SessionAwaitingUser {
		t.Fatalf("state = %q, want the heuristic to have parked it at awaiting_user", got)
	}

	if ev := d.applyExternalState(msg.SessionIdle, "turn_complete_signal_neither", msg.SessionAwaitingUser); ev == nil {
		t.Fatal("no transition event; the classifier could not correct the heuristic")
	}
	if got := d.currentState(); got != msg.SessionIdle {
		t.Errorf("state = %q, want idle", got)
	}
}

func TestApplyExternalStateIsDroppedOnceANewTurnHasOpened(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "All set."}})
	// The user replied before the classifier answered.
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	if got := d.currentState(); got != msg.SessionToolRunning {
		t.Fatalf("state = %q, want tool_running", got)
	}

	ev := d.applyExternalState(msg.SessionAwaitingUser, "turn_complete_signal_question", msg.SessionIdle, msg.SessionAwaitingUser)
	if ev != nil {
		t.Fatalf("late verdict flipped a running turn to %q", ev.State.State)
	}
	if got := d.currentState(); got != msg.SessionToolRunning {
		t.Errorf("state = %q, want the running turn left alone", got)
	}
}

func TestApplyExternalStateWithoutABoundWritesNothing(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "All set."}})

	// A caller that forgets allowedFrom gets no write, not an unbounded one:
	// the failure mode of an unbounded default is silent state corruption.
	if ev := d.applyExternalState(msg.SessionAwaitingUser, "no bound"); ev != nil {
		t.Fatal("an unbounded external write was accepted")
	}
	if got := d.currentState(); got != msg.SessionIdle {
		t.Errorf("state = %q, want idle", got)
	}
}

func TestApplyExternalStateEmitsNothingWhenTheStateAlreadyMatches(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "Which branch should I use?"}})
	if got := d.currentState(); got != msg.SessionAwaitingUser {
		t.Fatalf("state = %q, want awaiting_user", got)
	}

	// The heuristic and the classifier agreed. Re-announcing the state would
	// put a no-op transition on every subscriber's stream.
	if ev := d.applyExternalState(msg.SessionAwaitingUser, "turn_complete_signal_question", msg.SessionIdle, msg.SessionAwaitingUser); ev != nil {
		t.Errorf("emitted a no-op transition %q → %q", ev.State.Previous, ev.State.State)
	}
}
