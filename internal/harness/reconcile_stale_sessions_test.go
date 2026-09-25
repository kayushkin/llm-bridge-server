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

// The startup reconcile stored an idle status in bridge.db and never told
// log-store, so every restart left one event per stale session in one copy
// only (measured with cmd/event-copy-parity on 2026-09-25: the 21:09:00,
// 21:09:31, 21:17:19 and 21:28:41 restarts). Both copies must now hold the
// same event, byte for byte.
func TestReconcileStaleSessionsSendsTheStoredStatusToLogStore(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	logStore := newLogStoreStub()
	logStoreServer := httptest.NewServer(logStore)
	t.Cleanup(logStoreServer.Close)
	m := NewManager(st, logStoreServer.URL, "http://127.0.0.1:0", "", 0, nil)

	const staleID, idleID = "br_left_running", "br_already_idle"
	for id, state := range map[string]msg.SessionState{staleID: msg.SessionRunning, idleID: msg.SessionIdle} {
		if err := st.CreateSession(&store.Session{SessionID: id, Harness: msg.HarnessClaudeCode, State: string(state)}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	sessions, err := m.ReconcileStaleSessions()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != staleID {
		t.Fatalf("reconciled %+v; want only %s", sessions, staleID)
	}

	bridgeRows, err := st.ListEventsSinceID(staleID, 0)
	if err != nil {
		t.Fatalf("read bridge events: %v", err)
	}
	if len(bridgeRows) != 1 {
		t.Fatalf("bridge.db holds %d events; want the one session_status", len(bridgeRows))
	}

	var logStoreData []byte
	deadline := time.Now().Add(5 * time.Second)
	for logStoreData == nil && time.Now().Before(deadline) {
		logStore.mu.Lock()
		if events := logStore.events[staleID]; len(events) > 0 {
			logStoreData = events[0].data
		}
		logStore.mu.Unlock()
		if logStoreData == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if logStoreData == nil {
		t.Fatal("log-store never received the reconcile status")
	}

	var inLogStore msg.Event
	if err := json.Unmarshal(logStoreData, &inLogStore); err != nil {
		t.Fatalf("decode log-store body: %v", err)
	}
	if inLogStore.Type != msg.EventSessionStatus || inLogStore.Status == nil ||
		inLogStore.Status.State != msg.SessionIdle || inLogStore.Status.AsOf != int64(bridgeRows[0].RowID) {
		t.Fatalf("log-store got %s; want an idle session_status as of bridge row %d", logStoreData, bridgeRows[0].RowID)
	}
	// The parity check matches the two copies by their bytes.
	if string(logStoreData) != string(bridgeRows[0].Data) {
		t.Fatalf("copies differ:\nbridge.db %s\nlog-store %s", bridgeRows[0].Data, logStoreData)
	}

	logStore.mu.Lock()
	defer logStore.mu.Unlock()
	if n := len(logStore.events[idleID]); n != 0 {
		t.Fatalf("log-store got %d events for a session that was already idle; want 0", n)
	}
}
