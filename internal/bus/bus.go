package bus

import (
	"encoding/json"
	"fmt"
	"log"

	natsbus "github.com/kayushkin/bus"
	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/logging"
)

// Command represents a bus command
type Command struct {
	Action       string `json:"action"`
	Name         string `json:"name,omitempty"`
	Schedule     string `json:"schedule,omitempty"`
	Command      string `json:"command,omitempty"`
	Type         string `json:"type,omitempty"`
	Agent        string `json:"agent,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Model        string `json:"model,omitempty"`
	Orchestrator string `json:"orchestrator,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	ID           int64  `json:"id,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	JobID        int64  `json:"job_id,omitempty"`
}

// RunResult represents a job run result for publishing
type RunResult struct {
	RunID      int64  `json:"run_id"`
	JobID      int64  `json:"job_id"`
	JobName    string `json:"job_name"`
	Schedule   string `json:"schedule"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	Output     string `json:"output"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Duration   int64  `json:"duration_ms,omitempty"`
}

type Handler struct {
	client    *natsbus.Client
	store     *db.Store
	scheduler *cronpkg.Scheduler
	logger    *logging.Logger
}

// NewHandler connects to natsURL. An empty natsURL gives a handler that does
// nothing; this package reads no environment variable.
func NewHandler(natsURL string, store *db.Store, scheduler *cronpkg.Scheduler, logger *logging.Logger) (*Handler, error) {
	if natsURL == "" {
		// No NATS configured, return no-op handler
		return &Handler{store: store, scheduler: scheduler, logger: logger}, nil
	}

	client, err := natsbus.Connect(natsbus.Options{
		URL:  natsURL,
		Name: "scheduler",
	})
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	return &Handler{
		client:    client,
		store:     store,
		scheduler: scheduler,
		logger:    logger,
	}, nil
}

// Start begins listening for bus commands
func (h *Handler) Start() error {
	if h.client == nil {
		return nil
	}

	_, err := h.client.Reply("scheduler.commands", h.handleRequest)
	if err != nil {
		return fmt.Errorf("subscribe scheduler.commands: %w", err)
	}

	log.Println("bus: subscribed to scheduler.commands (NATS)")
	return nil
}

// Close closes the NATS connection
func (h *Handler) Close() {
	if h.client != nil {
		h.client.Close()
	}
}

// PublishRunResult publishes a job run result to the bus
func (h *Handler) PublishRunResult(run *db.Run, job *db.Job, startedAt, finishedAt string, duration int64) {
	if h.client == nil {
		return
	}

	result := RunResult{
		RunID:      run.ID,
		JobID:      run.JobID,
		JobName:    job.Name,
		Schedule:   job.Schedule,
		Command:    job.Command,
		Status:     run.Status,
		Output:     run.Output,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Duration:   duration,
	}

	if err := h.client.Publish("scheduler.runs", result); err != nil {
		log.Printf("bus: failed to publish run result: %v", err)
	}
}

// handleRequest processes incoming NATS request/reply messages
func (h *Handler) handleRequest(data []byte) (any, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return nil, fmt.Errorf("invalid command format: %w", err)
	}

	log.Printf("bus: received command: %s", cmd.Action)

	switch cmd.Action {
	case "list_jobs":
		return h.handleListJobs()
	case "create_job":
		return h.handleCreateJob(cmd)
	case "delete_job":
		return h.handleDeleteJob(cmd)
	case "run_job":
		return h.handleRunJob(cmd)
	case "toggle_job":
		return h.handleToggleJob(cmd)
	case "list_runs":
		return h.handleListRuns(cmd)
	default:
		return nil, fmt.Errorf("unknown action: %s", cmd.Action)
	}
}

func (h *Handler) handleListJobs() (any, error) {
	jobs, err := h.store.ListJobs()
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %w", err)
	}
	if jobs == nil {
		jobs = []db.Job{}
	}
	return map[string]any{"success": true, "data": jobs}, nil
}

