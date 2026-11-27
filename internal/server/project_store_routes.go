package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
)

// Projects and the session filer, moved in from the standalone project-store:
// the same routes under /project-store/, answered from the Postgres module.

// EnableProjectStore serves the store's routes from handlers. main calls it
// once the module is provisioned, migrated and open; until then, and on a
// server with no Postgres URL, the routes answer 503.
func (s *Server) EnableProjectStore(handlers *projects.Handlers) {
	s.projectStore.Store(handlers)
}

// projectStoreRoute answers one route with the store's handler for it.
func (s *Server) projectStoreRoute(route func(*projects.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
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
		report, err := handlers.SessionFiler.Run(ctx, projects.SessionFilerRunRequest{})
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
	s.mux.HandleFunc("GET /project-store/health", s.projectStoreRoute((*projects.Handlers).Health))
	s.mux.HandleFunc("GET /project-store/vocabulary", s.projectStoreRoute((*projects.Handlers).Vocabulary))
	s.mux.HandleFunc("GET /project-store/projects", s.projectStoreRoute((*projects.Handlers).ListProjects))
	s.mux.HandleFunc("POST /project-store/projects", s.projectStoreRoute((*projects.Handlers).CreateProject))
	s.mux.HandleFunc("GET /project-store/digest", s.projectStoreRoute((*projects.Handlers).Digest))
	s.mux.HandleFunc("GET /project-store/projects/{id}", s.projectStoreRoute((*projects.Handlers).GetProject))
	s.mux.HandleFunc("PATCH /project-store/projects/{id}", s.projectStoreRoute((*projects.Handlers).PatchProject))
	s.mux.HandleFunc("GET /project-store/projects/{id}/rollup", s.projectStoreRoute((*projects.Handlers).ProjectRollup))
	s.mux.HandleFunc("GET /project-store/projects/{id}/changes", s.projectStoreRoute((*projects.Handlers).ProjectChanges))
	s.mux.HandleFunc("GET /project-store/projects/{id}/links", s.projectStoreRoute((*projects.Handlers).ProjectLinks))
	s.mux.HandleFunc("POST /project-store/projects/{id}/links", s.projectStoreRoute((*projects.Handlers).AddLink))
	s.mux.HandleFunc("DELETE /project-store/projects/{id}/links/{link_id}", s.projectStoreRoute((*projects.Handlers).DeleteLink))
	s.mux.HandleFunc("GET /project-store/links", s.projectStoreRoute((*projects.Handlers).FindLinks))
	s.mux.HandleFunc("GET /project-store/projects/{id}/activity", s.projectStoreRoute((*projects.Handlers).ProjectActivity))
	s.mux.HandleFunc("POST /project-store/projects/{id}/next-steps", s.projectStoreRoute((*projects.Handlers).AddNextStep))
	s.mux.HandleFunc("GET /project-store/projects/{id}/suggestions", s.projectStoreRoute((*projects.Handlers).ListSuggestions))
	s.mux.HandleFunc("POST /project-store/projects/{id}/suggestions", s.projectStoreRoute((*projects.Handlers).RequestSuggestions))
	s.mux.HandleFunc("POST /project-store/suggestions/{suggestion_id}/accept", s.projectStoreRoute((*projects.Handlers).AcceptSuggestion))
	s.mux.HandleFunc("POST /project-store/suggestions/{suggestion_id}/dismiss", s.projectStoreRoute((*projects.Handlers).DismissSuggestion))
	s.mux.HandleFunc("GET /project-store/projects/{id}/goals", s.projectStoreRoute((*projects.Handlers).ListProjectGoals))
	s.mux.HandleFunc("POST /project-store/projects/{id}/goals", s.projectStoreRoute((*projects.Handlers).CreateGoal))
	s.mux.HandleFunc("GET /project-store/goals", s.projectStoreRoute((*projects.Handlers).ListGoals))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}", s.projectStoreRoute((*projects.Handlers).GetGoal))
	s.mux.HandleFunc("PATCH /project-store/goals/{goal_id}", s.projectStoreRoute((*projects.Handlers).PatchGoal))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}/changes", s.projectStoreRoute((*projects.Handlers).GoalChanges))
	s.mux.HandleFunc("GET /project-store/goals/{goal_id}/links", s.projectStoreRoute((*projects.Handlers).GoalLinks))
	s.mux.HandleFunc("POST /project-store/goals/{goal_id}/links", s.projectStoreRoute((*projects.Handlers).AddGoalLink))
	s.mux.HandleFunc("DELETE /project-store/goals/{goal_id}/links/{link_id}", s.projectStoreRoute((*projects.Handlers).DeleteGoalLink))
	s.mux.HandleFunc("GET /project-store/goal-links", s.projectStoreRoute((*projects.Handlers).FindGoalLinks))
	s.mux.HandleFunc("GET /project-store/session-filings", s.projectStoreRoute((*projects.Handlers).ListSessionFilings))
	s.mux.HandleFunc("POST /project-store/session-filings/run", s.projectStoreRoute((*projects.Handlers).RunSessionFiler))
	s.mux.HandleFunc("POST /project-store/session-filings/{session_filing_id}/accept", s.projectStoreRoute((*projects.Handlers).AcceptSessionFiling))
	s.mux.HandleFunc("POST /project-store/session-filings/{session_filing_id}/dismiss", s.projectStoreRoute((*projects.Handlers).DismissSessionFiling))
}
