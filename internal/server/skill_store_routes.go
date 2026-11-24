package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/skillstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// The registry of SKILL.md skills, moved in from the standalone skill-store:
// the same routes under /skill-store/, answered from the Postgres module and
// the data directory that holds each skill's files, and the four seed routes
// runners call under /api/skill-store/.

// EnableSkillStore serves the store's routes from store. main calls it once the
// module is provisioned, migrated and open, before the server takes requests;
// until then, and on a server with no Postgres URL, the routes answer 503.
func (s *Server) EnableSkillStore(store *skillstore.Store) {
	s.skillStoreForResourceChecks.Store(store)
	s.skillStoreHandlers.Store(skillstore.NewHandlers(store))
}

// skillStoreRoute answers one route with the store's handler for it.
func (s *Server) skillStoreRoute(route func(*skillstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.skillStoreHandlers.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "skill_store_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no skills")
			return
		}
		route(handlers, w, r)
	}
}

// skillStoreRunnerSeedRoute is skillStoreRoute for a runner: only a request
// carrying a known runner's bearer token reaches the store.
func (s *Server) skillStoreRunnerSeedRoute(route func(*skillstore.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	answer := s.skillStoreRoute(route)
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeRunnerRequest(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		answer(w, r)
	}
}

// registerSkillStoreRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerSkillStoreRoutes() {
	s.mux.HandleFunc("GET /skill-store/health", s.skillStoreRoute((*skillstore.Handlers).Health))
	s.mux.HandleFunc("GET /skill-store/sources", s.skillStoreRoute((*skillstore.Handlers).ListSources))
	s.mux.HandleFunc("POST /skill-store/sources", s.skillStoreRoute((*skillstore.Handlers).CreateSource))
	s.mux.HandleFunc("GET /skill-store/sources/{id}", s.skillStoreRoute((*skillstore.Handlers).GetSource))
	s.mux.HandleFunc("DELETE /skill-store/sources/{id}", s.skillStoreRoute((*skillstore.Handlers).DeleteSource))
	s.mux.HandleFunc("POST /skill-store/sources/{id}/sync", s.skillStoreRoute((*skillstore.Handlers).SyncSource))
	s.mux.HandleFunc("GET /skill-store/skills", s.skillStoreRoute((*skillstore.Handlers).ListSkills))
	s.mux.HandleFunc("GET /skill-store/skills/{id}", s.skillStoreRoute((*skillstore.Handlers).GetSkill))
	s.mux.HandleFunc("DELETE /skill-store/skills/{id}", s.skillStoreRoute((*skillstore.Handlers).DeleteSkill))
	s.mux.HandleFunc("GET /skill-store/skills/{id}/files", s.skillStoreRoute((*skillstore.Handlers).ListSkillFiles))
	s.mux.HandleFunc("GET /skill-store/skills/{id}/files/{path...}", s.skillStoreRoute((*skillstore.Handlers).GetSkillFile))
	s.mux.HandleFunc("POST /skill-store/skills/{id}/install", s.skillStoreRoute((*skillstore.Handlers).InstallSkill))
	s.mux.HandleFunc("POST /skill-store/skills/{id}/uninstall", s.skillStoreRoute((*skillstore.Handlers).UninstallSkill))
	s.mux.HandleFunc("GET /skill-store/installations", s.skillStoreRoute((*skillstore.Handlers).ListInstallations))
	s.mux.HandleFunc("GET /skill-store/seed/profile", s.skillStoreRoute((*skillstore.Handlers).GetSeedProfile))
	s.mux.HandleFunc("PUT /skill-store/seed/profile", s.skillStoreRoute((*skillstore.Handlers).PutSeedProfile))
	s.mux.HandleFunc("GET /skill-store/seed/manifest", s.skillStoreRoute((*skillstore.Handlers).GetSeedManifest))
	s.mux.HandleFunc("GET /skill-store/versions/{vid}/content", s.skillStoreRoute((*skillstore.Handlers).GetVersionContent))
	s.mux.HandleFunc("POST /skill-store/seed/observe", s.skillStoreRoute((*skillstore.Handlers).PostSeedObserve))
	s.mux.HandleFunc("POST /skill-store/seed/drift", s.skillStoreRoute((*skillstore.Handlers).PostSeedDrift))

	// The four routes a runner's skill reconciler calls (runner/seed.go). Until
	// the move, /api/skill-store/ forwarded any path to the standalone store,
	// so a runner token reached every skill-store route; now it reaches these.
	s.mux.HandleFunc("GET /api/skill-store/seed/manifest", s.skillStoreRunnerSeedRoute((*skillstore.Handlers).GetSeedManifest))
	s.mux.HandleFunc("GET /api/skill-store/versions/{vid}/content", s.skillStoreRunnerSeedRoute((*skillstore.Handlers).GetVersionContent))
	s.mux.HandleFunc("POST /api/skill-store/seed/observe", s.skillStoreRunnerSeedRoute((*skillstore.Handlers).PostSeedObserve))
	s.mux.HandleFunc("POST /api/skill-store/seed/drift", s.skillStoreRunnerSeedRoute((*skillstore.Handlers).PostSeedDrift))
}
