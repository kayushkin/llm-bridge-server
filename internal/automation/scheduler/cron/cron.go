package cron

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
)

type Scheduler struct {
	cron          *cron.Cron
	store         *db.Store
	exec          func(command string, timeout time.Duration) executor.Result
	agentExecutor *executor.AgentExecutor
	logger        Logger
	busHandler    BusHandler

	// inFlight holds the jobs currently executing, so a tick that arrives while
	// the previous run of the same job is still going is skipped rather than
	// stacked on top of it. Deliberately in-memory: a run cannot survive the
	// process, so a fresh process has nothing in flight by definition.
	inFlightMu sync.Mutex
	inFlight   map[int64]bool
}

// Logger interface for logging events
type Logger interface {
	LogJobStart(jobID int64, jobName, schedule string, runID int64)
	LogJobFinish(jobID int64, jobName, schedule string, runID int64, status, output string, duration time.Duration)
	LogSchedulerLifecycle(action string, details ...string)
	LogError(errorType, message string, metadata map[string]interface{})
}

// BusHandler interface for publishing events
type BusHandler interface {
	PublishRunResult(run *db.Run, job *db.Job, startedAt, finishedAt string, duration int64)
}

func New(store *db.Store) *Scheduler {
	return &Scheduler{
		cron:     cron.New(),
		store:    store,
		exec:     executor.Run,
		inFlight: make(map[int64]bool),
	}
}

// NewWithExecutor creates a scheduler with a custom executor (for testing).
func NewWithExecutor(store *db.Store, execFn func(string, time.Duration) executor.Result) *Scheduler {
	return &Scheduler{
		cron:     cron.New(),
		store:    store,
		exec:     execFn,
		inFlight: make(map[int64]bool),
	}
}

// tryAcquire marks a job as in flight, reporting false if it already was.
func (s *Scheduler) tryAcquire(jobID int64) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	if s.inFlight[jobID] {
		return false
	}
	s.inFlight[jobID] = true
	return true
}

func (s *Scheduler) release(jobID int64) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	delete(s.inFlight, jobID)
}

// SetLogger sets the logger for the scheduler
func (s *Scheduler) SetLogger(logger Logger) {
	s.logger = logger
}

// SetBusHandler sets the bus handler for the scheduler
func (s *Scheduler) SetBusHandler(busHandler BusHandler) {
	s.busHandler = busHandler
}

// SetAgentExecutor sets the agent executor for the scheduler
func (s *Scheduler) SetAgentExecutor(agentExec *executor.AgentExecutor) {
	s.agentExecutor = agentExec
}

func (s *Scheduler) Start() error {
	// Clear any expired holds on startup
	if cleared, err := s.store.ClearExpiredHolds(); err != nil {
		log.Printf("warning: failed to clear expired holds: %v", err)
	} else if cleared > 0 {
		log.Printf("cleared %d expired holds on startup", cleared)
	}

	jobs, err := s.store.ListJobs()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		// A triggered job has no schedule; the event-trigger consumer starts it.
		if !j.Enabled || j.EventTrigger != nil {
			continue
		}
		if _, err := s.addJob(j); err != nil {
			log.Printf("warning: failed to add job %d (%s) with schedule %q: %v", j.ID, j.Name, j.Schedule, err)
		}
	}

	// Add a periodic task to clear expired holds every minute
	s.cron.AddFunc("* * * * *", func() {
		if cleared, err := s.store.ClearExpiredHolds(); err != nil {
			log.Printf("error clearing expired holds: %v", err)
		} else if cleared > 0 {
			log.Printf("auto-cleared %d expired holds", cleared)
			s.Reload()
		}
	})

	s.cron.Start()
	return nil
}

func (s *Scheduler) Stop() {
	s.cron.Stop()
}

func (s *Scheduler) Reload() error {
	s.Stop()
	s.cron = cron.New()
	return s.Start()
}

