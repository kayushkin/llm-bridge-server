package cron

import (
	"log"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
)

type Scheduler struct {
	cron  *cron.Cron
	store *db.Store
}

func New(store *db.Store) *Scheduler {
	return &Scheduler{
		cron:  cron.New(),
		store: store,
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
		s.addJob(j)
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

func (s *Scheduler) addJob(j db.Job) {
	job := j
	s.cron.AddFunc(job.Schedule, func() {
		s.executeJob(job)
	})
}

func (s *Scheduler) executeJob(job db.Job) *db.Run {
	run, err := s.store.CreateRun(job.ID)
	if err != nil {
		log.Printf("create run for job %d: %v", job.ID, err)
		return nil
	}
	result := executor.Run(job.Command, 5*time.Minute)
	if err := s.store.FinishRun(run.ID, result.Status, result.Output); err != nil {
		log.Printf("finish run %d: %v", run.ID, err)
	}
	run.Status = result.Status
	run.Output = result.Output
	return run
}
