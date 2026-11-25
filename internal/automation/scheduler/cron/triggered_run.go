package cron

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/eventtriggers"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
)

// ErrRunsOnEventTrigger refuses Run now for a job with an event trigger: its
// prompt is a template that only a message can fill.
var ErrRunsOnEventTrigger = errors.New("this job runs when a message matches its event_trigger; it has no run now")

// TriggeredRunOutcome says what StartTriggeredRun did with one message for one
// job.
type TriggeredRunOutcome int

const (
	// TriggeredRunStarted: a run exists and is executing.
	TriggeredRunStarted TriggeredRunOutcome = iota
	// TriggeredRunNotRunnable: the job was deleted, disabled, put on hold or
	// given a schedule since the message was matched. The message is dropped
	// for this job, as a cron tick is.
	TriggeredRunNotRunnable
	// TriggeredRunJobBusy: the job's previous run is still in flight. Unlike a
	// cron tick, a message cannot simply be skipped, so the caller asks for it
	// again later.
	TriggeredRunJobBusy
)

// StartTriggeredRun starts a run of the job for one message, with the job's
// prompt filled from the message. It returns once the run exists; the run
// executes in the background. The run id is 0 unless the outcome is
// TriggeredRunStarted.
func (s *Scheduler) StartTriggeredRun(jobID int64, message eventtriggers.Message) (TriggeredRunOutcome, int64, error) {
	current, err := s.store.GetJob(jobID)
	if err == sql.ErrNoRows {
		return TriggeredRunNotRunnable, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read job %d: %w", jobID, err)
	}
	job := *current
	if !job.Enabled || job.IsOnHold() || job.EventTrigger == nil {
		return TriggeredRunNotRunnable, 0, nil
	}
	if !s.tryAcquire(job.ID) {
		return TriggeredRunJobBusy, 0, nil
	}
	startTime := time.Now()
	run, err := s.createRun(job)
	if err != nil {
		s.release(job.ID)
		return 0, 0, fmt.Errorf("create run of job %d: %w", job.ID, err)
	}
	prompt, renderErr := eventtriggers.RenderPrompt(job.Prompt, message)
	go func() {
		defer s.release(job.ID)
		defer s.recordJobPanic(job)
		if renderErr != nil {
			// Validate rendered this prompt against an empty message when the
			// job was saved, so this means the message itself broke it.
			s.finishRunWithoutExecuting(job, run, startTime, fmt.Sprintf("prompt template: %v", renderErr))
			return
		}
		s.runAcquiredJob(job, run, prompt, startTime)
	}()
	return TriggeredRunStarted, run.ID, nil
}

func (s *Scheduler) finishRunWithoutExecuting(job db.Job, run *db.Run, startTime time.Time, output string) {
	if err := s.store.FinishRun(run.ID, runstatus.Error, output); err != nil {
		s.logRunFinishFailure(job, run, err)
	}
	if s.logger != nil {
		s.logger.LogJobFinish(job.ID, job.Name, job.Schedule, run.ID, runstatus.Error, output, time.Since(startTime))
	}
}
