// Package server is the HTTP surface for permission-store. The actual policy
// engine lives in the parent package — server.go is just routing + JSON
// codec + HTTP error mapping.
package server

import (
	"net/http"

	permstore "github.com/kayushkin/llm-bridge-server/internal/access/permissionstore"
)

// Server bundles the engine + store with an http.Handler.
type Server struct {
	store  *permstore.Store
	engine *permstore.Engine
	mux    *http.ServeMux
}

// New constructs a Server from an open Store. The engine is created
// internally; the cache is invalidated on every rule write.
func New(s *permstore.Store) *Server {
	srv := &Server{
		store:  s,
		engine: permstore.NewEngine(s),
		mux:    http.NewServeMux(),
	}
	srv.routes()
	return srv
}

// ServeHTTP makes Server implement http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	s.mux.HandleFunc("GET /rules", s.handleListRules)
	s.mux.HandleFunc("POST /rules", s.handleCreateRule)
	s.mux.HandleFunc("GET /rules/{id}", s.handleGetRule)
	s.mux.HandleFunc("PATCH /rules/{id}", s.handleUpdateRule)
	s.mux.HandleFunc("DELETE /rules/{id}", s.handleDeleteRule)

	s.mux.HandleFunc("POST /evaluate", s.handleEvaluate)

	s.mux.HandleFunc("GET /audit", s.handleListAudit)
}
