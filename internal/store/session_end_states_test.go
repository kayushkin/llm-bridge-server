package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ArchiveOld spared only state 'running', which no session has held since the
// derivation split it into model_generating, tool_running and the rest, so it
// would file a session mid-tool into Archive.
func TestArchiveOldSparesEveryActiveState(t *testing.T) {
	s := testStore(t)
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, state := range msg.ActiveSessionStates() {
		mustCreateSessionAt(t, s, "br_"+string(state), state, old)
	}
	for _, state := range []msg.SessionState{msg.SessionIdle, msg.SessionCompleted, msg.SessionAborted, msg.SessionAwaitingUser} {
		mustCreateSessionAt(t, s, "br_"+string(state), state, old)
	}

	archived, err := s.ArchiveOld(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("ArchiveOld: %v", err)
	}
	moved := map[string]bool{}
	for _, id := range archived {
		moved[id] = true
	}
	for _, state := range msg.ActiveSessionStates() {
		id := "br_" + string(state)
		sess, _ := s.GetSession(id)
		if moved[id] || sess.FolderName == ArchiveFolder {
			t.Errorf("%s session was archived; an active session must be spared", state)
		}
	}
	for _, state := range []msg.SessionState{msg.SessionIdle, msg.SessionCompleted, msg.SessionAborted, msg.SessionAwaitingUser} {
		id := "br_" + string(state)
		sess, _ := s.GetSession(id)
		if !moved[id] || sess.FolderName != ArchiveFolder {
			t.Errorf("%s session was not archived", state)
		}
	}
}

func mustCreateSessionAt(t *testing.T, s *Store, bridgeID string, state msg.SessionState, updatedAt time.Time) {
	t.Helper()
	if err := s.CreateSession(&Session{SessionID: bridgeID, Harness: "claude_code", State: string(state)}); err != nil {
		t.Fatalf("create %s: %v", bridgeID, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at=$1 WHERE bridge_id=$2`, updatedAt.UTC(), bridgeID); err != nil {
		t.Fatalf("age %s: %v", bridgeID, err)
	}
}

// Opening the store marks every subagent row linked to a parent as harness
// controlled, leaves rows it should not touch alone, and changes nothing when
// it runs again — it runs on every open.
func TestOpeningMarksLinkedSubagentsHarnessControlled(t *testing.T) {
	database := newTestDatabase(t)
	s := database.open()
	insert := func(bridgeID, purpose, managerSessionID, controlledBy string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO sessions (bridge_id, session_id, display_name, harness, state, purpose, manager_session_id, controlled_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, bridgeID, bridgeID, bridgeID, "claude_code", "idle", purpose, managerSessionID, controlledBy, time.Now(), time.Now()); err != nil {
			t.Fatalf("insert %s: %v", bridgeID, err)
		}
	}
	insert("br_parent", "chat", "", "")
	insert("br_linked_subagent", "subagent", "br_parent", "")
	insert("br_linked_workflow_subagent", "workflow-subagent", "br_parent", "")
	insert("br_unlinked_subagent", "subagent", "", "")
	insert("br_linked_delegate", "delegate", "br_parent", "")

	for pass := 1; pass <= 2; pass++ {
		s = database.open()
		want := map[string]string{
			"br_parent":                   "",
			"br_linked_subagent":          msg.ControlledByHarness,
			"br_linked_workflow_subagent": msg.ControlledByHarness,
			// No parent is known, so nothing says what process it ran in.
			"br_unlinked_subagent": "",
			// A delegate is a session the bridge started and controls.
			"br_linked_delegate": "",
		}
		for id, wantControlledBy := range want {
			sess, err := s.GetSession(id)
			if err != nil {
				t.Fatalf("get %s: %v", id, err)
			}
			if sess.ControlledBy != wantControlledBy {
				t.Errorf("pass %d: %s controlled_by = %q, want %q", pass, id, sess.ControlledBy, wantControlledBy)
			}
		}
	}
}

func TestListSignalsBySessionTypeReadsTheSessionRow(t *testing.T) {
	s := testStore(t)
	for id, sessionType := range map[string]msg.SessionType{"br_chat": msg.SessionTypeInteractive, "br_worker": msg.SessionTypeAutonomous} {
		if err := s.CreateSession(&Session{SessionID: id, Harness: "claude_code", State: "idle", Type: sessionType}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSignal(&Signal{ID: "sig_" + id, SessionID: id, SessionType: sessionType, Kind: msg.SignalKindQuestion,
			Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceChat, Title: "q"}); err != nil {
			t.Fatal(err)
		}
	}
	signals, err := s.ListSignals(SignalFilter{SessionTypes: []msg.SessionType{msg.SessionTypeInteractive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0].SessionID != "br_chat" {
		t.Fatalf("got %+v; want only br_chat's signal", signals)
	}
}
