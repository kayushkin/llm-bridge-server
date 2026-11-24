package server

import (
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/agents/agentstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/modelstore"
	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
)

// agent-store, memory-store and model-store run in this process from their
// Postgres modules. agent-store's and memory-store's routes stay where the
// embedded copies always served them, at the root. What only the standalone
// services served is mounted here: model-store's registry routes under
// /model-store/, and agent-store's four seed routes for runners under
// /api/agent-store/, which until the move forwarded any path to the
// standalone agent-store.

// agentStoreHandlerHooks are the callbacks agent-store's routes run when a
// tracked file is saved, a scan ends or prompt drift is found: connected
// runners are told to reconcile, and held drifts are sent to be tagged.
func (s *Server) agentStoreHandlerHooks() agentstore.HandlerHooks {
	return agentstore.HandlerHooks{
		OnFileSaved: func(f *agentstore.TrackedFile, v *agentstore.TrackedFileVersion) {
			s.broadcastSeedSnapshot(runnerprotocol.SeedSourceAgentStore, "save")
		},
		OnScanCompleted: func(_ *agentstore.ScanResult) {
			s.broadcastSeedSnapshot(runnerprotocol.SeedSourceAgentStore, "scan")
		},
		OnPromptDriftsDetected: s.onPromptDriftsDetected,
	}
}

// modelStoreRoute answers one model-store route from the registry.
func (s *Server) modelStoreRoute(route func(*modelstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.modelStore == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "model_store_off", "this server was built without a model registry")
			return
		}
		route(modelstore.NewHandlers(s.modelStore), w, r)
	}
}

// agentStoreRunnerSeedRoute answers one of agent-store's seed routes for a
// runner: only a request carrying a known runner's bearer token reaches the
// store, which sees the path without /api/agent-store, as the standalone
// service did.
func (s *Server) agentStoreRunnerSeedRoute(agentStoreRoutes http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeRunnerRequest(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if agentStoreRoutes == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "agent_store_off", "this server was built without agent-store")
			return
		}
		inner := r.Clone(r.Context())
		inner.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/agent-store")
		inner.URL.RawPath = ""
		agentStoreRoutes.ServeHTTP(w, inner)
	}
}

// registerAgentsRegistryRoutes mounts the routes above. The patterns are
// written out, because the route classification test reads them from string
// literals.
func (s *Server) registerAgentsRegistryRoutes() {
	s.mux.HandleFunc("GET /model-store/api/models", s.modelStoreRoute((*modelstore.Handlers).ListModels))
	s.mux.HandleFunc("POST /model-store/api/models/toggle", s.modelStoreRoute((*modelstore.Handlers).ToggleModel))
	s.mux.HandleFunc("GET /model-store/api/roles", s.modelStoreRoute((*modelstore.Handlers).ListRoles))
	s.mux.HandleFunc("POST /model-store/api/roles", s.modelStoreRoute((*modelstore.Handlers).SetRole))
	s.mux.HandleFunc("GET /model-store/api/roles/{role}", s.modelStoreRoute((*modelstore.Handlers).GetRole))
	s.mux.HandleFunc("GET /model-store/api/roles/{role}/models", s.modelStoreRoute((*modelstore.Handlers).GetRoleModels))
	s.mux.HandleFunc("GET /model-store/api/health", s.modelStoreRoute((*modelstore.Handlers).Health))

	// The four routes a runner's agent seed reconciler calls (runner/seed.go).
	var agentStoreRoutes http.Handler
	if s.agentStore != nil {
		routes := http.NewServeMux()
		agentstore.RegisterHandlersWithHookSet(routes, s.agentStore, s.agentStoreHandlerHooks())
		agentStoreRoutes = routes
	}
	s.mux.HandleFunc("GET /api/agent-store/seed/manifest", s.agentStoreRunnerSeedRoute(agentStoreRoutes))
	s.mux.HandleFunc("GET /api/agent-store/versions/{vid}/content", s.agentStoreRunnerSeedRoute(agentStoreRoutes))
	s.mux.HandleFunc("POST /api/agent-store/seed/observe", s.agentStoreRunnerSeedRoute(agentStoreRoutes))
	s.mux.HandleFunc("POST /api/agent-store/seed/drift", s.agentStoreRunnerSeedRoute(agentStoreRoutes))
}
