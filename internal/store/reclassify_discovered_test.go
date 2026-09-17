package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

type countingNotifier struct{ changed map[string]int }

func (n *countingNotifier) OnSessionChanged(bridgeID string) { n.changed[bridgeID]++ }
func (n *countingNotifier) OnSessionDeleted(string)          {}
func (n *countingNotifier) OnSignalsChanged(string)          {}

// importUnrecognised inserts a discovered row the way discovery did before the
// adapter recognised oneshot transcripts: no source, so purpose "discovered".
func importUnrecognised(t *testing.T, s *Store, harnessID string, updatedAt time.Time) string {
	t.Helper()
	id, inserted, err := s.UpsertDiscoveredSession(harnessID, harnessID, "classify this", "claude_code", "", "", "", updatedAt, updatedAt)
	if err != nil || !inserted {
		t.Fatalf("insert %s: inserted=%v err=%v", harnessID, inserted, err)
	}
	return id
}

func TestReclassifyDiscoveredSession_MovesADiscoveredRowToTheReportedPurpose(t *testing.T) {
	s := testStore(t)
	updatedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	id := importUnrecognised(t, s, "uuid-oneshot", updatedAt)

	notifier := &countingNotifier{changed: map[string]int{}}
	s.SetNotifier(notifier)

	changed, err := s.ReclassifyDiscoveredSession(id, msg.PurposeOneshot, "Oneshot")
	if err != nil || !changed {
		t.Fatalf("reclassify: changed=%v err=%v, want changed", changed, err)
	}
	got, err := s.GetSession(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Purpose != msg.PurposeOneshot {
		t.Errorf("purpose = %q, want %q", got.Purpose, msg.PurposeOneshot)
	}
	if got.Type != msg.SessionTypeExternal {
		t.Errorf("type = %q, want the registry's %q", got.Type, msg.SessionTypeExternal)
	}
	if got.FolderName != "Oneshot" {
		t.Errorf("folder = %q, want Oneshot (the row was still in the discovered folder)", got.FolderName)
	}
	if got.Origin != msg.OriginDiscovery {
		t.Errorf("origin = %q, want %q unchanged", got.Origin, msg.OriginDiscovery)
	}
	if !got.UpdatedAt.Equal(updatedAt) {
		t.Errorf("updated_at = %v, want %v untouched — reclassifying is not activity", got.UpdatedAt, updatedAt)
	}
	if notifier.changed[id] != 1 {
		t.Errorf("change notifications for %s = %d, want 1", id, notifier.changed[id])
	}

	// Second pass: already reclassified, nothing to do, nobody notified.
	changed, err = s.ReclassifyDiscoveredSession(id, msg.PurposeOneshot, "Oneshot")
	if err != nil || changed {
		t.Fatalf("second reclassify: changed=%v err=%v, want a no-op", changed, err)
	}
	if notifier.changed[id] != 1 {
		t.Errorf("a no-op notified: %d notifications", notifier.changed[id])
	}
}

func TestReclassifyDiscoveredSession_NeverOverwritesAPurposeSomethingElseSet(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	id, _, err := s.UpsertDiscoveredSession("uuid-sub", "uuid-sub", "sub", "claude_code", "", msg.PurposeSubagent, "Subagents", now, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	changed, err := s.ReclassifyDiscoveredSession(id, msg.PurposeOneshot, "Oneshot")
	if err != nil || changed {
		t.Fatalf("reclassify over subagent: changed=%v err=%v, want untouched", changed, err)
	}
	got, _ := s.GetSession(id)
	if got.Purpose != msg.PurposeSubagent || got.FolderName != "Subagents" {
		t.Errorf("row = purpose %q folder %q, want subagent/Subagents unchanged", got.Purpose, got.FolderName)
	}
}

func TestReclassifyDiscoveredSession_KeepsAFolderSomeoneChose(t *testing.T) {
	s := testStore(t)
	id := importUnrecognised(t, s, "uuid-archived", time.Now())
	if _, err := s.db.Exec(`UPDATE sessions SET folder_name='Archive' WHERE bridge_id=?`, id); err != nil {
		t.Fatalf("archive: %v", err)
	}
	changed, err := s.ReclassifyDiscoveredSession(id, msg.PurposeOneshot, "Oneshot")
	if err != nil || !changed {
		t.Fatalf("reclassify: changed=%v err=%v", changed, err)
	}
	got, _ := s.GetSession(id)
	if got.Purpose != msg.PurposeOneshot {
		t.Errorf("purpose = %q, want oneshot", got.Purpose)
	}
	if got.FolderName != "Archive" {
		t.Errorf("folder = %q, want Archive kept", got.FolderName)
	}
}

func TestReclassifyDiscoveredSession_RefusesUnregisteredOrGenericPurpose(t *testing.T) {
	s := testStore(t)
	id := importUnrecognised(t, s, "uuid-x", time.Now())
	for _, purpose := range []string{"not-a-purpose", msg.PurposeDiscovered, ""} {
		if _, err := s.ReclassifyDiscoveredSession(id, purpose, ""); err == nil {
			t.Errorf("purpose %q: want an error", purpose)
		}
	}
	got, _ := s.GetSession(id)
	if got.Purpose != msg.PurposeDiscovered {
		t.Errorf("purpose = %q after refused calls, want discovered", got.Purpose)
	}
}
