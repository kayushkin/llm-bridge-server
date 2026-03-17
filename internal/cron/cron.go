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
	cron  *cron.Cron
	store *db.Store
	exec  func(command string, timeout time.Duration) executor.Result
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

func (s *Scheduler) Start() error {
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
	run, err := s.store.CreateRun(job.ID)
	if err != nil {
		log.Printf("create run for job %d: %v", job.ID, err)
		return nil
	}
	result := s.exec(job.Command, 5*time.Minute)
	if err := s.store.FinishRun(run.ID, result.Status, result.Output); err != nil {
		log.Printf("finish run %d: %v", run.ID, err)
	}
	run.Status = result.Status
	run.Output = result.Output
	return run
}