func (h *Handler) handleCreateJob(cmd Command) (any, error) {
	jobType := cmd.Type
	if jobType == "" {
		jobType = "shell"
	}
	if cmd.Name == "" || cmd.Schedule == "" {
		return nil, fmt.Errorf("name and schedule are required")
	}
	if jobType == "shell" && cmd.Command == "" {
		return nil, fmt.Errorf("command is required for shell jobs")
	}
	if jobType == "agent" && (cmd.Agent == "" || cmd.Prompt == "") {
		return nil, fmt.Errorf("agent and prompt are required for agent jobs")
	}

	job, err := h.store.CreateJobFull(db.Job{
		Name:         cmd.Name,
		Schedule:     cmd.Schedule,
		Command:      cmd.Command,
		Type:         jobType,
		Agent:        cmd.Agent,
		Prompt:       cmd.Prompt,
		Model:        cmd.Model,
		Orchestrator: cmd.Orchestrator,
		SessionID:    cmd.SessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	if err := h.scheduler.Reload(); err != nil {
		log.Printf("bus: failed to reload scheduler after job creation: %v", err)
	}

	h.logger.LogJobCreated(job.ID, job.Name, job.Schedule, job.Command)
	return map[string]any{"success": true, "data": job}, nil
}

func (h *Handler) handleDeleteJob(cmd Command) (any, error) {
	if cmd.ID == 0 {
		return nil, fmt.Errorf("job id is required")
	}

	job, err := h.store.GetJob(cmd.ID)
	if err != nil {
		return nil, fmt.Errorf("job not found: %w", err)
	}

	if err := h.store.DeleteJob(cmd.ID); err != nil {
		return nil, fmt.Errorf("failed to delete job: %w", err)
	}

	if err := h.scheduler.Reload(); err != nil {
		log.Printf("bus: failed to reload scheduler after job deletion: %v", err)
	}

	h.logger.LogJobDeleted(job.ID, job.Name)
	return map[string]any{"success": true, "data": map[string]string{"status": "deleted"}}, nil
}

func (h *Handler) handleRunJob(cmd Command) (any, error) {
	if cmd.ID == 0 {
		return nil, fmt.Errorf("job id is required")
	}

	run, err := h.scheduler.RunJobNow(cmd.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to run job: %w", err)
	}

	return map[string]any{"success": true, "data": run}, nil
}

func (h *Handler) handleToggleJob(cmd Command) (any, error) {
	if cmd.ID == 0 {
		return nil, fmt.Errorf("job id is required")
	}
	if cmd.Enabled == nil {
		return nil, fmt.Errorf("enabled field is required")
	}

	job, err := h.store.GetJob(cmd.ID)
	if err != nil {
		return nil, fmt.Errorf("job not found: %w", err)
	}

	if err := h.store.UpdateJob(cmd.ID, db.JobPatch{Enabled: cmd.Enabled}); err != nil {
		return nil, fmt.Errorf("failed to update job: %w", err)
	}

	if err := h.scheduler.Reload(); err != nil {
		log.Printf("bus: failed to reload scheduler after job toggle: %v", err)
	}

	h.logger.LogJobUpdated(job.ID, job.Name, "enabled", *cmd.Enabled)

	updatedJob, _ := h.store.GetJob(cmd.ID)
	return map[string]any{"success": true, "data": updatedJob}, nil
}

func (h *Handler) handleListRuns(cmd Command) (any, error) {
	if cmd.JobID == 0 {
		return nil, fmt.Errorf("job_id is required")
	}

	runs, err := h.store.ListRuns(cmd.JobID)
	if err != nil {
		return nil, fmt.Errorf("failed to list runs: %w", err)
	}
	if runs == nil {
		runs = []db.Run{}
	}
	return map[string]any{"success": true, "data": runs}, nil
}
