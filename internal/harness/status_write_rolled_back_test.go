package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// A Claude Code session the bridge did not start has hook events and no
// sessions row, so every status derived for it fails its write. Until
// 2026-09-25 it still went to log-store and to subscribers, stamped as of a
// rolled-back row id (six such sessions in four hours, card 63fb5135). A
// status whose write failed goes nowhere: not into the session log, not to a
// subscriber.
//
// The error event sent after it is the control: it takes the same path and
// must arrive.
func TestAStatusWhoseWriteRolledBackReachesNeitherTheSessionLogNorSubscribers(t *testing.T) {
	m, _, _ := newSingleCopyTestManager(t)

	const hookOnlyID = "br_hook_only_no_sessions_row"
	subscriber := m.Subscribe(hookOnlyID)
	t.Cleanup(func() { m.Unsubscribe(hookOnlyID, subscriber) })

	now := time.Now()
	statusErr := m.broadcastDerived(hookOnlyID, &msg.Event{
		Type:      msg.EventSessionStatus,
		Harness:   msg.HarnessClaudeCode,
		Timestamp: now,
		Status:    &msg.SessionStatus{State: msg.SessionIdle, ChangedAt: now},
	})
	if statusErr == nil {
		t.Fatal("status write for a session with no sessions row succeeded; want it to fail")
	}

	if controlErr := m.broadcastDerived(hookOnlyID, &msg.Event{
		Type:      msg.EventError,
		Timestamp: now,
		Error:     &msg.ErrorEvent{Message: "control event"},
	}); controlErr != nil {
		t.Fatalf("control event: %v", controlErr)
	}

	stored, err := m.SessionEvents().ListEventsSinceID(hookOnlyID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var storedTypes []msg.EventType
	for _, row := range stored {
		var ev msg.Event
		if err := json.Unmarshal(row.Data, &ev); err != nil {
			t.Fatal(err)
		}
		storedTypes = append(storedTypes, ev.Type)
	}
	if len(storedTypes) != 1 || storedTypes[0] != msg.EventError {
		t.Fatalf("the session log holds %v; want only the control %s", storedTypes, msg.EventError)
	}

	var subscriberTypes []msg.EventType
	for drained := false; !drained; {
		select {
		case stored := <-subscriber:
			subscriberTypes = append(subscriberTypes, stored.Event.Type)
		default:
			drained = true
		}
	}
	if len(subscriberTypes) != 1 || subscriberTypes[0] != msg.EventError {
		encoded, _ := json.Marshal(subscriberTypes)
		t.Fatalf("subscriber got %s; want only the control %s", encoded, msg.EventError)
	}
}
