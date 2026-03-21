package cron

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
)

func testStore(t *testing.T) *db.Store {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunJobNow(t *testing.T) {
	store := testStore(t)
	called := false
	sched := NewWithExecutor(store, func(cmd string, timeout time.Duration) executor.Result {
		called = true
		return executor.Result{Status: "success", Output: "mock output"}
	})

	job, err := store.CreateJob("test", "* * * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}

	run, err := sched.RunJobNow(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("executor not called")
	}
	if run.Status != "success" || run.Output != "mock output" {
		t.Fatalf("unexpected run: %+v", run)
	}
}

func TestRunJobNowNotFound(t *testing.T) {
	store := testStore(t)
	sched := New(store)
	_, err := sched.RunJobNow(999)
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
}

func TestDisabledJobsNotScheduled(t *testing.T) {
	store := testStore(t)
	callCount := 0
	sched := NewWithExecutor(store, func(cmd string, timeout time.Duration) executor.Result {
		callCount++
		return executor.Result{Status: "success", Output: ""}
	})

	job, _ := store.CreateJob("disabled", "* * * * *", "echo")
	store.UpdateJobEnabled(job.ID, false)

	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// The disabled job should not have been added to cron
	// 1 entry = the hold-clearing periodic task; the disabled job should NOT be added
	entries := sched.cron.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 cron entry (hold-clearer only) for disabled job, got %d", len(entries))
	}
}

func TestReloadPicksUpChanges(t *testing.T) {
	store := testStore(t)
	sched := New(store)

	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}

	// Initially 1 entry (hold-clearer only)
	if len(sched.cron.Entries()) != 1 {
		t.Fatalf("expected 1 entry (hold-clearer), got %d", len(sched.cron.Entries()))
	}

	store.CreateJob("new", "* * * * *", "echo")
	sched.Reload()

	if len(sched.cron.Entries()) != 2 {
		t.Fatalf("expected 2 entries after reload (job + hold-clearer), got %d", len(sched.cron.Entries()))
	}

	sched.Stop()
}

func TestInvalidScheduleLogsWarning(t *testing.T) {
	store := testStore(t)
	// Manually insert a job with bad schedule via SQL
	store.CreateJob("bad", "not-a-cron-expr", "echo")

	sched := New(store)
	// Start should not fail, just log warning
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	sched.Stop()

	// The bad job should not appear; only the hold-clearer entry
	if len(sched.cron.Entries()) != 1 {
		t.Fatalf("expected 1 entry (hold-clearer only) for invalid schedule, got %d", len(sched.cron.Entries()))
	}
}