// RunJobNow runs a job outside its schedule. A shell job's run is returned
// finished, with its output. An agent job's run is returned as soon as it is
// created, still running: the run follows its session until the turn ends,
// which takes minutes, and the callers — the API, the bus, and the bridge's
// run-a-job button with its 30-second limit — would otherwise give up on a run
// that is going fine. Read its outcome from GET /api/jobs/{id}/runs.
// It returns nil when the job was not run: on hold, deleted, or still running.
func (s *Scheduler) RunJobNow(jobID int64) (*db.Run, error) {
	job, err := s.store.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	if job.EventTrigger != nil {
		return nil, ErrRunsOnEventTrigger
	}
	if job.Type != "agent" {
		return s.executeJob(*job), nil
	}
	runCreated := make(chan *db.Run, 1)
	go func() {
		defer s.recordJobPanic(*job)
		s.executeJobReportingRun(*job, runCreated)
	}()
	return <-runCreated, nil
}

func (s *Scheduler) addJob(j db.Job) (cron.EntryID, error) {
	job := j
	id, err := s.cron.AddFunc(job.Schedule, func() {
		defer s.recordJobPanic(job)
		s.executeJob(job)
	})
	return id, err
}

// recordJobPanic, deferred around a job's execution, logs a panic and records
// it as a failed run.
func (s *Scheduler) recordJobPanic(job db.Job) {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("panic in job %d (%s): %v", job.ID, job.Name, r)

	// Log the panic
	if s.logger != nil {
		s.logger.LogError("job_panic", fmt.Sprintf("Job %s panicked: %v", job.Name, r), map[string]interface{}{
			"job_id":   job.ID,
			"job_name": job.Name,
			"schedule": job.Schedule,
			"panic":    fmt.Sprintf("%v", r),
		})
	}

	// Try to record the panic as a failed run
	run, err := s.store.CreateRun(job.ID)
	if err == nil {
		s.store.FinishRun(run.ID, "panic", fmt.Sprintf("panic: %v", r))
	}
}

func (s *Scheduler) executeJob(job db.Job) *db.Run {
	return s.executeJobReportingRun(job, nil)
}

// executeJobReportingRun runs the job and returns its finished run. When
// runCreated is not nil it also receives the run as soon as it is created, and
// is closed when the run ends, so a job that is skipped sends nil.
func (s *Scheduler) executeJobReportingRun(job db.Job, runCreated chan<- *db.Run) *db.Run {
	if runCreated != nil {
		defer close(runCreated)
	}
	// The cron entry closes over the job as it looked when the schedule was
	// loaded, and every field of it is editable through the API. Run the row as
	// it stands NOW: reading it only for the hold check, then executing the
	// stale copy, is what made an edited command or prompt take effect no
	// earlier than the next reload.
	current, err := s.store.GetJob(job.ID)
	if err == sql.ErrNoRows {
		// Deleted between the tick and this read. Reload will drop the entry.
		log.Printf("skipping job %d (%s): job no longer exists", job.ID, job.Name)
		return nil
	}
	if err != nil {
		// Refuse rather than fall back to the stale copy — a definition we
		// cannot read is a definition we cannot claim to be running.
		log.Printf("skipping job %d (%s): cannot read current definition: %v", job.ID, job.Name, err)
		if s.logger != nil {
			s.logger.LogError("db_error", fmt.Sprintf("Failed to read job %d before running it: %v", job.ID, err), map[string]interface{}{
				"job_id":   job.ID,
				"job_name": job.Name,
			})
		}
		return nil
	}
	job = *current

	if job.IsOnHold() {
		log.Printf("skipping job %d (%s): on hold until %s", job.ID, job.Name, job.HoldUntil.Format(time.RFC3339))
		return nil
	}

	// A tick that lands while the previous run of this job is still going is
	// dropped, not stacked. Stacking is what let one slow job accumulate dozens
	// of concurrent runs until the scheduler was OOM-killed.
	if !s.tryAcquire(job.ID) {
		log.Printf("skipping job %d (%s): previous run still in flight", job.ID, job.Name)
		if s.logger != nil {
			s.logger.LogError("job_overlap", fmt.Sprintf("Job %s skipped: previous run still in flight", job.Name), map[string]interface{}{
				"job_id":   job.ID,
				"job_name": job.Name,
				"schedule": job.Schedule,
			})
		}
		return nil
	}
	defer s.release(job.ID)

	startTime := time.Now()

	run, err := s.createRun(job)
	if err != nil {
		return nil
	}
	if runCreated != nil {
		started := *run
		runCreated <- &started
	}
	return s.runAcquiredJob(job, run, job.Prompt, startTime)
}

