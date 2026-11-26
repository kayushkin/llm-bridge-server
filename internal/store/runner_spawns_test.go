package store

import (
	"testing"
)

func storeForRunnerSpawnTests(t *testing.T) *Store {
	t.Helper()
	st, err := newTestStore(t), error(nil)
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
