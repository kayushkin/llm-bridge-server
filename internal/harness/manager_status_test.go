package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func statusEvents(events []StoredEvent) []StoredEvent {
	var out []StoredEvent
	for _, ev := range events {
		if ev.Type == msg.EventSessionStatus {
			out = append(out, ev)
		}
	}
	return out
}

// The property the whole status rests on: a status's AsOf is its own event row
// id, the same value is on the session row, and a later status has a larger
// one — so two copies read from two streams can always be put in order.
func TestManager_StatusAsOfIsItsOwnRowIDAndMatchesTheSessionRow(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-status-asof"
	if err := m.store.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 4)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	proc.ch <- msg.Event{Type: msg.EventToolCall, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"sleep 600"}`)}}
	// user_message, session_state, session_status, tool_call, session_state, session_status.
	statuses := statusEvents(recvWithin(t, sub, 6, 2*time.Second))
	if len(statuses) != 2 {
		t.Fatalf("got %d session_status events; want 2", len(statuses))
	}
	for i, ev := range statuses {
		if ev.RowID == 0 || ev.Status.AsOf != ev.RowID {
			t.Fatalf("status %d: AsOf = %d, row id = %d; want them equal and non-zero", i, ev.Status.AsOf, ev.RowID)
		}
	}
	if statuses[1].Status.AsOf <= statuses[0].Status.AsOf {
		t.Fatalf("AsOf went %d → %d; want it to grow", statuses[0].Status.AsOf, statuses[1].Status.AsOf)
	}

	row, err := m.store.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if row.State != string(msg.SessionToolRunning) || row.Status == nil || row.Status.AsOf != statuses[1].Status.AsOf ||
		len(row.Status.Tools) != 1 || row.Status.Tools[0].Summary != "sleep 600" {
		t.Fatalf("session row: state=%q status=%+v; want the newest status, tool included", row.State, row.Status)
	}

	// Stop, with Bash still in flight. One status, newer than the tool's, says
	// paused with nothing running — on the stream and on the row alike.
	if changed, err := m.ForceSessionState(bridgeID, msg.SessionPaused, "user_interrupt"); err != nil || !changed {
		t.Fatalf("ForceSessionState: changed=%v err=%v", changed, err)
	}
	paused := statusEvents(recvWithin(t, sub, 2, 2*time.Second))
	if len(paused) != 1 || paused[0].Status.State != msg.SessionPaused || len(paused[0].Status.Tools) != 0 ||
		paused[0].Status.AsOf <= statuses[1].Status.AsOf {
		t.Fatalf("paused status = %+v; want paused, no tools, AsOf above %d", paused, statuses[1].Status.AsOf)
	}
	row, _ = m.store.GetSession(bridgeID)
	if row.State != string(msg.SessionPaused) || row.Status.AsOf != paused[0].Status.AsOf {
		t.Fatalf("session row after pause: state=%q status=%+v", row.State, row.Status)
	}
	close(proc.ch)
}

// A state written while the session has no process — a kill, the reaper, a
// failed spawn — used to reach the row and nothing else. It is an event on the
// session's own stream now, with an AsOf like any other.
func TestManager_ForcedStateWithNoProcessIsBroadcastWithAnAsOf(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-status-no-process"
	if err := m.store.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionToolRunning)}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sub := m.Subscribe(bridgeID)
	if changed, err := m.ForceSessionState(bridgeID, msg.SessionAborted, "idle_reaped"); err != nil || !changed {
		t.Fatalf("ForceSessionState: changed=%v err=%v", changed, err)
	}
	got := recvWithin(t, sub, 1, 2*time.Second)
	if got[0].Type != msg.EventSessionStatus || got[0].Status.State != msg.SessionAborted || got[0].Status.AsOf == 0 || got[0].Status.AsOf != got[0].RowID {
		t.Fatalf("broadcast = %+v status=%+v", got[0].Event, got[0].Status)
	}
	row, _ := m.store.GetSession(bridgeID)
	if row.State != string(msg.SessionAborted) || row.Status.AsOf != got[0].RowID {
		t.Fatalf("session row: state=%q status=%+v", row.State, row.Status)
	}
}
