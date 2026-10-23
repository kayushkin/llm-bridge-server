package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/robfig/cron/v3"
	"github.com/slava/scheduler/internal/boundedtext"
	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
)

// APIError is a structured JSON error response.
type APIError struct {
	Error   string `json:"error"`
	Status  int    `json:"status"`
	Details string `json:"details,omitempty"`
}

type Server struct {
	store     *db.Store
	scheduler *cronpkg.Scheduler
	token     string
	mux       *http.ServeMux
	startedAt time.Time
	logger    Logger
	// checkModelRole asks model-store whether an agent job's model_role
	// exists. Its error wraps servicesettings.ErrSettingOwnerUnavailable when
	// model-store could not be asked.
	checkModelRole func(role string) error
}

// Logger interface for logging events
type Logger interface {
	LogJobCreated(jobID int64, jobName, schedule, command string)
	LogJobUpdated(jobID int64, jobName string, field string, value interface{})
	LogJobDeleted(jobID int64, jobName string)
}

// New serves the API. settings is the service's settings registry, described at
// GET /settings behind the same bearer as /api/*; it is required, so no second
// caller of New can forget the route. checkModelRole is required too: POST and
// PATCH take an agent job's model_role, and one nobody checked would start no
// session when the job ran.
func New(store *db.Store, scheduler *cronpkg.Scheduler, token string, settings *servicesettings.Registry, checkModelRole func(role string) error) *Server {
	if settings == nil {
		panic("api.New: a nil settings registry; the service must describe its settings at GET /settings")
	}
	if checkModelRole == nil {
		panic("api.New: a nil model-role checker; an agent job's model_role must be checked with model-store")
	}
	s := &Server{store: store, scheduler: scheduler, token: token, mux: http.NewServeMux(), startedAt: time.Now(), checkModelRole: checkModelRole}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/api/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/jobs/", s.handleJobSub)
	s.mux.HandleFunc("GET /api/runs/latest", s.handleLatestRuns)
	s.mux.HandleFunc("/api/calendar/events", s.handleCalendarEvents)
	// At the root, not under /api: dash sends /api/scheduler/settings here, as
	// it does for every service's settings. GET describes every setting; PUT
	// /settings/{key} changes one the service stores (the job binaries' model
	// roles) and is refused for the rest.
	settingsHandler := servicesettings.Handler(settings, "/settings")
	s.mux.Handle("GET /settings", settingsHandler)
	s.mux.Handle("PUT /settings/{key}", settingsHandler)
	return s
}

// SetLogger sets the logger for the API server
func (s *Server) SetLogger(logger Logger) {
	s.logger = logger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		return
	}
	// Health endpoint doesn't require auth
	if r.URL.Path == "/health" {
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	writeJSON(w, map[string]interface{}{
		"status":     "ok",
		"uptime_s":   time.Since(s.startedAt).Seconds(),
		"started_at": s.startedAt.UTC(),
	})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := s.store.ListJobs()
		if err != nil {
			writeError(w, 500, "failed to list jobs", err.Error())
			return
		}
		if jobs == nil {
			jobs = []db.Job{}
		}
		writeJSON(w, jobs)
	case http.MethodPost:
		var req struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			Schedule     string `json:"schedule"`
			Command      string `json:"command"`
			Type         string `json:"type"`
			Agent        string `json:"agent"`
			Prompt       string `json:"prompt"`
			ModelRole    string `json:"model_role"`
			Orchestrator string `json:"orchestrator"`
			SessionID    string `json:"session_id"`
			WorkspaceID  string `json:"workspace_id"`
			TimeoutSecs  int    `json:"timeout_seconds"`
			// Pointer, for the same reason jobPatchRequest.Enabled is one: a
			// plain bool cannot tell "enabled: false" from "field absent", and
			// those must mean different things here. Absent means true, so no
			// existing caller changes behaviour; false means create it off.
			Enabled *bool `json:"enabled"`
		}
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, 400, "invalid request body", err.Error())
			return
		}
		jobType := req.Type
		if jobType == "" {
			jobType = db.DefaultJobType
		}
		if problem := db.ValidateJob(db.Job{
			Name:        req.Name,
			Schedule:    req.Schedule,
			Command:     req.Command,
			Type:        jobType,
			Agent:       req.Agent,
			Prompt:      req.Prompt,
			ModelRole:   req.ModelRole,
			TimeoutSecs: req.TimeoutSecs,
		}); problem != "" {
			writeError(w, 400, problem, "")
			return
		}
		if !s.modelRoleIsKnown(w, req.ModelRole) {
			return
		}
		job, err := s.store.CreateJobFull(db.Job{
			Name:         req.Name,
			Description:  req.Description,
			Schedule:     req.Schedule,
			Command:      req.Command,
			Type:         jobType,
			Agent:        req.Agent,
			Prompt:       req.Prompt,
			ModelRole:    req.ModelRole,
			Orchestrator: req.Orchestrator,
			SessionID:    req.SessionID,
			WorkspaceID:  req.WorkspaceID,
			TimeoutSecs:  req.TimeoutSecs,
			Enabled:      db.EnabledOnCreate(req.Enabled),
		})
		if err != nil {
			writeError(w, 500, "failed to create job", err.Error())
			return
		}
		s.scheduler.Reload()

		// Log job creation
		if s.logger != nil {
			cmd := job.Command
			if job.Type == "agent" {
				cmd = "agent:" + job.Agent
			}
			s.logger.LogJobCreated(job.ID, job.Name, job.Schedule, cmd)
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, job)
	default:
		writeError(w, 405, "method not allowed", "")
	}
}

