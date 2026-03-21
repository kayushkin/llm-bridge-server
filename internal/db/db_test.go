package db

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndListJobs(t *testing.T) {
	s := testStore(t)
	j, err := s.CreateJob("test", "* * * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if j.ID == 0 {
		t.Fatal("expected non-zero id")
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "test" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestDeleteJob(t *testing.T) {
	s := testStore(t)
	j, _ := s.CreateJob("del", "* * * * *", "echo")
	if err := s.DeleteJob(j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(j.ID); err != sql.ErrNoRows {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}

func TestRuns(t *testing.T) {
	s := testStore(t)
	j, _ := s.CreateJob("r", "* * * * *", "echo")
	run, err := s.CreateRun(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(run.ID, "success", "ok"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "success" {
		t.Fatalf("unexpected: %+v", runs)
	}
}

func TestHold(t *testing.T) {
	s := testStore(t)
	j, _ := s.CreateJob("hold-test", "* * * * *", "echo")

	until := time.Now().Add(24 * time.Hour)
	if err := s.SetHold(j.ID, until); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(j.ID)
	if got.HoldUntil == nil {
		t.Fatal("expected hold_until to be set")
	}
	if !got.IsOnHold() {
		t.Fatal("expected job to be on hold")
	}

	if err := s.ClearHold(j.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(j.ID)
	if got.HoldUntil != nil {
		t.Fatal("expected hold_until to be nil after clear")
	}

	if err := s.SetHold(999, until); err != sql.ErrNoRows {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}

func TestClearExpiredHolds(t *testing.T) {
	s := testStore(t)
	j, _ := s.CreateJob("expired", "* * * * *", "echo")

	past := time.Now().Add(-1 * time.Hour)
	s.SetHold(j.ID, past)

	cleared, err := s.ClearExpiredHolds()
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Fatalf("expected 1 cleared, got %d", cleared)
	}
	got, _ := s.GetJob(j.ID)
	if got.HoldUntil != nil {
		t.Fatal("expected hold to be cleared")
	}
}
