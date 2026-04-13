package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

type Server struct {
	mux          *http.ServeMux
	store        *store.Store
	agentStore   *agentstore.Store
	memoryStore  *memorystore.Store
	harnessStore *harnessstore.Store
	modelStore   *modelstore.Store
	harness      *harness.Manager
	bridgePrefs  *bridgePrefsStore
	cfg          *config.Config
}

func New(st *store.Store, as *agentstore.Store, ms *memorystore.Store, hs *harnessstore.Store, mds *modelstore.Store, cfg *config.Config) *Server {
	srv := &Server{
		mux:          http.NewServeMux(),
		store:        st,
		agentStore:   as,
		memoryStore:  ms,
		harnessStore: hs,
		modelStore:   mds,
		harness:      harness.NewManager(st, cfg.LogStoreURL),
		bridgePrefs:  newBridgePrefsStore(cfg.BridgePrefsPath),
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
	s.mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(s.cfg.ImagesDir))))

	// Session routes
	s.mux.HandleFunc("GET /sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /sessions/discover", s.handleDiscoverSessions)
	s.mux.HandleFunc("POST /sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /sessions/{id}/send", s.handleSendMessage)
	s.mux.HandleFunc("GET /sessions/{id}/events", s.handleSessionEvents)
	s.mux.HandleFunc("GET /sessions/{id}/messages", s.proxyToLogStore)
	s.mux.HandleFunc("GET /sessions/{id}/history", s.proxyToLogStore)
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

	// Credential routes (aiauth)
	s.mux.HandleFunc("GET /credentials", s.handleCredentialsList)
	s.mux.HandleFunc("POST /credentials", s.handleCredentialCreate)
	s.mux.HandleFunc("DELETE /credentials/{id}", s.handleCredentialDelete)

	// Model routes (model-store)
	if s.modelStore != nil {
		s.mux.HandleFunc("GET /models", s.handleModels)
	}

	// Bridge prefs
	s.mux.HandleFunc("GET /bridge-prefs", s.handleBridgePrefs)
	s.mux.HandleFunc("PUT /bridge-prefs", s.handleBridgePrefs)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// proxyToLogStore proxies /sessions/{id}/messages and /sessions/{id}/history to log-store.
func (s *Server) proxyToLogStore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	endpoint := path.Base(r.URL.Path) // "messages" or "history"
	target := fmt.Sprintf("%s/api/v1/sessions/%s/%s", s.cfg.LogStoreURL, id, endpoint)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("log-store unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// AutoDiscover runs session discovery for all harness types and imports them to the store.
// Called on startup to populate the session list with on-disk sessions.
func (s *Server) AutoDiscover() {
	go func() {
		ctx := context.Background()
		sessions, err := s.harness.DiscoverSessions(ctx, "")
		if err != nil {
			log.Printf("auto-discover: %v", err)
			return
		}

		// Build map of harness type → local instance ID.
		// Discovery runs the harness binary locally, so sessions belong to the local instance.
		localInstances := make(map[msg.Harness]string)
		if s.harnessStore != nil {
			for _, h := range []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex} {
				instances, err := s.harnessStore.ListInstancesByHarness(h)
				if err == nil {
					for _, inst := range instances {
						if inst.Enabled && inst.Transport == msg.TransportLocal {
							localInstances[h] = inst.ID
							break
						}
					}
				}
			}
		}

		var imported int
		for _, ds := range sessions {
			// Use prompt as display name - it's more useful for identifying sessions
			displayName := ds.Prompt
			if displayName == "" {
				displayName = ds.Project
			}
			if len(displayName) > 100 {
				displayName = displayName[:100]
			}

			instanceID := localInstances[ds.Harness]
			inserted, err := s.store.UpsertDiscoveredSession(ds.ID, displayName, string(ds.Harness), instanceID, ds.CreatedAt, ds.UpdatedAt)
			if err == nil && inserted {
				imported++
				// Import history to log-store for new sessions
				go func(h msg.Harness, sid string) {
					n, err := s.harness.ImportHistory(context.Background(), h, sid)
					if err != nil {
						log.Printf("[auto-discover] failed to import history for %s: %v", sid, err)
					} else if n > 0 {
						log.Printf("[auto-discover] imported %d events for session %s", n, sid)
					}
				}(ds.Harness, ds.ID)
			}
		}
		if imported > 0 {
			log.Printf("[auto-discover] imported %d sessions", imported)
		}
	}()
}
