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

func TestFillDiscoveredSessionMetadataReplacesInjectedTitleAndFillsFolder(t *testing.T) {
	s := testStore(t)
	id := discoveredRow(t, s, "01a17f13-5a13-7950-be14-de83e37aff1d", injectedAgentsTitle)
	_, _, updatedBefore := titleAndFolder(t, s, id)

	changed, err := s.FillDiscoveredSessionMetadata(id, "Which are the recent orders?", "/work/repo")
	if err != nil {
		t.Fatalf("FillDiscoveredSessionMetadata: %v", err)
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

	again, err := s.FillDiscoveredSessionMetadata(id, "Which are the recent orders?", "/work/repo")
	if err != nil || len(again) != 0 {
		t.Errorf("second call changed %v (err %v), want nothing", again, err)
	}
}

func TestFillDiscoveredSessionMetadataKeepsTitlesSomeoneChose(t *testing.T) {
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
		changed, err := s.FillDiscoveredSessionMetadata(id, "Something else", "/work/repo")
		if err != nil {
			t.Fatalf("FillDiscoveredSessionMetadata(%s): %v", id, err)
		}
		if want := []string{"working_dir"}; !reflect.DeepEqual(changed, want) {
			t.Errorf("%s: changed = %v, want only the folder", id, changed)
		}
		if after, _, _ := titleAndFolder(t, s, id); after != before {
			t.Errorf("%s: title moved from %q to %q", id, before, after)
		}
	}
}

func TestFillDiscoveredSessionMetadataNeverWritesInjectedTextOrOverwritesAFolder(t *testing.T) {
	s := testStore(t)
	id := discoveredRow(t, s, "01a17f13-9000-7950-be14-de83e37aff1d", "")
	if _, err := s.db.Exec(`UPDATE sessions SET working_dir='/work/chosen' WHERE bridge_id=?`, id); err != nil {
		t.Fatalf("set folder: %v", err)
	}
	changed, err := s.FillDiscoveredSessionMetadata(id, "<environment_context>\n  <cwd>/work</cwd>", "/work/other")
	if err != nil {
		t.Fatalf("FillDiscoveredSessionMetadata: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want nothing: the title offered is codex's own text and the folder is already set", changed)
	}
	if title, folder, _ := titleAndFolder(t, s, id); title != "" || folder != "/work/chosen" {
		t.Errorf("row = (%q, %q)", title, folder)
	}
}
