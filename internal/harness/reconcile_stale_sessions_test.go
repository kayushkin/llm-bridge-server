package harness

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The startup reconcile stores one idle status per stale session, in the
// session log, with the row: until 2026-09-25 it reached bridge.db only, and
// every restart left one event per stale session in one copy. A session that
// was already idle gets nothing.
func TestReconcileStaleSessionsStoresTheStatusWithItsRow(t *testing.T) {
	m, st, _ := newSingleCopyTestManager(t)

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

	rows, err := m.SessionEvents().ListEventsSinceID(staleID, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the session log holds %d events; want the one session_status", len(rows))
	}
	var stored msg.Event
	if err := json.Unmarshal(rows[0].Data, &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.Type != msg.EventSessionStatus || stored.Status == nil ||
		stored.Status.State != msg.SessionIdle || stored.Status.AsOf != int64(rows[0].RowID) {
		t.Fatalf("stored %s; want an idle session_status as of its own id %d", rows[0].Data, rows[0].RowID)
	}
	row, err := st.GetSession(staleID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != string(msg.SessionIdle) || row.Status.AsOf != int64(rows[0].RowID) {
		t.Fatalf("row %q as of %d; want idle as of %d", row.State, row.Status.AsOf, rows[0].RowID)
	}

	idleRows, err := m.SessionEvents().ListEventsSinceID(idleID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(idleRows) != 0 {
		t.Fatalf("the session log holds %d events for a session that was already idle; want 0", len(idleRows))
	}
}
