package harness

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A Claude Code session the bridge did not start has hook events in bridge.db
// and no sessions row, so every status derived for it fails its write and the
// transaction rolls back. It still went to log-store and to subscribers,
// stamped as of the rolled-back row id, which bridge.db then gave to the next
// event (measured by cmd/event-copy-parity on 2026-09-25: six such sessions in
// four hours, card 63fb5135). A status whose write failed now goes nowhere.
//
// The error event sent after it is the control: it takes the same path and
// must arrive, so an empty log-store cannot pass this test by being slow.
func TestAStatusWhoseWriteRolledBackReachesNeitherLogStoreNorSubscribers(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	logStore := newLogStoreStub()
	logStoreServer := httptest.NewServer(logStore)
	t.Cleanup(logStoreServer.Close)
	m := NewManager(st, logStoreServer.URL, "http://127.0.0.1:0", "", 0, nil)

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

	var logStoreTypes []string
	deadline := time.Now().Add(5 * time.Second)
	for len(logStoreTypes) == 0 && time.Now().Before(deadline) {
		logStore.mu.Lock()
		for _, event := range logStore.events[hookOnlyID] {
			logStoreTypes = append(logStoreTypes, event.typ)
		}
		logStore.mu.Unlock()
		if len(logStoreTypes) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(logStoreTypes) != 1 || logStoreTypes[0] != string(msg.EventError) {
		t.Fatalf("log-store got %v; want only the control %s", logStoreTypes, msg.EventError)
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
