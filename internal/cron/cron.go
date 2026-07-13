package cron

import (
	"fmt"
	"log"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
)

type Scheduler struct {
	cron          *cron.Cron
	store         *db.Store
	exec          func(command string, timeout time.Duration) executor.Result
	agentExecutor *executor.AgentExecutor
	logger        Logger
	busHandler    BusHandler
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
		cron:  cron.New(),
		store: store,
		exec:  executor.Run,
	}
}

// NewWithExecutor creates a scheduler with a custom executor (for testing).
func NewWithExecutor(store *db.Store, execFn func(string, time.Duration) executor.Result) *Scheduler {
	return &Scheduler{
		cron:  cron.New(),
		store: store,
		exec:  execFn,
	}
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
		if !j.Enabled {
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

func (s *Scheduler) RunJobNow(jobID int64) (*db.Run, error) {
	job, err := s.store.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	return s.executeJob(*job), nil
}

func (s *Scheduler) addJob(j db.Job) (cron.EntryID, error) {
	job := j
	id, err := s.cron.AddFunc(job.Schedule, func() {
		defer func() {
			if r := recover(); r != nil {
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
		}()
		s.executeJob(job)
	})
	return id, err
}

func (s *Scheduler) executeJob(job db.Job) *db.Run {
	// Re-fetch job to check current hold status
	current, err := s.store.GetJob(job.ID)
	if err == nil && current.IsOnHold() {
		log.Printf("skipping job %d (%s): on hold until %s", job.ID, job.Name, current.HoldUntil.Format(time.RFC3339))
		return nil
	}

	startTime := time.Now()
	
	run, err := s.store.CreateRun(job.ID)
	if err != nil {
		log.Printf("create run for job %d: %v", job.ID, err)
		if s.logger != nil {
			s.logger.LogError("db_error", fmt.Sprintf("Failed to create run for job %d: %v", job.ID, err), map[string]interface{}{
				"job_id":   job.ID,
				"job_name": job.Name,
			})
		}
		return nil
	}
	
	// Log job start
	if s.logger != nil {
		s.logger.LogJobStart(job.ID, job.Name, job.Schedule, run.ID)
	}
	
	var result executor.Result
	if job.Type == "agent" {
		if s.agentExecutor == nil {
			result = executor.Result{Status: "error", Output: "agent executor not configured"}
		} else {
			result = s.agentExecutor.Run(executor.AgentRun{
				Agent:        job.Agent,
				Prompt:       job.Prompt,
				Orchestrator: job.Orchestrator,
				SessionID:    job.SessionID,
				WorkspaceID:  job.WorkspaceID,
			})
		}
	} else {
		result = s.exec(job.Command, 5*time.Minute)
	}
	duration := time.Since(startTime)
	
	if err := s.store.FinishRun(run.ID, result.Status, result.Output); err != nil {
		log.Printf("finish run %d: %v", run.ID, err)
		if s.logger != nil {
			s.logger.LogError("db_error", fmt.Sprintf("Failed to finish run %d: %v", run.ID, err), map[string]interface{}{
				"run_id":   run.ID,
				"job_id":   job.ID,
				"job_name": job.Name,
			})
		}
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