// modelRoleIsKnown checks an agent job's model_role with model-store and
// writes the refusal when it fails: 400 for a role model-store does not know,
// 502 when model-store could not be asked. An empty role is known: the job
// then takes the agent_job_model_role setting.
func (s *Server) modelRoleIsKnown(w http.ResponseWriter, role string) bool {
	if role == "" {
		return true
	}
	err := s.checkModelRole(role)
	if err == nil {
		return true
	}
	if errors.Is(err, servicesettings.ErrSettingOwnerUnavailable) {
		writeError(w, http.StatusBadGateway, "model_role could not be checked", err.Error())
		return false
	}
	writeError(w, http.StatusBadRequest, "model_role is not a model-store role", err.Error())
	return false
}

func (s *Server) handleJobSub(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.SplitN(path, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeError(w, 400, "invalid job id", "")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			job, err := s.store.GetJob(id)
			if err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to get job", err.Error())
				return
			}
			writeJSON(w, job)
		case http.MethodPatch:
			var req jobPatchRequest
			if err := decodeStrict(r, &req); err != nil {
				writeError(w, 400, "invalid request body", err.Error())
				return
			}
			currentJob, err := s.store.GetJob(id)
			if err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to get job", err.Error())
				return
			}
			patch := req.toJobPatch()
			// Validate the job the patch would PRODUCE, not the patch itself.
			// Clearing a shell job's command is only invalid in combination with
			// the type it already has, and a patch read on its own cannot see that.
			if problem := db.ValidateJob(applyJobPatch(*currentJob, patch)); problem != "" {
				writeError(w, 400, problem, "")
				return
			}
			if patch.ModelRole != nil && !s.modelRoleIsKnown(w, *patch.ModelRole) {
				return
			}
			changes := patch.Changes()
			if len(changes) > 0 {
				if err := s.store.UpdateJob(id, patch); err != nil {
					if err == sql.ErrNoRows {
						writeError(w, 404, "job not found", "")
						return
					}
					writeError(w, 500, "failed to update job", err.Error())
					return
				}
				// Every field is reachable now, and the cron closures hold a copy
				// of the job taken at load time — so reload after ANY change, not
				// just after enabled. Reloading only on enabled is what made a
				// written-through prompt sit unused until the next restart.
				s.scheduler.Reload()

				if s.logger != nil {
					for _, change := range changes {
						s.logger.LogJobUpdated(currentJob.ID, currentJob.Name, change.Column, change.Value)
					}
				}
			}
			job, err := s.store.GetJob(id)
			if err != nil {
				writeError(w, 500, "failed to get job", err.Error())
				return
			}
			writeJSON(w, job)
		case http.MethodDelete:
			// Get job info for logging before deletion
			jobToDelete, err := s.store.GetJob(id)
			if err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to get job", err.Error())
				return
			}

			if err := s.store.DeleteJob(id); err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to delete job", err.Error())
				return
			}
			s.scheduler.Reload()

			// Log job deletion
			if s.logger != nil {
				s.logger.LogJobDeleted(jobToDelete.ID, jobToDelete.Name)
			}

			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, 405, "method not allowed", "")
		}
		return
	}

	switch parts[1] {
	case "hold":
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Days  *int    `json:"days"`
				Until *string `json:"until"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, 400, "invalid request body", err.Error())
				return
			}
			var holdUntil time.Time
			if req.Until != nil {
				parsed, err := time.Parse(time.RFC3339, *req.Until)
				if err != nil {
					// Bounded: *req.Until is caller-supplied and time.Parse
					// quotes its whole input back inside its own message,
					// twice, so a 5 KB until answered with a 10 KB body.
					writeError(w, 400, "invalid 'until' format, use RFC3339", boundedtext.ErrorMessage(err))
					return
				}
				holdUntil = parsed
			} else if req.Days != nil && *req.Days > 0 {
				holdUntil = time.Now().Add(time.Duration(*req.Days) * 24 * time.Hour)
			} else {
				writeError(w, 400, "provide 'days' (positive int) or 'until' (RFC3339)", "")
				return
			}
			if holdUntil.Before(time.Now()) {
				writeError(w, 400, "hold time must be in the future", "")
				return
			}
			if err := s.store.SetHold(id, holdUntil); err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to set hold", err.Error())
				return
			}
			s.scheduler.Reload()
			job, _ := s.store.GetJob(id)
			writeJSON(w, job)
		case http.MethodDelete:
			if err := s.store.ClearHold(id); err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to clear hold", err.Error())
				return
			}
			s.scheduler.Reload()
			job, _ := s.store.GetJob(id)
			writeJSON(w, job)
		default:
			writeError(w, 405, "method not allowed", "")
		}
		return
	case "runs":
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed", "")
			return
		}
		runs, err := s.store.ListRuns(id)
		if err != nil {
			writeError(w, 500, "failed to list runs", err.Error())
			return
		}
		if runs == nil {
			runs = []db.Run{}
		}
		writeJSON(w, runs)
	case "run":
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed", "")
			return
		}
		run, err := s.scheduler.RunJobNow(id)
		if err != nil {
			if err == sql.ErrNoRows {
				writeError(w, 404, "job not found", "")
				return
			}
			writeError(w, 500, "failed to run job", err.Error())
			return
		}
		writeJSON(w, run)
	default:
		writeError(w, 404, "not found", "")
	}
}

// handleCalendarEvents returns every scheduled occurrence of every enabled job
// whose cron expression fires between ?from and ?to (both RFC3339). This is the
// single source of truth for populating a calendar view with cron jobs — the
// Job record holds only the cron expression, so we expand it here with the
// same parser robfig/cron uses at runtime.
func (s *Server) handleCalendarEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}

	now := time.Now()
	from := now
	to := now.Add(7 * 24 * time.Hour)

	if v := r.URL.Query().Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, 400, "invalid 'from' (use RFC3339)", boundedtext.ErrorMessage(err))
			return
		}
		from = parsed
	}
	if v := r.URL.Query().Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, 400, "invalid 'to' (use RFC3339)", boundedtext.ErrorMessage(err))
			return
		}
		to = parsed
	}
	if !to.After(from) {
		writeError(w, 400, "'to' must be after 'from'", "")
		return
	}

	jobs, err := s.store.ListJobs()
	if err != nil {
		writeError(w, 500, "failed to list jobs", err.Error())
		return
	}

	type occurrence struct {
		ID          string    `json:"id"`
		JobID       int64     `json:"job_id"`
		Name        string    `json:"name"`
		Description string    `json:"description,omitempty"`
		Schedule    string    `json:"schedule"`
		Type        string    `json:"type"`
		Start       time.Time `json:"start"`
	}

	out := make([]occurrence, 0)
	const maxPerJob = 500
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		sched, err := cron.ParseStandard(j.Schedule)
		if err != nil {
			continue
		}
		// Skip occurrences before the job's hold expires.
		windowStart := from
		if j.HoldUntil != nil && j.HoldUntil.After(windowStart) {
			windowStart = *j.HoldUntil
		}
		// Next() returns the *next* activation strictly after its argument,
		// so step back one second to include a fire at exactly windowStart.
		next := sched.Next(windowStart.Add(-time.Second))
		for i := 0; i < maxPerJob && !next.IsZero() && !next.After(to); i++ {
			out = append(out, occurrence{
				ID:          fmt.Sprintf("sched-%d-%d", j.ID, next.Unix()),
				JobID:       j.ID,
				Name:        j.Name,
				Description: j.Description,
				Schedule:    j.Schedule,
				Type:        j.Type,
				Start:       next,
			})
			next = sched.Next(next)
		}
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, details string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Error: msg, Status: status, Details: details})
}

// handleLatestRuns answers each job's newest run without its output, so a
// page showing how every job last ended reads one small answer instead of a
// hundred runs with output for each job.
func (s *Server) handleLatestRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.LatestRuns()
	if err != nil {
		writeError(w, 500, "failed to list the latest runs", err.Error())
		return
	}
	writeJSON(w, runs)
}
