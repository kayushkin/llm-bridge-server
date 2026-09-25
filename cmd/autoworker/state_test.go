package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The cooldown lives in one small file, and it is the only memory this binary
// has. Every failure mode here is quiet: a state that never loads reads as "never
// fired" and dispatches on every tick; a state that never saves does the same.
//
// Ported from e0f686a5, where loadState read HOME itself. On main the path is an
// argument and where it points is pinned by
// TestHomeRelativePathsAreBuiltFromTheGivenHome, so the branch's path test is
// not carried over.

func statePathIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".config", "autoworker", "state.json")
}

func TestStateSurvivesASaveAndLoadRoundTrip(t *testing.T) {
	path := statePathIn(t)

	st, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState on a fresh install: %v", err)
	}
	st.LastFire["anthropic"] = 1_800_000_000
	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState after save: %v", err)
	}
	if got := reloaded.LastFire["anthropic"]; got != 1_800_000_000 {
		t.Errorf("the last fire did not survive the round trip: got %d, want 1800000000; "+
			"a cooldown that does not persist fires on every tick", got)
	}
}

// A fresh install has no state file, and that is not an error. Treating it as one
// makes the binary fatal on its very first tick and it never fires at all.
func TestAFreshInstallLoadsAnEmptyState(t *testing.T) {
	st, err := loadState(statePathIn(t))
	if err != nil {
		t.Fatalf("a missing state file is a fresh install, not a failure: %v", err)
	}
	if st == nil || st.LastFire == nil {
		t.Fatal("loadState returned a state whose map is nil; the first fire would panic " +
			"writing to it")
	}
	if len(st.LastFire) != 0 {
		t.Errorf("a fresh install remembers a fire it never made: %v", st.LastFire)
	}
}

// A state file whose map serialised as null is repaired on load. Without the
// repair the first write to it panics, and the panic happens AFTER the session
// has been published — so the money is spent and the cooldown is not recorded.
func TestAStateFileWithANullMapIsRepaired(t *testing.T) {
	path := statePathIn(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"last_fire": null}`), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if st.LastFire == nil {
		t.Fatal("the null map was not repaired; the next fire panics writing to it")
	}
	st.LastFire["anthropic"] = 1 // would panic on a nil map
}

// A malformed state file is an error, not a silently empty state. Swallowing it
// would turn a corrupt file into "never fired" and dispatch on every tick, which
// is the expensive direction.
func TestACorruptStateFileIsAnError(t *testing.T) {
	path := statePathIn(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadState(path); err == nil {
		t.Error("a corrupt state file was read as an empty one; the cooldown is then " +
			"absent and every tick fires")
	}
}
