package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
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
}

// Logger interface for logging events
type Logger interface {
	LogJobCreated(jobID int64, jobName, schedule, command string)
	LogJobUpdated(jobID int64, jobName string, field string, value interface{})
	LogJobDeleted(jobID int64, jobName string)
}

func New(store *db.Store, scheduler *cronpkg.Scheduler, token string) *Server {
	s := &Server{store: store, scheduler: scheduler, token: token, mux: http.NewServeMux(), startedAt: time.Now()}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/api/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/jobs/", s.handleJobSub)
	s.mux.HandleFunc("/api/calendar/events", s.handleCalendarEvents)
	return s
}

// SetLogger sets the logger for the API server
func (s *Server) SetLogger(logger Logger) {
	s.logger = logger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
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
		"status":    "ok",
		"uptime_s":  time.Since(s.startedAt).Seconds(),
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
			Model        string `json:"model"`
			Orchestrator string `json:"orchestrator"`
			SessionID    string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid request body", err.Error())
			return
		}
		if req.Name == "" || req.Schedule == "" {
			writeError(w, 400, "name and schedule are required", "")
			return
		}
		jobType := req.Type
		if jobType == "" {
			jobType = "shell"
		}
		if jobType == "shell" && req.Command == "" {
			writeError(w, 400, "command is required for shell jobs", "")
			return
		}
		if jobType == "agent" && (req.Agent == "" || req.Prompt == "") {
			writeError(w, 400, "agent and prompt are required for agent jobs", "")
			return
		}
		if jobType != "shell" && jobType != "agent" {
			writeError(w, 400, "type must be 'shell' or 'agent'", "")
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
			Model:        req.Model,
			Orchestrator: req.Orchestrator,
			SessionID:    req.SessionID,
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
			var req struct {
				Enabled     *bool   `json:"enabled"`
				Name        *string `json:"name"`
				Description *string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, 400, "invalid request body", err.Error())
				return
			}
			if req.Description != nil {
				if err := s.store.UpdateJobDescription(id, *req.Description); err != nil {
					if err == sql.ErrNoRows {
						writeError(w, 404, "job not found", "")
						return
					}
					writeError(w, 500, "failed to update job", err.Error())
					return
				}
			}
			if req.Enabled != nil {
				// Get job info for logging before update
				currentJob, err := s.store.GetJob(id)
				if err != nil {
					if err == sql.ErrNoRows {
						writeError(w, 404, "job not found", "")
						return
					}
					writeError(w, 500, "failed to get job", err.Error())
					return
				}
				
				if err := s.store.UpdateJobEnabled(id, *req.Enabled); err != nil {
					if err == sql.ErrNoRows {
						writeError(w, 404, "job not found", "")
						return
					}
					writeError(w, 500, "failed to update job", err.Error())
					return
				}
				s.scheduler.Reload()
				
				// Log job update
				if s.logger != nil {
					s.logger.LogJobUpdated(currentJob.ID, currentJob.Name, "enabled", *req.Enabled)
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
					writeError(w, 400, "invalid 'until' format, use RFC3339", err.Error())
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
			writeError(w, 400, "invalid 'from' (use RFC3339)", err.Error())
			return
		}
		from = parsed
	}
	if v := r.URL.Query().Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, 400, "invalid 'to' (use RFC3339)", err.Error())
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
