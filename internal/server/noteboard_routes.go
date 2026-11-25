package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	noteboardapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
)

// The operator's notes and todos, moved in from the standalone noteboard: the
// same routes under /noteboard/, answered from the Postgres module.

// EnableNoteboard serves the store's routes from store. main calls it once the
// module is provisioned, migrated and open; until then, and on a server with no
// Postgres URL, the routes answer 503.
func (s *Server) EnableNoteboard(store *items.Store) {
	s.noteboard.Store(noteboardapi.New(store))
	s.referenceReadable.noteboard.Store(store)
}

// noteboardRoute answers one route with the store's handler for it.
func (s *Server) noteboardRoute(route func(*noteboardapi.API, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := s.noteboard.Load()
		if handlers == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "noteboard_off",
				config.PostgresURLEnvironmentVariable+" is not set, so this server keeps no notes")
			return
		}
		route(handlers, w, r)
	}
}

// registerNoteboardRoutes mounts the store. The patterns are written out,
// because the route classification test reads them from string literals.
func (s *Server) registerNoteboardRoutes() {
	s.mux.HandleFunc("GET /noteboard/health", s.noteboardRoute((*noteboardapi.API).Health))
	s.mux.HandleFunc("GET /noteboard/api/items", s.noteboardRoute((*noteboardapi.API).ListItems))
	s.mux.HandleFunc("POST /noteboard/api/items", s.noteboardRoute((*noteboardapi.API).CreateItem))
	s.mux.HandleFunc("POST /noteboard/api/items/rerank", s.noteboardRoute((*noteboardapi.API).Rerank))
	s.mux.HandleFunc("POST /noteboard/api/items/query", s.noteboardRoute((*noteboardapi.API).QueryItems))
	s.mux.HandleFunc("POST /noteboard/api/items/batch", s.noteboardRoute((*noteboardapi.API).BatchItems))
	s.mux.HandleFunc("GET /noteboard/api/items/query-options", s.noteboardRoute((*noteboardapi.API).ItemsQueryOptions))
	s.mux.HandleFunc("GET /noteboard/api/items/{id}", s.noteboardRoute((*noteboardapi.API).GetItem))
	s.mux.HandleFunc("PATCH /noteboard/api/items/{id}", s.noteboardRoute((*noteboardapi.API).PatchItem))
	s.mux.HandleFunc("DELETE /noteboard/api/items/{id}", s.noteboardRoute((*noteboardapi.API).DeleteItem))
	s.mux.HandleFunc("GET /noteboard/api/items/{id}/revisions", s.noteboardRoute((*noteboardapi.API).ListRevisions))
	s.mux.HandleFunc("POST /noteboard/api/items/{id}/restore", s.noteboardRoute((*noteboardapi.API).RestoreItem))
	s.mux.HandleFunc("POST /noteboard/api/items/{id}/hold", s.noteboardRoute((*noteboardapi.API).HoldItem))
	s.mux.HandleFunc("POST /noteboard/api/items/{id}/unhold", s.noteboardRoute((*noteboardapi.API).UnholdItem))
	s.mux.HandleFunc("GET /noteboard/api/items/{id}/occurrences", s.noteboardRoute((*noteboardapi.API).Occurrences))
	s.mux.HandleFunc("GET /noteboard/api/lists", s.noteboardRoute((*noteboardapi.API).Lists))
	s.mux.HandleFunc("GET /noteboard/api/tags", s.noteboardRoute((*noteboardapi.API).Tags))
	s.mux.HandleFunc("GET /noteboard/api/search", s.noteboardRoute((*noteboardapi.API).Search))
}
