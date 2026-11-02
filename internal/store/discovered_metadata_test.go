package store

import (
	"reflect"
	"testing"
	"time"
)

const injectedAgentsTitle = "# AGENTS.md instructions for /work/repo <INSTRUCTIONS> # Directives **Use the requested UI source.**"

func discoveredRow(t *testing.T, s *Store, harnessID, displayName string) string {
	t.Helper()
	now := time.Now()
	id, inserted, err := s.UpsertDiscoveredSession(harnessID, harnessID, displayName, "codex", "", "", "", now, now)
	if err != nil || !inserted {
		t.Fatalf("UpsertDiscoveredSession(%s): inserted=%v err=%v", harnessID, inserted, err)
	}
	return id
}

func titleAndFolder(t *testing.T, s *Store, bridgeID string) (string, string, time.Time) {
	t.Helper()
	var title, folder string
	var updated time.Time
	if err := s.db.QueryRow(`SELECT display_name, working_dir, updated_at FROM sessions WHERE bridge_id=?`, bridgeID).Scan(&title, &folder, &updated); err != nil {
		t.Fatalf("read %s: %v", bridgeID, err)
	}
	return title, folder, updated
}

func TestFillOrRepairDiscoveredSessionMetadataReplacesInjectedTitleAndFillsFolder(t *testing.T) {
	s := testStore(t)
	id := discoveredRow(t, s, "01a17f13-5a13-7950-be14-de83e37aff1d", injectedAgentsTitle)
	_, _, updatedBefore := titleAndFolder(t, s, id)

	changed, err := s.FillOrRepairDiscoveredSessionMetadata(id, "Which are the recent orders?", "/work/repo")
	if err != nil {
		t.Fatalf("FillOrRepairDiscoveredSessionMetadata: %v", err)
	}
	if want := []string{"working_dir", "display_name"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	title, folder, updatedAfter := titleAndFolder(t, s, id)
	if title != "Which are the recent orders?" || folder != "/work/repo" {
		t.Errorf("row = (%q, %q), want the typed request and the rollout's cwd", title, folder)
	}
	if !updatedAfter.Equal(updatedBefore) {
		t.Errorf("updated_at moved from %v to %v; correcting a label is not activity", updatedBefore, updatedAfter)
	}

	again, err := s.FillOrRepairDiscoveredSessionMetadata(id, "Which are the recent orders?", "/work/repo")
	if err != nil || len(again) != 0 {
		t.Errorf("second call changed %v (err %v), want nothing", again, err)
	}
}

func TestFillOrRepairDiscoveredSessionMetadataKeepsTitlesSomeoneChose(t *testing.T) {
	s := testStore(t)

	renamedByPerson := discoveredRow(t, s, "01a17f13-6000-7950-be14-de83e37aff1d", injectedAgentsTitle)
	if err := s.UpdateSessionDisplayName(renamedByPerson, "<INSTRUCTIONS> is what I want it called"); err != nil {
		t.Fatalf("UpdateSessionDisplayName: %v", err)
	}
	renamedByRenamer := discoveredRow(t, s, "01a17f13-7000-7950-be14-de83e37aff1d", "")
	if _, err := s.db.Exec(`UPDATE sessions SET display_name=?, display_name_named_at_turn=3 WHERE bridge_id=?`, injectedAgentsTitle, renamedByRenamer); err != nil {
		t.Fatalf("stamp renamer: %v", err)
	}
	plainTitle := discoveredRow(t, s, "01a17f13-8000-7950-be14-de83e37aff1d", "Fix the date parser")

	for _, id := range []string{renamedByPerson, renamedByRenamer, plainTitle} {
		before, _, _ := titleAndFolder(t, s, id)
		changed, err := s.FillOrRepairDiscoveredSessionMetadata(id, "Something else", "/work/repo")
		if err != nil {
			t.Fatalf("FillOrRepairDiscoveredSessionMetadata(%s): %v", id, err)
		}
		if want := []string{"working_dir"}; !reflect.DeepEqual(changed, want) {
			t.Errorf("%s: changed = %v, want only the folder", id, changed)
		}
		if after, _, _ := titleAndFolder(t, s, id); after != before {
			t.Errorf("%s: title moved from %q to %q", id, before, after)
		}
	}
}

func TestFillOrRepairDiscoveredSessionMetadataNeverWritesInjectedTextOrOverwritesAFolder(t *testing.T) {
	s := testStore(t)
	id := discoveredRow(t, s, "01a17f13-9000-7950-be14-de83e37aff1d", "")
	if _, err := s.db.Exec(`UPDATE sessions SET working_dir='/work/chosen' WHERE bridge_id=?`, id); err != nil {
		t.Fatalf("set folder: %v", err)
	}
	changed, err := s.FillOrRepairDiscoveredSessionMetadata(id, "<environment_context>\n  <cwd>/work</cwd>", "/work/other")
	if err != nil {
		t.Fatalf("FillOrRepairDiscoveredSessionMetadata: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want nothing: the title offered is codex's own text and the folder is already set", changed)
	}
	if title, folder, _ := titleAndFolder(t, s, id); title != "" || folder != "/work/chosen" {
		t.Errorf("row = (%q, %q)", title, folder)
	}
}

func TestFillOrRepairDiscoveredSessionMetadataRepairsTheClaudeCodeProjectNameMisreading(t *testing.T) {
	s := testStore(t)

	misread := discoveredRow(t, s, "8f1d5481-1914-48a4-8b66-194179709d6b", "Email alert poller")
	if _, err := s.FillOrRepairDiscoveredSessionMetadata(misread, "", "/work/llm/bridge/server"); err != nil {
		t.Fatalf("seed the misread folder: %v", err)
	}
	changed, err := s.FillOrRepairDiscoveredSessionMetadata(misread, "", "/work/llm-bridge-server")
	if err != nil {
		t.Fatalf("FillOrRepairDiscoveredSessionMetadata: %v", err)
	}
	if want := []string{"working_dir"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if _, folder, _ := titleAndFolder(t, s, misread); folder != "/work/llm-bridge-server" {
		t.Errorf("working_dir = %q, want the transcript's cwd /work/llm-bridge-server", folder)
	}

	chosen := discoveredRow(t, s, "8f1d5481-1914-48a4-8b66-194179709d6c", "Elsewhere")
	if _, err := s.FillOrRepairDiscoveredSessionMetadata(chosen, "", "/work/other"); err != nil {
		t.Fatalf("seed a folder: %v", err)
	}
	if changed, err := s.FillOrRepairDiscoveredSessionMetadata(chosen, "", "/work/llm-bridge-server"); err != nil || len(changed) != 0 {
		t.Errorf("changed %v (err %v); a folder that is not the misreading must be kept", changed, err)
	}
}

func TestIsClaudeCodeProjectNameMisreading(t *testing.T) {
	cases := []struct {
		stored, actual string
		want           bool
	}{
		{"/work/llm/bridge/server", "/work/llm-bridge-server", true},
		{"/home/u//claude/projects", "/home/u/.claude/projects", true},
		{"/work/photo/store/wt/matcher", "/work/photo_store/wt/matcher", true},
		{"/work/repo", "/work/repo", false},
		{"/work/llm/bridge", "/work/llm-bridge-server", false},
		{"/work/llm-bridge-server", "/work/llm/bridge/server", false},
	}
	for _, c := range cases {
		if got := isClaudeCodeProjectNameMisreading(c.stored, c.actual); got != c.want {
			t.Errorf("isClaudeCodeProjectNameMisreading(%q, %q) = %v, want %v", c.stored, c.actual, got, c.want)
		}
	}
}
