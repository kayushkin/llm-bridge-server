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

	interrupted, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 2 {
		t.Fatalf("reconciled %d runs, want 2", len(interrupted))
	}

	runs, err := s.ListRuns(job.ID, MaxRunsListed)
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
	if len(again) != 0 {
		t.Fatalf("second reconcile touched %d runs, want 0", len(again))
	}
}

// The caller publishes one finish per returned run, so each one has to arrive
// carrying the job it belonged to. A count cannot say which jobs were killed.
func TestReconcileOrphanedRunsIdentifiesTheJobEachRunBelongedTo(t *testing.T) {
	s := testStore(t)
	nightly, err := s.CreateJob("nightly-guard", "0 3 * * *", "echo guard")
	if err != nil {
		t.Fatal(err)
	}
	frequent, err := s.CreateJob("every-five", "*/5 * * * *", "echo tick")
	if err != nil {
		t.Fatal(err)
	}
	nightlyRun, _ := s.CreateRun(nightly.ID)
	frequentRun, _ := s.CreateRun(frequent.ID)

	interrupted, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 2 {
		t.Fatalf("reconciled %d runs, want 2", len(interrupted))
	}

	byRunID := map[int64]InterruptedRun{}
	for _, r := range interrupted {
		byRunID[r.RunID] = r
	}

	got := byRunID[nightlyRun.ID]
	if got.JobID != nightly.ID || got.JobName != "nightly-guard" || got.Schedule != "0 3 * * *" {
		t.Errorf("stranded run reported as job %d %q %q, want %d %q %q",
			got.JobID, got.JobName, got.Schedule, nightly.ID, "nightly-guard", "0 3 * * *")
	}

	// A second job, so the join cannot pass by handing every run the same row.
	other := byRunID[frequentRun.ID]
	if other.JobID != frequent.ID || other.JobName != "every-five" || other.Schedule != "*/5 * * * *" {
		t.Errorf("stranded run reported as job %d %q %q, want %d %q %q",
			other.JobID, other.JobName, other.Schedule, frequent.ID, "every-five", "*/5 * * * *")
	}

	if got.StartedAt.IsZero() {
		t.Error("stranded run has no started_at, so its duration cannot be reported")
	}
	if got.FinishedAt.Before(got.StartedAt) {
		t.Errorf("finished_at %v precedes started_at %v", got.FinishedAt, got.StartedAt)
	}
}

// The runs table declares ON DELETE CASCADE, but nothing sets PRAGMA
// foreign_keys=ON, so SQLite does not enforce it and runs outlive their jobs.
// Measured on the live database: 1825 runs whose job is gone, 7 of them already
// "interrupted" — so this path is exercised in production, not hypothetical.
//
// An inner join would drop these from the returned set while the UPDATE still
// closed them, publishing fewer finishes than the number of runs ended.
func TestReconcileOrphanedRunsReportsARunWhoseJobIsGone(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("deleted-later", "0 0 * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	orphan, _ := s.CreateRun(job.ID)
	survivor, err := s.CreateJob("still-here", "0 1 * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	survivorRun, _ := s.CreateRun(survivor.ID)

	if err := s.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}

	interrupted, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 2 {
		t.Fatalf("reconciled %d runs, want 2 — the job-less run must not be dropped", len(interrupted))
	}

	var jobless *InterruptedRun
	for i := range interrupted {
		if interrupted[i].RunID == orphan.ID {
			jobless = &interrupted[i]
		}
	}
	if jobless == nil {
		t.Fatal("the run whose job was deleted was not reported")
	}
	// Left empty rather than invented; the job id still names the row that is gone.
	if jobless.JobName != "" || jobless.Schedule != "" {
		t.Errorf("job-less run reported name %q schedule %q, want both empty",
			jobless.JobName, jobless.Schedule)
	}
	if jobless.JobID != job.ID {
		t.Errorf("job-less run reported job id %d, want %d", jobless.JobID, job.ID)
	}

	// The surviving job's run still carries its name, so the join is not simply
	// returning empty strings for everything.
	for _, r := range interrupted {
		if r.RunID == survivorRun.ID && r.JobName != "still-here" {
			t.Errorf("surviving job's run reported name %q, want %q", r.JobName, "still-here")
		}
	}
}

// The caller publishes the output it is handed, so what comes back has to be
// what is stored. Two spellings of the explanation — one in SQL, one in Go —
// would put one string in the database and publish the other.
func TestReconcileOrphanedRunsReturnsTheOutputItStored(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("j", "* * * * *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	silent, _ := s.CreateRun(job.ID)

	// A run that had already written output before the process died. Its own
	// output is the useful one and must survive.
	noisy, _ := s.CreateRun(job.ID)
	if _, err := s.db.Exec(`UPDATE runs SET output='half a log line'::bytea WHERE id=$1`, noisy.ID); err != nil {
		t.Fatal(err)
	}

	interrupted, err := s.ReconcileOrphanedRuns()
	if err != nil {
		t.Fatal(err)
	}

	stored := map[int64]Run{}
	runs, err := s.ListRuns(job.ID, MaxRunsListed)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		stored[r.ID] = r
	}

	for _, r := range interrupted {
		if got := stored[r.RunID].Output; got != r.Output {
			t.Errorf("run %d: reconciliation returned output %q but stored %q", r.RunID, r.Output, got)
		}
	}

	for _, r := range interrupted {
		switch r.RunID {
		case silent.ID:
			if r.Output != InterruptedRunOutput {
				t.Errorf("a run with no output of its own reported %q, want %q", r.Output, InterruptedRunOutput)
			}
		case noisy.ID:
			if r.Output != "half a log line" {
				t.Errorf("a run that had written output reported %q, want it kept", r.Output)
			}
		}
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

	timeoutSeconds := 900
	if err := s.UpdateJob(plain.ID, JobPatch{TimeoutSecs: &timeoutSeconds}); err != nil {
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
