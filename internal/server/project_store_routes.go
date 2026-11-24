package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projectstore"
)

// Projects and the session filer, moved in from the standalone project-store:
// the same routes under /project-store/, answered from the Postgres module.

// EnableProjectStore serves the store's routes from handlers. main calls it
// once the module is provisioned, migrated and open; until then, and on a
// server with no Postgres URL, the routes answer 503.
func (s *Server) EnableProjectStore(handlers *projectstore.Handlers) {
	s.projectStore.Store(handlers)
}

// projectStoreRoute answers one route with the store's handler for it.
func (s *Server) projectStoreRoute(route func(*projectstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.projectStore.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "project_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no projects")
			return
		}
		route(handlers, w, r)
	}
}

// RunProjectStoreSessionFiler runs a filing pass, then waits interval, until
// ctx ends. It is what the scheduler job that calls the standalone store's
// POST /session-filings/run does, in this process; main starts it only when
// project_store.session_filer.enabled is on. A pass that fails, or fails to
// judge some session, is logged and the next pass tries again, as the next
// run of the job would.
func (s *Server) RunProjectStoreSessionFiler(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		handlers := s.projectStore.Load()
		if handlers == nil || handlers.SessionFiler == nil {
			log.Printf("[project-store session filer] ERROR: no session filer is open, so no pass ran")
			continue
		}
		report, err := handlers.SessionFiler.Run(ctx, projectstore.SessionFilerRunRequest{})
		if err != nil {
			log.Printf("[project-store session filer] ERROR: the pass failed: %v", err)
			continue
		}
		log.Printf("[project-store session filer] pass: %d chats changed since %s, %d judged, %d failed, %d left for the next pass",
			report.ChatSessions, time.Unix(report.Since, 0).UTC().Format(time.RFC3339), len(report.Judged), len(report.Failed), report.Remaining)
		for _, failure := range report.Failed {
			log.Printf("[project-store session filer] ERROR: could not judge %s: %s", failure.SessionID, failure.Error)
		}
	}
}

// registerProjectStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerProjectStoreRoutes() {
	s.mux.HandleFunc("GET /project-store/health", s.projectStoreRoute((*projectstore.Handlers).Health))
	s.mux.HandleFunc("GET /project-store/vocabulary", s.projectStoreRoute((*projectstore.Handlers).Vocabulary))
	s.mux.HandleFunc("GET /project-store/projects", s.projectStoreRoute((*projectstore.Handlers).ListProjects))
	s.mux.HandleFunc("POST /project-store/projects", s.projectStoreRoute((*projectstore.Handlers).CreateProject))
	s.mux.HandleFunc("GET /project-store/digest", s.projectStoreRoute((*projectstore.Handlers).Digest))
	s.mux.HandleFunc("GET /project-store/projects/{id}", s.projectStoreRoute((*projectstore.Handlers).GetProject))
	s.mux.HandleFunc("PATCH /project-store/projects/{id}", s.projectStoreRoute((*projectstore.Handlers).PatchProject))
	s.mux.HandleFunc("GET /project-store/projects/{id}/rollup", s.projectStoreRoute((*projectstore.Handlers).ProjectRollup))
	s.mux.HandleFunc("GET /project-store/projects/{id}/changes", s.projectStoreRoute((*projectstore.Handlers).ProjectChanges))
	s.mux.HandleFunc("GET /project-store/projects/{id}/links", s.projectStoreRoute((*projectstore.Handlers).ProjectLinks))
	s.mux.HandleFunc("POST /project-store/projects/{id}/links", s.projectStoreRoute((*projectstore.Handlers).AddLink))
	s.mux.HandleFunc("DELETE /project-store/projects/{id}/links/{link_id}", s.projectStoreRoute((*projectstore.Handlers).DeleteLink))
	s.mux.HandleFunc("GET /project-store/links", s.projectStoreRoute((*projectstore.Handlers).FindLinks))
	s.mux.HandleFunc("GET /project-store/projects/{id}/activity", s.projectStoreRoute((*projectstore.Handlers).ProjectActivity))
	s.mux.HandleFunc("POST /project-store/projects/{id}/next-steps", s.projectStoreRoute((*projectstore.Handlers).AddNextStep))
	s.mux.HandleFunc("GET /project-store/projects/{id}/suggestions", s.projectStoreRoute((*projectstore.Handlers).ListSuggestions))
	s.mux.HandleFunc("POST /project-store/projects/{id}/suggestions", s.projectStoreRoute((*projectstore.Handlers).RequestSuggestions))
	s.mux.HandleFunc("POST /project-store/suggestions/{suggestion_id}/accept", s.projectStoreRoute((*projectstore.Handlers).AcceptSuggestion))
	s.mux.HandleFunc("POST /project-store/suggestions/{suggestion_id}/dismiss", s.projectStoreRoute((*projectstore.Handlers).DismissSuggestion))
	s.mux.HandleFunc("GET /project-store/projects/{id}/goals", s.projectStoreRoute((*projectstore.Handlers).ListProjectGoals))
	s.mux.HandleFunc("POST /project-store/projects/{id}/goals", s.projectStoreRoute((*projectstore.Handlers).CreateGoal))
	s.mux.HandleFunc("GET /project-store/goals", s.projectStoreRoute((*projectstore.Handlers).ListGoals))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}", s.projectStoreRoute((*projectstore.Handlers).GetGoal))
	s.mux.HandleFunc("PATCH /project-store/goals/{goal_id}", s.projectStoreRoute((*projectstore.Handlers).PatchGoal))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}/changes", s.projectStoreRoute((*projectstore.Handlers).GoalChanges))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}/links", s.projectStoreRoute((*projectstore.Handlers).GoalLinks))
	s.mux.HandleFunc("POST /project-store/goals/{goal_id}/links", s.projectStoreRoute((*projectstore.Handlers).AddGoalLink))
	s.mux.HandleFunc("DELETE /project-store/goals/{goal_id}/links/{link_id}", s.projectStoreRoute((*projectstore.Handlers).DeleteGoalLink))
	s.mux.HandleFunc("GET /project-store/goal-links", s.projectStoreRoute((*projectstore.Handlers).FindGoalLinks))
	s.mux.HandleFunc("GET /project-store/session-filings", s.projectStoreRoute((*projectstore.Handlers).ListSessionFilings))
	s.mux.HandleFunc("POST /project-store/session-filings/run", s.projectStoreRoute((*projectstore.Handlers).RunSessionFiler))
	s.mux.HandleFunc("POST /project-store/session-filings/{session_filing_id}/accept", s.projectStoreRoute((*projectstore.Handlers).AcceptSessionFiling))
	s.mux.HandleFunc("POST /project-store/session-filings/{session_filing_id}/dismiss", s.projectStoreRoute((*projectstore.Handlers).DismissSessionFiling))
}
