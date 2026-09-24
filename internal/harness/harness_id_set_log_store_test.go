package harness

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The pump announces a session's harness id with a system/harness_id_set
// event. It went to bridge.db and to live subscribers but never to
// log-store, so the two event copies disagreed by one row per session —
// the largest class in the 2026-09-16 parity count (135 of 174 sessions).
// log-store is the durable copy; it must hold the event too, and ahead of
// the event that carried the id, as bridge.db does.
func TestHarnessIDSetReachesLogStoreAheadOfTheEventThatCarriedIt(t *testing.T) {
	m, _ := newSlowLogStoreManager(t, 0)
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

	raw, err := m.logStore.ListEvents(bridgeID, 0, nil)
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
