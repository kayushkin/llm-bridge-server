package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

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
}

func New(store *db.Store, scheduler *cronpkg.Scheduler, token string) *Server {
	s := &Server{store: store, scheduler: scheduler, token: token, mux: http.NewServeMux(), startedAt: time.Now()}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/api/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/jobs/", s.handleJobSub)
	s.mux.HandleFunc("/api/calendar/events", s.handleCalendarEvents)
	return s
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
			Name     string `json:"name"`
			Schedule string `json:"schedule"`
			Command  string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid request body", err.Error())
			return
		}
		if req.Name == "" || req.Schedule == "" || req.Command == "" {
			writeError(w, 400, "name, schedule, and command are required", "")
			return
		}
		job, err := s.store.CreateJob(req.Name, req.Schedule, req.Command)
		if err != nil {
			writeError(w, 500, "failed to create job", err.Error())
			return
		}
		s.scheduler.Reload()
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
				Enabled *bool   `json:"enabled"`
				Name    *string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, 400, "invalid request body", err.Error())
				return
			}
			if req.Enabled != nil {
				if err := s.store.UpdateJobEnabled(id, *req.Enabled); err != nil {
					if err == sql.ErrNoRows {
						writeError(w, 404, "job not found", "")
						return
					}
					writeError(w, 500, "failed to update job", err.Error())
					return
				}
				s.scheduler.Reload()
			}
			job, err := s.store.GetJob(id)
			if err != nil {
				writeError(w, 500, "failed to get job", err.Error())
				return
			}
			writeJSON(w, job)
		case http.MethodDelete:
			if err := s.store.DeleteJob(id); err != nil {
				if err == sql.ErrNoRows {
					writeError(w, 404, "job not found", "")
					return
				}
				writeError(w, 500, "failed to delete job", err.Error())
				return
			}
			s.scheduler.Reload()
			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, 405, "method not allowed", "")
		}
		return
	}

	switch parts[1] {
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

func (s *Server) handleCalendarEvents(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "calendar integration not yet implemented", "")
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
