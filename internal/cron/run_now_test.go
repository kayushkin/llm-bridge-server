package cron

import (
	"testing"
	"time"

	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/runstatus"
)

// An agent run follows its session for minutes, and the bridge's run-a-job
// button gives up after 30 seconds. So running an agent job now hands back the
// run as soon as it exists, still running, and the run finishes on its own.
func TestRunningAnAgentJobNowReturnsTheRunWhileItRuns(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJobFull(db.Job{Name: "agent", Schedule: "0 10 * * 1", Type: "agent", Prompt: "do it", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// No agent executor: the run ends as an error the moment it starts, which
	// is enough to see the run handed back before it is finished.
	s := New(store)

	run, err := s.RunJobNow(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != runstatus.Running {
		t.Fatalf("got %+v, want the run handed back while running", run)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, err := store.ListRuns(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Status == runstatus.Error {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never finished: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A job on hold is not run, and running it now says so with no run.
func TestRunningAHeldAgentJobNowReturnsNoRun(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJobFull(db.Job{Name: "agent", Schedule: "0 10 * * 1", Type: "agent", Prompt: "do it", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetHold(job.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	run, err := New(store).RunJobNow(job.ID)
	if err != nil || run != nil {
		t.Fatalf("got run %+v err %v, want neither", run, err)
	}
}