// createRun records a new run of the job, logging a failure.
func (s *Scheduler) createRun(job db.Job) (*db.Run, error) {
	run, err := s.store.CreateRun(job.ID)
	if err != nil {
		log.Printf("create run for job %d: %v", job.ID, err)
		if s.logger != nil {
			s.logger.LogError("db_error", fmt.Sprintf("Failed to create run for job %d: %v", job.ID, err), map[string]interface{}{
				"job_id":   job.ID,
				"job_name": job.Name,
			})
		}
	}
	return run, err
}

// runAcquiredJob executes a job its caller holds in flight, with prompt in
// place of the job's own for an agent job, and finishes the run.
func (s *Scheduler) runAcquiredJob(job db.Job, run *db.Run, prompt string, startTime time.Time) *db.Run {
	// Log job start
	if s.logger != nil {
		s.logger.LogJobStart(job.ID, job.Name, job.Schedule, run.ID)
	}

	var result executor.Result
	if job.Type == "agent" {
		if s.agentExecutor == nil {
			result = executor.Result{Status: runstatus.Error, Output: "agent executor not configured"}
		} else {
			result = s.agentExecutor.Run(executor.AgentRun{
				Agent:        job.Agent,
				Prompt:       prompt,
				Orchestrator: job.Orchestrator,
				SessionID:    job.SessionID,
				WorkspaceID:  job.WorkspaceID,
				ModelRole:    job.ModelRole,
				// timeout_seconds caps how long the run follows its session.
				SessionWaitLimit: executor.AgentSessionWaitLimit(job.TimeoutSecs),
			})
		}
	} else {
		timeout := executor.DefaultTimeout
		if job.TimeoutSecs > 0 {
			timeout = time.Duration(job.TimeoutSecs) * time.Second
		}
		result = s.exec(job.Command, timeout)
	}
	duration := time.Since(startTime)

	if err := s.store.FinishRun(run.ID, result.Status, result.Output); err != nil {
		s.logRunFinishFailure(job, run, err)
	}

	run.Status = result.Status
	run.Output = result.Output
	finishTime := time.Now()
	run.FinishedAt = &finishTime

	// Log job finish
	if s.logger != nil {
		s.logger.LogJobFinish(job.ID, job.Name, job.Schedule, run.ID, result.Status, result.Output, duration)
	}

	// Publish run result to bus
	if s.busHandler != nil {
		startedAt := startTime.UTC().Format(time.RFC3339)
		finishedAt := finishTime.UTC().Format(time.RFC3339)
		s.busHandler.PublishRunResult(run, &job, startedAt, finishedAt, duration.Milliseconds())
	}

	return run
}

func (s *Scheduler) logRunFinishFailure(job db.Job, run *db.Run, err error) {
	log.Printf("finish run %d: %v", run.ID, err)
	if s.logger != nil {
		s.logger.LogError("db_error", fmt.Sprintf("Failed to finish run %d: %v", run.ID, err), map[string]interface{}{
			"run_id":   run.ID,
			"job_id":   job.ID,
			"job_name": job.Name,
		})
	}
}
