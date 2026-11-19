package store

import (
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func storeForRunnerSpawnTests(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestARunnerSpawnsProgressNeverMovesBackAndFollowsOnlyTheCurrentSpawn(t *testing.T) {
	st := storeForRunnerSpawnTests(t)
	if err := st.RecordRunnerSpawn("br_1", "spawn_a", "m_1"); err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []uint64{3, 5, 4} {
		if err := st.AdvanceRunnerSpawn("br_1", "spawn_a", sequence); err != nil {
			t.Fatal(err)
		}
	}
	if spawn, _ := st.RunnerSpawnOfSession("br_1"); spawn == nil || spawn.LastSequence != 5 {
		t.Fatalf("after advancing to 3, 5 and then 4 the spawn is %+v, want stored through 5", spawn)
	}

	// The session is started again. A late write for the old spawn must not
	// touch the new one, whose numbering starts over.
	if err := st.RecordRunnerSpawn("br_1", "spawn_b", "m_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceRunnerSpawn("br_1", "spawn_a", 9); err != nil {
		t.Fatal(err)
	}
	if err := st.EndRunnerSpawn("br_1", "spawn_a"); err != nil {
		t.Fatal(err)
	}
	spawn, err := st.RunnerSpawnOfSession("br_1")
	if err != nil || spawn == nil || spawn.SpawnID != "spawn_b" || spawn.LastSequence != 0 || spawn.Ended {
		t.Fatalf("the new spawn is %+v (err %v), want spawn_b, nothing stored, open", spawn, err)
	}
	if none, err := st.RunnerSpawnOfSession("br_unknown"); err != nil || none != nil {
		t.Errorf("a session with no spawn answered %+v, %v", none, err)
	}
}

func TestAnEventAndItsEntryNumberAreStoredTogether(t *testing.T) {
	st := storeForRunnerSpawnTests(t)
	if err := st.RecordRunnerSpawn("br_1", "spawn_a", "m_1"); err != nil {
		t.Fatal(err)
	}
	rowID, err := st.StoreEventAdvancingRunnerSpawn("br_1", "system", "", "", []byte(`{"type":"system"}`), "br_1", "spawn_a", 7)
	if err != nil || rowID == 0 {
		t.Fatalf("StoreEventAdvancingRunnerSpawn: row %d, %v", rowID, err)
	}
	events, err := st.ListEventsSinceID("br_1", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("stored %d events (err %v), want 1", len(events), err)
	}
	if spawn, _ := st.RunnerSpawnOfSession("br_1"); spawn.LastSequence != 7 {
		t.Errorf("the spawn is stored through %d, want 7", spawn.LastSequence)
	}

	// A subagent's event is filed under the subagent's session, and still
	// counts as an entry of the process that produced it.
	if _, err := st.StoreEventAdvancingRunnerSpawn("br_subagent", "system", "", "", []byte(`{"type":"system"}`), "br_1", "spawn_a", 8); err != nil {
		t.Fatal(err)
	}
	if spawn, _ := st.RunnerSpawnOfSession("br_1"); spawn.LastSequence != 8 {
		t.Errorf("after a subagent's event the spawn is stored through %d, want 8", spawn.LastSequence)
	}
}

func TestStartupReconcileLeavesSessionsARunnerMayStillHold(t *testing.T) {
	st := storeForRunnerSpawnTests(t)
	running := string(msg.SessionRunning)
	for _, session := range []*Session{
		{SessionID: "br_held", Harness: msg.HarnessMock, State: running},
		{SessionID: "br_held_subagent", Harness: msg.HarnessMock, State: running, ManagerSessionID: "br_held", ControlledBy: msg.ControlledByHarness},
		{SessionID: "br_delegate_of_held", Harness: msg.HarnessMock, State: running, ManagerSessionID: "br_held"},
		{SessionID: "br_ended_spawn", Harness: msg.HarnessMock, State: running},
		{SessionID: "br_no_spawn", Harness: msg.HarnessMock, State: running},
	} {
		if err := st.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordRunnerSpawn("br_held", "spawn_a", "m_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRunnerSpawn("br_ended_spawn", "spawn_b", "m_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.EndRunnerSpawn("br_ended_spawn", "spawn_b"); err != nil {
		t.Fatal(err)
	}

	reconciled, err := st.ReconcileSessions(msg.ActiveSessionStates()...)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, session := range reconciled {
		got[session.SessionID] = true
	}
	// A delegate has a process of its own; only a harness subagent lives in
	// its manager's process.
	want := map[string]bool{"br_delegate_of_held": true, "br_ended_spawn": true, "br_no_spawn": true}
	for sessionID := range want {
		if !got[sessionID] {
			t.Errorf("%s was not reconciled; nothing holds its process", sessionID)
		}
	}
	for _, sessionID := range []string{"br_held", "br_held_subagent"} {
		if got[sessionID] {
			t.Errorf("%s was reset at startup, though a runner may still hold its process", sessionID)
		}
		if session, _ := st.GetSession(sessionID); session == nil || session.State != running {
			t.Errorf("%s was left in state %q, want %q", sessionID, session.State, running)
		}
	}
}
