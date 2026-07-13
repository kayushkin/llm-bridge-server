package db

import (
	"testing"
)

// Runs left "running" by a process that died (crash, OOM kill, deploy restart)
// must be closed out at startup. Nothing else ever clears them, so without this
// they accumulate forever and read as concurrent duplicates of the job.
func TestReconcileOrphanedRuns(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("j", "* * * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}

	// Two runs stranded mid-flight, plus one that completed normally.
	orphanA, _ := s.CreateRun(job.ID)
	orphanB, _ := s.CreateRun(job.ID)
	done, _ := s.CreateRun(job.ID)
	if err := s.FinishRun(done.ID, "success", "hi"); err != nil {
		t.Fatal(err)
	}

	n, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reconciled %d runs, want 2", n)
	}

	runs, err := s.ListRuns(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}

	for _, id := range []int64{orphanA.ID, orphanB.ID} {
		r := byID[id]
		if r.Status != "interrupted" {
			t.Fatalf("orphan %d has status %q, want interrupted", id, r.Status)
		}
		if r.FinishedAt == nil {
			t.Fatalf("orphan %d has no finished_at", id)
		}
		if r.Output == "" {
			t.Fatalf("orphan %d has no explanation in output", id)
		}
	}

	// A completed run must not be touched.
	if got := byID[done.ID]; got.Status != "success" || got.Output != "hi" {
		t.Fatalf("completed run was rewritten: status=%q output=%q", got.Status, got.Output)
	}

	// Idempotent: a second pass has nothing left to do.
	again, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second reconcile touched %d runs, want 0", again)
	}
}

// The per-job timeout must survive a write/read round-trip and default to 0.
func TestJobTimeoutRoundTrip(t *testing.T) {
	s := testStore(t)

	withTimeout, err := s.CreateJobFull(Job{
		Name: "long", Schedule: "0 4 * * *", Command: "sleep 1",
		Type: "shell", TimeoutSecs: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(withTimeout.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSecs != 3600 {
		t.Fatalf("timeout_seconds round-tripped as %d, want 3600", got.TimeoutSecs)
	}

	// Unset means 0, which the executor reads as "use the default".
	plain, err := s.CreateJob("plain", "* * * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(plain.ID); got.TimeoutSecs != 0 {
		t.Fatalf("unset timeout is %d, want 0", got.TimeoutSecs)
	}

	if err := s.UpdateJobTimeout(plain.ID, 900); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(plain.ID); got.TimeoutSecs != 900 {
		t.Fatalf("after update timeout is %d, want 900", got.TimeoutSecs)
	}

	// ListJobs must carry it too — the scheduler builds its cron entries there.
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == withTimeout.ID && j.TimeoutSecs != 3600 {
			t.Fatalf("ListJobs dropped timeout_seconds (%d)", j.TimeoutSecs)
		}
	}
}
