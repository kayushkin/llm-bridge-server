package cron

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/bus/messages"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/eventtriggers"
)

func triggerMessage(t *testing.T) eventtriggers.Message {
	t.Helper()
	subject, _ := eventtriggers.FindSubject(messages.MessagingInboundSubject)
	data, _ := json.Marshal(messages.MessagingInbound{EventID: "$a", Body: "hi"})
	message, err := eventtriggers.DecodeMessage(subject, data)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func createTriggeredJob(t *testing.T, store *db.Store) *db.Job {
	t.Helper()
	job, err := store.CreateJobFull(db.Job{Name: "t", Type: "agent", Agent: "claude-code", Prompt: "{{.body}}", Enabled: true,
		EventTrigger: &eventtriggers.Trigger{Subject: messages.MessagingInboundSubject}})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestStartTriggeredRun(t *testing.T) {
	store := testStore(t)
	scheduler := New(store)
	job := createTriggeredJob(t, store)

	// Held in flight by someone else: busy, and no run is created.
	scheduler.tryAcquire(job.ID)
	if outcome, _, err := scheduler.StartTriggeredRun(job.ID, triggerMessage(t)); err != nil || outcome != TriggeredRunJobBusy {
		t.Fatalf("in flight: %v, %v", outcome, err)
	}
	scheduler.release(job.ID)

	outcome, runID, err := scheduler.StartTriggeredRun(job.ID, triggerMessage(t))
	if err != nil || outcome != TriggeredRunStarted || runID == 0 {
		t.Fatalf("start: %v, %d, %v", outcome, runID, err)
	}
	// This scheduler has no agent executor, so the run ends in error at once;
	// what matters is that one run exists and the job is released.
	deadline := time.Now().Add(5 * time.Second)
	for scheduler.isInFlightForTest(job.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	runs, _ := store.ListRuns(job.ID, 10)
	if len(runs) != 1 || runs[0].ID != runID || runs[0].Status == "running" {
		t.Fatalf("runs = %+v", runs)
	}

	store.UpdateJob(job.ID, db.JobPatch{Enabled: new(bool)})
	if outcome, _, _ := scheduler.StartTriggeredRun(job.ID, triggerMessage(t)); outcome != TriggeredRunNotRunnable {
		t.Fatalf("disabled: %v", outcome)
	}
	if outcome, _, _ := scheduler.StartTriggeredRun(job.ID+100, triggerMessage(t)); outcome != TriggeredRunNotRunnable {
		t.Fatalf("deleted: %v", outcome)
	}
}

func TestRunJobNowRefusesATriggeredJob(t *testing.T) {
	store := testStore(t)
	job := createTriggeredJob(t, store)
	if _, err := New(store).RunJobNow(job.ID); err != ErrRunsOnEventTrigger {
		t.Fatalf("RunJobNow = %v", err)
	}
}

// Start leaves a triggered job off the cron schedule: it has none.
func TestStartSkipsATriggeredJob(t *testing.T) {
	store := testStore(t)
	createTriggeredJob(t, store)
	scheduler := New(store)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	// Only the hold-clearing entry Start always adds.
	if entries := len(scheduler.cron.Entries()); entries != 1 {
		t.Fatalf("cron entries = %d, want 1", entries)
	}
}

func (s *Scheduler) isInFlightForTest(jobID int64) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	return s.inFlight[jobID]
}
