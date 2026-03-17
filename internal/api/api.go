package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
)

type Server struct {
	store     *db.Store
	scheduler *cronpkg.Scheduler
	token     string
	mux       *http.ServeMux
}

func New(store *db.Store, scheduler *cronpkg.Scheduler, token string) *Server {
	s := &Server{store: store, scheduler: scheduler, token: token, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/jobs/", s.handleJobSub)
	s.mux.HandleFunc("/api/calendar/events", s.handleCalendarEvents)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		return
	}
	if s.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := s.store.ListJobs()
		if err != nil {
			http.Error(w, err.Error(), 500)
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
			http.Error(w, err.Error(), 400)
			return
		}
		job, err := s.store.CreateJob(req.Name, req.Schedule, req.Command)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.scheduler.Reload()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, job)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleJobSub(w http.ResponseWriter, r *http.Request) {
	// paths: /api/jobs/{id}, /api/jobs/{id}/runs, /api/jobs/{id}/run
	path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.SplitN(path, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}

	if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			if err := s.store.DeleteJob(id); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, "not found", 404)
					return
				}
				http.Error(w, err.Error(), 500)
				return
			}
			s.scheduler.Reload()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "method not allowed", 405)
		return
	}

	switch parts[1] {
	case "runs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		runs, err := s.store.ListRuns(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if runs == nil {
			runs = []db.Run{}
		}
		writeJSON(w, runs)
	case "run":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		run, err := s.scheduler.RunJobNow(id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "not found", 404)
				return
			}
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, run)
	default:
		http.Error(w, "not found", 404)
	}
}

func (s *Server) handleCalendarEvents(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
