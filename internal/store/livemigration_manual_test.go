package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateACopyOfTheLiveDatabase is a manual check, skipped unless
// LIVE_BRIDGE_DB names a database to copy. A fresh-DB test cannot prove the
// ADD COLUMN lands on the ~8k-row schema the gateway actually carries.
func TestMigrateACopyOfTheLiveDatabase(t *testing.T) {
	src := os.Getenv("LIVE_BRIDGE_DB")
	if src == "" {
		t.Skip("set LIVE_BRIDGE_DB to a copy of a live bridge.db")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "bridge.db")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write copy: %v", err)
	}

	s, err := New(dst)
	if err != nil {
		t.Fatalf("open+migrate: %v", err)
	}
	defer s.Close()

	sessions, err := s.ListSessions()
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	t.Logf("listed %d sessions after migration", len(sessions))
	if len(sessions) == 0 {
		t.Fatalf("no sessions read back from the live copy")
	}
	nonEmpty := 0
	for _, x := range sessions {
		if x.WorkingDir != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 0 {
		t.Errorf("%d legacy rows came back with a working_dir; every one must inherit", nonEmpty)
	}
	x := sessions[0]
	t.Logf("spot check: id=%s harness=%s state=%s purpose=%q origin=%q folder=%q mode=%q budget=%v",
		x.SessionID, x.Harness, x.State, x.Purpose, x.Origin, x.FolderName, x.Mode, x.MaxBudgetUSD)
	if x.Harness == "" || x.State == "" {
		t.Errorf("scan misaligned on a live row: %+v", x)
	}
}
