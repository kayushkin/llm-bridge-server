package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestMachineDiskDeletions(t *testing.T) {
	s := testStore(t)
	first, err := s.RecordRequestedMachineDiskDeletion(MachineDiskDeletion{MachineID: "m_pc", Path: `D:\Games\Old`, Kind: msg.DiskUsageEntryDirectory, ExpectedSizeBytes: 5000, RequestedByPrincipalID: "principal_000001"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "machine_disk_deletion_000001" || first.State != msg.DiskUsageDeletionRequested {
		t.Fatalf("first %+v", first)
	}
	second, err := s.RecordRequestedMachineDiskDeletion(MachineDiskDeletion{MachineID: "m_pc", Path: `D:\x.iso`, Kind: msg.DiskUsageEntryFile})
	if err != nil {
		t.Fatal(err)
	}

	// Another machine's runner cannot settle this machine's deletion.
	if err := s.UpdateMachineDiskDeletion("m_other", first.ID, msg.DiskUsageDeletion{State: msg.DiskUsageDeletionCompleted}, ""); err == nil {
		t.Fatal("another machine settled a deletion")
	}
	if err := s.MarkMachineDiskDeletionRunning("m_pc", first.ID); err != nil {
		t.Fatal(err)
	}
	if lost, err := s.MarkUnfinishedMachineDiskDeletionsLost("m_pc"); err != nil || lost != 2 {
		t.Fatalf("lost %d, %v", lost, err)
	}
	finishedAt := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	if err := s.UpdateMachineDiskDeletion("m_pc", first.ID, msg.DiskUsageDeletion{State: msg.DiskUsageDeletionPartial, FinishedAt: &finishedAt, DeletedBytes: 4000, DeletedFileCount: 3, FailedEntryCount: 1, Errors: []string{"in use"}}, ""); err != nil {
		t.Fatal(err)
	}
	// A late "running" does not undo a recorded end.
	if err := s.MarkMachineDiskDeletionRunning("m_pc", first.ID); err != nil {
		t.Fatal(err)
	}

	deletions, err := s.MachineDiskDeletions("m_pc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletions) != 2 || deletions[0].ID != second.ID || deletions[1].ID != first.ID {
		t.Fatalf("order %+v", deletions)
	}
	got := deletions[1]
	if got.State != msg.DiskUsageDeletionPartial || got.DeletedBytes != 4000 || got.FailedEntryCount != 1 || len(got.Errors) != 1 ||
		got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) || got.RequestedByPrincipalID != "principal_000001" || got.ExpectedSizeBytes != 5000 {
		t.Fatalf("first after its end %+v", got)
	}
	if deletions[0].State != msg.DiskUsageDeletionLost {
		t.Fatalf("second %+v", deletions[0])
	}
}
