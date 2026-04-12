package server

import (
	"encoding/json"
	"log"
	"net/http"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	memorystore "github.com/kayushkin/memory-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

type Server struct {
	mux          *http.ServeMux
	store        *store.Store
	agentStore   *agentstore.Store
	memoryStore  *memorystore.Store
	harnessStore *harnessstore.Store
	harness      *harness.Manager
	cfg          *config.Config
}

func New(st *store.Store, as *agentstore.Store, ms *memorystore.Store, hs *harnessstore.Store, cfg *config.Config) *Server {
	srv := &Server{
		mux:          http.NewServeMux(),
		store:        st,
		agentStore:   as,
		memoryStore:  ms,
		harnessStore: hs,
		harness:      harness.NewManager(st),
		cfg:          cfg,
	}
	srv.routes()
	srv.syncHarnessTypes()
	return srv
}

// syncHarnessTypes seeds the harness-store with harness type metadata.
func (s *Server) syncHarnessTypes() {
	if s.harnessStore == nil {
		return
	}
	for _, h := range allHarnesses {
		meta := harnessMetadata[h]
		if err := s.harnessStore.UpsertHarnessType(&harnessstore.HarnessType{
			Name:  h,
			Label: meta.Label,
			Emoji: meta.Emoji,
			Image: meta.Image,
		}); err != nil {
			log.Printf("sync harness type %s: %v", h, err)
		}
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /harnesses", s.handleHarnesses)

	// Static harness images
	s.mux.Handle("GET /images/", http.StripPrefix("/images/", http.FileServer(http.Dir(s.cfg.ImagesDir))))

	// Session routes
	s.mux.HandleFunc("GET /sessions", s.handleListSessions)
	s.mux.HandleFunc("POST /sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /sessions/{id}/send", s.handleSendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.handleSessionEvents)
	s.mux.HandleFunc("GET /sessions/{id}/messages", s.handleSessionMessages)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.handleSessionHistory)
	s.mux.HandleFunc("POST /sessions/{id}/interrupt", s.handleInterruptSession)
	s.mux.HandleFunc("POST /sessions/{id}/resume", s.handleResumeSession)
	s.mux.HandleFunc("POST /sessions/{id}/stop", s.handleStopSession)
	s.mux.HandleFunc("POST /sessions/{id}/compact", s.handleCompactSession)
	s.mux.HandleFunc("POST /sessions/{id}/fork", s.handleForkSession)
	s.mux.HandleFunc("POST /sessions/{id}/config", s.handleConfigSession)

	// Agent-store routes (mounted from agent-store library)
	if s.agentStore != nil {
		agentstore.RegisterHandlers(s.mux, s.agentStore)
	}

	// Memory-store routes (mounted from memory-store library)
	if s.memoryStore != nil {
		memorystore.RegisterHandlers(s.mux, s.memoryStore)
	}

	// Instance routes only available if harness-store is loaded
	if s.harnessStore != nil {
		s.mux.HandleFunc("GET /instances", s.handleListInstances)
		s.mux.HandleFunc("POST /instances", s.handleCreateInstance)
		s.mux.HandleFunc("GET /instances/{id}", s.handleGetInstance)
		s.mux.HandleFunc("PUT /instances/{id}", s.handleUpdateInstance)
		s.mux.HandleFunc("DELETE /instances/{id}", s.handleDeleteInstance)
		s.mux.HandleFunc("GET /instances/{id}/status", s.handleInstanceStatus)
		s.mux.HandleFunc("GET /instances/{id}/sessions", s.handleListInstanceSessions)
		s.mux.HandleFunc("GET /instances/{id}/credentials", s.handleListInstanceCredentials)
		s.mux.HandleFunc("POST /instances/{id}/credentials", s.handleBindCredential)
		s.mux.HandleFunc("DELETE /instances/{id}/credentials/{cred_id}", s.handleUnbindCredential)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
