package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// drainEvents collects StoredEvents from ch until it is closed (readEvents
// closes subscriber channels when the process's event channel closes) or the
// deadline elapses.
func drainEvents(t *testing.T, ch chan StoredEvent, timeout time.Duration) []StoredEvent {
	t.Helper()
	var got []StoredEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("drain timeout after %d events", len(got))
		}
	}
}

// TestManager_OTelEchoAttachesToOpenTurn is the Bug-3 regression. Claude Code
// reports every prompt twice — the harness/rollout copy and, ~1s later, an OTel
// echo tagged extensions.source=="otel". The manager used to mint a fresh
// turn_id for EVERY user_message, so the echo opened a second turn and the real
// work landed there, splitting one prompt into two rendered turns. The fix
// attaches an OTel echo of the currently-open prompt to that turn instead of
// opening a new one — while never dropping it (PTY mode has only the OTel copy).
func TestManager_OTelEchoAttachesToOpenTurn(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-otel-echo"
	const prompt = "do the thing"

	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 16)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	otel := map[string]json.RawMessage{"source": json.RawMessage(`"otel"`)}
	feed := []msg.Event{
		// Opening prompt via /send — no TurnID pre-stamped, so the manager mints one.
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
			Result: &msg.ResultEvent{Text: prompt}},
		// Real work lands in the open turn.
		{Type: msg.EventToolCall, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
			ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}},
		// OTel echo of the same prompt — must attach, not open a new turn.
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
			Extensions: otel, Result: &msg.ResultEvent{Text: prompt}},
		{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
			Result: &msg.ResultEvent{Text: "done"}},
	}
	for _, ev := range feed {
		proc.ch <- ev
	}
	close(proc.ch)

	got := drainEvents(t, sub, 3*time.Second)

	var userTurns []string
	turnIDs := map[string]struct{}{}
	for _, ev := range got {
		if ev.TurnID != "" {
			turnIDs[ev.TurnID] = struct{}{}
		}
		if ev.Type == msg.EventUserMessage {
			userTurns = append(userTurns, ev.TurnID)
		}
	}

	if len(userTurns) != 2 {
		t.Fatalf("expected 2 user_message events (prompt + otel echo); got %d", len(userTurns))
	}
	if userTurns[0] == "" {
		t.Fatalf("opening prompt got an empty turn_id")
	}
	if userTurns[0] != userTurns[1] {
		t.Fatalf("otel echo opened a new turn: prompt turn=%q, echo turn=%q", userTurns[0], userTurns[1])
	}
	if len(turnIDs) != 1 {
		t.Fatalf("one logical prompt spanned %d turn_ids %v; want exactly 1", len(turnIDs), keys(turnIDs))
	}
}

// TestManager_NonEchoUserMessageOpensNewTurn is the negative guard: an OTel
// user_message whose text does NOT match the open turn's prompt (a genuinely
// new prompt) must still open its own turn — the echo rule must not collapse
// distinct prompts.
func TestManager_NonEchoUserMessageOpensNewTurn(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-otel-distinct"

	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	first := &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "first prompt"}}
	m.AssignMessageID(bridgeID, first)

	// OTel-sourced, but a DIFFERENT prompt — not an echo of the open turn.
	second := &msg.Event{
		Type:            msg.EventUserMessage,
		BridgeSessionID: bridgeID,
		Extensions:      map[string]json.RawMessage{"source": json.RawMessage(`"otel"`)},
		Result:          &msg.ResultEvent{Text: "a different prompt"},
	}
	m.AssignMessageID(bridgeID, second)

	if second.TurnID == "" {
		t.Fatalf("distinct OTel prompt got an empty turn_id")
	}
	if second.TurnID == first.TurnID {
		t.Fatalf("distinct OTel prompt was wrongly attached to the open turn %q", first.TurnID)
	}
}

// TestManager_MidTurnSessionStatePreservesTurn is the Bug-4 regression that
// gates the deletion of the EventSessionState turn-clear in AssignMessageID.
// That case cleared turn tracking whenever state != "running" — firing on
// tool_running / awaiting_permission / idle and severing turn continuity
// mid-turn. The real turn boundary is owned by EventResult/EventError, so the
// clear was a redundant, fragile backstop. This proves that a mid-turn
// tool_running session_state passing through AssignMessageID leaves the open
// turn intact and a subsequent event keeps the same turn_id.
func TestManager_MidTurnSessionStatePreservesTurn(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-midturn-state"

	// Open a turn.
	userMsg := &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "hi"}}
	m.AssignMessageID(bridgeID, userMsg)
	openTurn := userMsg.TurnID
	if openTurn == "" {
		t.Fatalf("opening user_message got an empty turn_id")
	}

	// A mid-turn tool_running session_state must NOT clear the open turn.
	stateEv := &msg.Event{Type: msg.EventSessionState, BridgeSessionID: bridgeID, State: &msg.StateEvent{State: msg.SessionToolRunning}}
	m.AssignMessageID(bridgeID, stateEv)
	if stateEv.TurnID != openTurn {
		t.Fatalf("mid-turn session_state got turn_id %q; want the open turn %q", stateEv.TurnID, openTurn)
	}

	// A subsequent assistant event must still carry the same open turn.
	toolEv := &msg.Event{Type: msg.EventToolCall, BridgeSessionID: bridgeID, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}}
	m.AssignMessageID(bridgeID, toolEv)
	if toolEv.TurnID != openTurn {
		t.Fatalf("turn continuity broken across mid-turn session_state: tool_call turn=%q, want %q", toolEv.TurnID, openTurn)
	}

	// The terminator still closes the turn.
	resultEv := &msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "done"}}
	m.AssignMessageID(bridgeID, resultEv)
	if resultEv.TurnID != openTurn {
		t.Fatalf("result carried turn %q; want %q", resultEv.TurnID, openTurn)
	}
	// After the terminator, a fresh user_message opens a NEW turn.
	next := &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "again"}}
	m.AssignMessageID(bridgeID, next)
	if next.TurnID == openTurn || next.TurnID == "" {
		t.Fatalf("post-terminator user_message did not open a new turn: got %q (open was %q)", next.TurnID, openTurn)
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
