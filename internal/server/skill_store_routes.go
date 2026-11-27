package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/skills"
	"github.com/kayushkin/llm-bridge-server/internal/config"
)

// The registry of SKILL.md skills, moved in from the standalone skill-store:
// the same routes under /skills/, answered from the Postgres module and
// the data directory that holds each skill's files, and the four seed routes
// runners call under /api/skill-store/.

// EnableSkills serves the store's routes from store. main calls it once the
// module is provisioned, migrated and open, before the server takes requests;
// until then, and on a server with no Postgres URL, the routes answer 503.
func (s *Server) EnableSkills(store *skills.Store) {
	s.skillStoreForResourceChecks.Store(store)
	s.skillStoreHandlers.Store(skills.NewHandlers(store))
}

// skillStoreRoute answers one route with the store's handler for it.
func (s *Server) skillStoreRoute(route func(*skills.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
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
func (s *Server) skillStoreRunnerSeedRoute(route func(*skills.Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
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
	s.mux.HandleFunc("GET /skills/health", s.skillStoreRoute((*skills.Handlers).Health))
	s.mux.HandleFunc("GET /skills/sources", s.skillStoreRoute((*skills.Handlers).ListSources))
	s.mux.HandleFunc("POST /skills/sources", s.skillStoreRoute((*skills.Handlers).CreateSource))
	s.mux.HandleFunc("GET /skills/sources/{id}", s.skillStoreRoute((*skills.Handlers).GetSource))
	s.mux.HandleFunc("DELETE /skills/sources/{id}", s.skillStoreRoute((*skills.Handlers).DeleteSource))
	s.mux.HandleFunc("POST /skills/sources/{id}/sync", s.skillStoreRoute((*skills.Handlers).SyncSource))
	s.mux.HandleFunc("GET /skills/skills", s.skillStoreRoute((*skills.Handlers).ListSkills))
	s.mux.HandleFunc("GET /skills/skills/{id}", s.skillStoreRoute((*skills.Handlers).GetSkill))
	s.mux.HandleFunc("DELETE /skills/skills/{id}", s.skillStoreRoute((*skills.Handlers).DeleteSkill))
	s.mux.HandleFunc("GET /skills/skills/{id}/files", s.skillStoreRoute((*skills.Handlers).ListSkillFiles))
	s.mux.HandleFunc("GET /skills/skills/{id}/files/{path...}", s.skillStoreRoute((*skills.Handlers).GetSkillFile))
	s.mux.HandleFunc("POST /skills/skills/{id}/install", s.skillStoreRoute((*skills.Handlers).InstallSkill))
	s.mux.HandleFunc("POST /skills/skills/{id}/uninstall", s.skillStoreRoute((*skills.Handlers).UninstallSkill))
	s.mux.HandleFunc("GET /skills/installations", s.skillStoreRoute((*skills.Handlers).ListInstallations))
	s.mux.HandleFunc("GET /skills/seed/profile", s.skillStoreRoute((*skills.Handlers).GetSeedProfile))
	s.mux.HandleFunc("PUT /skills/seed/profile", s.skillStoreRoute((*skills.Handlers).PutSeedProfile))
	s.mux.HandleFunc("GET /skills/seed/manifest", s.skillStoreRoute((*skills.Handlers).GetSeedManifest))
	s.mux.HandleFunc("GET /skills/versions/{vid}/content", s.skillStoreRoute((*skills.Handlers).GetVersionContent))
	s.mux.HandleFunc("POST /skills/seed/observe", s.skillStoreRoute((*skills.Handlers).PostSeedObserve))
	s.mux.HandleFunc("POST /skills/seed/drift", s.skillStoreRoute((*skills.Handlers).PostSeedDrift))

	// The four routes a runner's skill reconciler calls (runner/seed.go). Until
	// the move, /api/skill-store/ forwarded any path to the standalone store,
	// so a runner token reached every skill-store route; now it reaches these.
	s.mux.HandleFunc("GET /api/skill-store/seed/manifest", s.skillStoreRunnerSeedRoute((*skills.Handlers).GetSeedManifest))
	s.mux.HandleFunc("GET /api/skill-store/versions/{vid}/content", s.skillStoreRunnerSeedRoute((*skills.Handlers).GetVersionContent))
	s.mux.HandleFunc("POST /api/skill-store/seed/observe", s.skillStoreRunnerSeedRoute((*skills.Handlers).PostSeedObserve))
	s.mux.HandleFunc("POST /api/skill-store/seed/drift", s.skillStoreRunnerSeedRoute((*skills.Handlers).PostSeedDrift))
}
