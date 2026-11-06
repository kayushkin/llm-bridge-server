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

// TestManager_TheResultCarriesTheClientRequestIDOfItsTurn pins what chat
// links read at turn end: the result of a turn opened by a user_message
// carries that message's client_request_id, and the next turn's does not.
func TestManager_TheResultCarriesTheClientRequestIDOfItsTurn(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-client-request"
	if err := m.store.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, ClientRequestID: "chat-link-delivery:x", Result: &msg.ResultEvent{Text: "from a person"}})
	result := &msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "reply"}}
	m.AssignMessageID(bridgeID, result)
	if result.ClientRequestID != "chat-link-delivery:x" {
		t.Fatalf("result client_request_id = %q", result.ClientRequestID)
	}
	m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "from the operator"}})
	next := &msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "reply"}}
	m.AssignMessageID(bridgeID, next)
	if next.ClientRequestID != "" {
		t.Fatalf("the operator's turn carried %q", next.ClientRequestID)
	}
}

// otelUserMessage is Claude Code's OTel report of a prompt it started.
func otelUserMessage(bridgeID, text string) *msg.Event {
	return &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID,
		Extensions: map[string]json.RawMessage{"source": json.RawMessage(`"otel"`)},
		Result:     &msg.ResultEvent{Text: text}}
}

func seedClaudeCodeSession(t *testing.T, m *Manager, bridgeID string) {
	t.Helper()
	if err := m.store.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func resultOf(t *testing.T, m *Manager, bridgeID string) *msg.Event {
	t.Helper()
	result := &msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "reply"}}
	m.AssignMessageID(bridgeID, result)
	return result
}

// TestManager_ATaskNotificationStartedMidTurnKeepsTheOpenTurnsRequestID is
// what happened on 2026-11-05 at 05:33:59Z: a chat link sent a person's
// message, and in the same second Claude Code started a run for a background
// agent's task notification and took the person's message into it. Its OTel
// report is of the notification, not of the sent prompt. The one result of
// that run answered the person, so it must carry the delivery's id; it used to
// carry none, and the reply was never sent.
func TestManager_ATaskNotificationStartedMidTurnKeepsTheOpenTurnsRequestID(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-task-notification-mid-turn"
	seedClaudeCodeSession(t, m, bridgeID)

	sent := &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, ClientRequestID: "chat-link-delivery:a", Result: &msg.ResultEvent{Text: "from a person"}}
	m.AssignMessageID(bridgeID, sent)
	notification := otelUserMessage(bridgeID, "<task-notification>agent finished</task-notification>")
	m.AssignMessageID(bridgeID, notification)
	if notification.TurnID == sent.TurnID {
		t.Fatalf("a distinct OTel prompt joined the open turn id %q", sent.TurnID)
	}
	if result := resultOf(t, m, bridgeID); result.ClientRequestID != "chat-link-delivery:a" {
		t.Fatalf("result client_request_id = %q, want the delivery's", result.ClientRequestID)
	}

	// The prompt was answered, so it is no longer waiting for an echo: a
	// later run Claude Code starts by itself carries no id.
	m.AssignMessageID(bridgeID, otelUserMessage(bridgeID, "from a person"))
	if result := resultOf(t, m, bridgeID); result.ClientRequestID != "" {
		t.Fatalf("a late echo of an answered prompt carried %q", result.ClientRequestID)
	}
}

// TestManager_TwoPromptsSentAtOnceEachKeepTheirRequestID is what happened on
// 2026-11-05 at 06:07:32Z: a chat link's failure report and its next delivery
// went into the session 11 ms apart. Claude Code ran them one after the
// other, and reported each by OTel as it started it. The report's result must
// carry the report's id and the delivery's result the delivery's; both used
// to carry none.
func TestManager_TwoPromptsSentAtOnceEachKeepTheirRequestID(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-two-prompts-at-once"
	seedClaudeCodeSession(t, m, bridgeID)

	m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, ClientRequestID: "chat-link-report:r", Result: &msg.ResultEvent{Text: "a report"}})
	m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, ClientRequestID: "chat-link-delivery:d", Result: &msg.ResultEvent{Text: "from a person"}})

	m.AssignMessageID(bridgeID, otelUserMessage(bridgeID, "a report"))
	if result := resultOf(t, m, bridgeID); result.ClientRequestID != "chat-link-report:r" {
		t.Fatalf("the report's result carried %q", result.ClientRequestID)
	}
	m.AssignMessageID(bridgeID, otelUserMessage(bridgeID, "from a person"))
	if result := resultOf(t, m, bridgeID); result.ClientRequestID != "chat-link-delivery:d" {
		t.Fatalf("the delivery's result carried %q", result.ClientRequestID)
	}
}

// TestManager_ARunClaudeCodeStartsWhileIdleCarriesNoRequestID: a task
// notification with no turn open is Claude Code's own run, and its reply must
// never go to a chat link.
func TestManager_ARunClaudeCodeStartsWhileIdleCarriesNoRequestID(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-own-run"
	seedClaudeCodeSession(t, m, bridgeID)

	m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, ClientRequestID: "chat-link-delivery:a", Result: &msg.ResultEvent{Text: "from a person"}})
	m.AssignMessageID(bridgeID, otelUserMessage(bridgeID, "from a person"))
	resultOf(t, m, bridgeID)
	m.AssignMessageID(bridgeID, otelUserMessage(bridgeID, "<task-notification>agent finished</task-notification>"))
	if result := resultOf(t, m, bridgeID); result.ClientRequestID != "" {
		t.Fatalf("Claude Code's own run carried %q", result.ClientRequestID)
	}
}

// TestManager_UnstartedPromptsAreBounded: a harness with no OTel echo never
// takes its prompts back by echo, so the list must not grow without end.
func TestManager_UnstartedPromptsAreBounded(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-no-echo"
	seedClaudeCodeSession(t, m, bridgeID)
	for range 3 * unstartedPromptsKept {
		m.AssignMessageID(bridgeID, &msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Result: &msg.ResultEvent{Text: "no id"}})
	}
	if kept := len(m.msgState[bridgeID].unstartedPrompts); kept != unstartedPromptsKept {
		t.Fatalf("kept %d unstarted prompts, want %d", kept, unstartedPromptsKept)
	}
}
