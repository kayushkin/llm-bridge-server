package server

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	schedulerapi "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/api"
	schedulerdb "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
)

// The scheduler and rule-store run in this process from their Postgres
// modules. Their routes are mounted under /scheduler/ and /rules/ with
// the paths the standalone services answered, and each request reaches the
// store's own handler with the prefix taken off. This server's scheduler job
// actions, tool-store's scheduler tool, project-store's job links and
// reference-store's scheduler_job references ask the scheduler in process.

// automationStores holds the two stores' handlers once main has opened their
// modules; until then their routes answer 503.
type automationStores struct {
	scheduler atomic.Pointer[http.Handler]
	jobs      atomic.Pointer[schedulerJobs]
	ruleStore atomic.Pointer[http.Handler]
}

const (
	schedulerMountPrefix = "/scheduler"
	rulesMountPrefix     = "/rules"
)

// schedulerJobs is the scheduler's jobs as this server reads and runs them,
// in process: schedulerapi.Server, or a fake in a test. Each refuses as its
// route does (schedulerapi.ErrJobNotFound for the route's 404).
type schedulerJobs interface {
	Job(id int64) (*schedulerdb.Job, error)
	RunJobNow(id int64) (*schedulerdb.Run, error)
}

// EnableScheduler serves the scheduler's routes from scheduler, at the paths
// without the /scheduler prefix, and reads and runs its jobs in process.
func (s *Server) EnableScheduler(scheduler *schedulerapi.Server) {
	s.enableSchedulerParts(scheduler.Routes(), scheduler)
}

func (s *Server) enableSchedulerParts(routes http.Handler, jobs schedulerJobs) {
	s.automationStores.jobs.Store(&jobs)
	s.automationStores.scheduler.Store(&routes)
}

// schedulerJobsInProcess is the scheduler's jobs, or nil while its module is
// not open.
func (s *Server) schedulerJobsInProcess() schedulerJobs {
	jobs := s.automationStores.jobs.Load()
	if jobs == nil {
		return nil
	}
	return *jobs
}

// EnableRules serves rule-store's routes from routes, which answer the
// paths without the /rules prefix.
func (s *Server) EnableRules(routes http.Handler) {
	s.automationStores.ruleStore.Store(&routes)
}

// schedulerHandler answers a scheduler path without the prefix, or 503 while
// the module is not open.
func (s *Server) schedulerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes := s.automationStores.scheduler.Load()
		if routes == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "scheduler_off", "this server's scheduler module is not open")
			return
		}
		(*routes).ServeHTTP(w, r)
	})
}

func (s *Server) ruleStoreHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes := s.automationStores.ruleStore.Load()
		if routes == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "rule_store_off", "this server's rule-store module is not open")
			return
		}
		(*routes).ServeHTTP(w, r)
	})
}

// SchedulerInProcessBaseURL is the base URL SchedulerHTTPClient's requests
// name: the scheduler's own paths go after it, as they went after the
// standalone scheduler's address.
const SchedulerInProcessBaseURL = inprocesshttp.BaseURL

// SchedulerHTTPClient carries a call to the scheduler to its handler in this
// process, with no socket and no credential between. Only tool-store's
// scheduler tool uses it: that tool is a client of the scheduler's whole job
// API (list, create, update, delete, runs), written against its routes.
func (s *Server) SchedulerHTTPClient() *http.Client {
	return inprocesshttp.Client(s.schedulerHandler())
}

func (s *Server) schedulerRoute() http.HandlerFunc {
	return storeRouteWithoutPrefix(schedulerMountPrefix, s.schedulerHandler)
}

func (s *Server) ruleStoreRoute() http.HandlerFunc {
	return storeRouteWithoutPrefix(rulesMountPrefix, s.ruleStoreHandler)
}

// registerAutomationRoutes mounts the two stores. The patterns are written
// out, because the route classification test reads them from string literals.
func (s *Server) registerAutomationRoutes() {
	s.mux.HandleFunc("GET /scheduler/health", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/jobs", s.schedulerRoute())
	s.mux.HandleFunc("POST /scheduler/api/jobs", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/jobs/{id}", s.schedulerRoute())
	s.mux.HandleFunc("PATCH /scheduler/api/jobs/{id}", s.schedulerRoute())
	s.mux.HandleFunc("DELETE /scheduler/api/jobs/{id}", s.schedulerRoute())
	s.mux.HandleFunc("POST /scheduler/api/jobs/{id}/hold", s.schedulerRoute())
	s.mux.HandleFunc("DELETE /scheduler/api/jobs/{id}/hold", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/jobs/{id}/runs", s.schedulerRoute())
	s.mux.HandleFunc("POST /scheduler/api/jobs/{id}/run", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/runs/latest", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/job-references/{reference}", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/event-trigger-subjects", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/api/calendar/events", s.schedulerRoute())
	s.mux.HandleFunc("GET /scheduler/settings", s.schedulerRoute())
	s.mux.HandleFunc("PUT /scheduler/settings/{key}", s.schedulerRoute())
	s.registerRuleStoreRoutes()
}

func (s *Server) registerRuleStoreRoutes() {
	s.mux.HandleFunc("GET /rules/health", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/vocabulary", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/rules", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/rules/{id}", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules/{id}/revisions", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules/{id}/activate", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules/{id}/pause", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules/{id}/retire", s.ruleStoreRoute())
	s.mux.HandleFunc("POST /rules/rules/{id}/preview", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/rules/{id}/comparison", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/runs", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/subjects/{type}/{id}", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/activity", s.ruleStoreRoute())
	s.mux.HandleFunc("GET /rules/summary", s.ruleStoreRoute())
}

// HTTPClientToThisServer answers a client that carries a call to any route
// of this server to its handler in this process, as the internal service, and
// the base URL its requests name. Every credential a request carries is
// replaced by this server's service token, as the standalone scheduler and
// rule-store presented it. A timeout of 0 sets none, for a stream.
func (s *Server) HTTPClientToThisServer(timeout time.Duration) (string, *http.Client) {
	asInternalService := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Clone(r.Context())
		request.Header.Del("Authorization")
		request.Header.Del(principalIdentityHeader)
		request.Header.Set(serviceTokenHeader, s.cfg.ServiceToken)
		s.ServeHTTP(w, request)
	})
	client := inprocesshttp.Client(asInternalService)
	client.Timeout = timeout
	return inprocesshttp.BaseURL, client
}
