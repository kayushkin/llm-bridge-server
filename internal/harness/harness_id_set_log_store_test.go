package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The pump announces a session's harness id with a system/harness_id_set
// event. Until 2026-09-16 it never reached log-store (135 of 174 sessions in
// that day's parity count). The session log is the only copy now; it must
// hold the event, ahead of the event that carried the id.
func TestHarnessIDSetIsStoredAheadOfTheEventThatCarriedIt(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-harness-id-set-parity"
	const harnessID = "cc-harness-uuid-1"
	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 2)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	proc.ch <- msg.Event{
		Type:             msg.EventSessionInfo,
		BridgeSessionID:  bridgeID,
		HarnessSessionID: harnessID,
		Harness:          msg.HarnessClaudeCode,
		Info:             &msg.SessionInfo{},
	}
	close(proc.ch)
	drainUntilClosed(t, sub)

	raw, err := m.sessionEventLog().ListEvents(bridgeID, 0, nil)
	if err != nil {
		t.Fatalf("list log-store events: %v", err)
	}
	var kinds []string
	for _, r := range raw {
		var ev msg.Event
		if err := json.Unmarshal(r, &ev); err != nil {
			t.Fatalf("decode log-store event %s: %v", r, err)
		}
		kind := string(ev.Type)
		if ev.System != nil {
			kind += "/" + ev.System.Subtype
			if ev.System.Subtype == "harness_id_set" && ev.System.Message != harnessID {
				t.Fatalf("harness_id_set carries %q; want %q", ev.System.Message, harnessID)
			}
		}
		kinds = append(kinds, kind)
	}
	idSet, carrier := indexOf(kinds, "system/harness_id_set"), indexOf(kinds, string(msg.EventSessionInfo))
	if idSet < 0 || carrier < 0 || idSet > carrier {
		t.Fatalf("log-store holds %v; want system/harness_id_set ahead of session_info", kinds)
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

// drainUntilClosed reads a subscriber channel until readEvents closes it,
// which it does once the process's events are all handled. Gives a test a
// definite "the pump is finished" point.
func drainUntilClosed(t *testing.T, sub chan StoredEvent) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-sub:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("pump never closed its subscriber channel")
		}
	}
}
