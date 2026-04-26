package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"time"

	agentstore "github.com/kayushkin/agent-store"
	harnessstore "github.com/kayushkin/harness-store"
	hookstore "github.com/kayushkin/hook-store"
	memorystore "github.com/kayushkin/memory-store"
	modelstore "github.com/kayushkin/model-store"
	snapshotstore "github.com/kayushkin/snapshot-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// autoResumeWindow caps how recently a session must have been active (by
// updated_at) for startup reconciliation to auto-restart its harness. Sessions
// older than this are left at idle for the user to resume manually.
const autoResumeWindow = 5 * time.Minute

type Server struct {
	mux           *http.ServeMux
	store         *store.Store
	agentStore    *agentstore.Store
	memoryStore   *memorystore.Store
	harnessStore  *harnessstore.Store
	hookStore     *hookstore.Store
	modelStore    *modelstore.Store
	snapshotStore *snapshotstore.Store
	harness       *harness.Manager
	bridgePrefs   *bridgePrefsStore
	cfState       *conformanceState
	cfg           *config.Config
}

func New(st *store.Store, as *agentstore.Store, ms *memorystore.Store, hs *harnessstore.Store, hks *hookstore.Store, mds *modelstore.Store, ss *snapshotstore.Store, cfg *config.Config) *Server {
	srv := &Server{
		mux:           http.NewServeMux(),
		store:         st,
		agentStore:    as,
		memoryStore:   ms,
		harnessStore:  hs,
		hookStore:     hks,
		modelStore:    mds,
		snapshotStore: ss,
		harness:       harness.NewManager(st, cfg.LogStoreURL),
		bridgePrefs:   newBridgePrefsStore(cfg.BridgePrefsPath),
		cfState:       newConformanceState(cfg.ConformancePath),
		cfg:           cfg,
	}
	srv.routes()
	srv.syncHarnessTypes()
	srv.startSnapshotGC()
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
	s.mux.HandleFunc("GET /harnesses/{name}/capabilities", s.handleHarnessCapabilities)

	// Static harness images
	s.mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(s.cfg.ImagesDir))))

	// Session routes
	s.mux.HandleFunc("GET /sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /sessions/search", s.handleSearchSessions)
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
	s.mux.HandleFunc("POST /sessions/{id}/rename", s.handleRenameSession)
	s.mux.HandleFunc("POST /sessions/{id}/auto-rename", s.handleAutoRenameSession)
	s.mux.HandleFunc("POST /sessions/{id}/config", s.handleConfigSession)
	s.mux.HandleFunc("PUT /sessions/{id}/folder", s.handleSetSessionFolder)
	s.mux.HandleFunc("GET /sessions/{id}/git/repos", s.handleSessionGitRepos)
	s.mux.HandleFunc("GET /sessions/{id}/git", s.handleSessionGit)

	// Folder registry — sidebar organization for sessions
	s.mux.HandleFunc("GET /folders", s.handleListFolders)
	s.mux.HandleFunc("POST /folders", s.handleCreateFolder)
	s.mux.HandleFunc("DELETE /folders/{name}", s.handleDeleteFolder)
	s.mux.HandleFunc("PUT /folders/{name}", s.handleRenameFolder)

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

	// Hook registry routes (mounted only when hook-store is loaded).
	// /hooks/exec/{id} is always registered because the hook-store is
	// where registered hooks are resolved; without the store, exec 404s.
	if s.hookStore != nil {
		s.mux.HandleFunc("POST /hooks", s.handleCreateHook)
		s.mux.HandleFunc("GET /hooks", s.handleListHooks)
		s.mux.HandleFunc("GET /hooks/{id}", s.handleGetHook)
		s.mux.HandleFunc("PATCH /hooks/{id}", s.handleUpdateHook)
		s.mux.HandleFunc("DELETE /hooks/{id}", s.handleDeleteHook)
		s.mux.HandleFunc("POST /hooks/exec/{id}", s.handleExecHook)
	}

	// Credential routes (aiauth)
	s.mux.HandleFunc("GET /credentials", s.handleCredentialsList)
	s.mux.HandleFunc("POST /credentials", s.handleCredentialCreate)
	s.mux.HandleFunc("DELETE /credentials/{id}", s.handleCredentialDelete)

	// Model routes (model-store)
	if s.modelStore != nil {
		s.mux.HandleFunc("GET /models", s.handleModels)
	}

	// Snapshot routes (snapshot-store). Metadata + blob retrieval for
	// Edit/Write tool-call before/after diffs rendered by the UI.
	if s.snapshotStore != nil {
		s.mux.HandleFunc("GET /sessions/{id}/tools/{tool_use_id}/snapshots", s.handleGetSnapshots)
		s.mux.HandleFunc("GET /snapshots/blob/{sha}", s.handleGetSnapshotBlob)
	}

	// Conformance
	s.mux.HandleFunc("GET /conformance", s.handleConformanceGet)
	s.mux.HandleFunc("POST /conformance/run", s.handleConformanceRun)

	// Bridge prefs
	s.mux.HandleFunc("GET /bridge-prefs", s.handleBridgePrefs)
	s.mux.HandleFunc("PUT /bridge-prefs", s.handleBridgePrefs)

	// Admin housekeeping (called by scheduler cron)
	s.mux.HandleFunc("POST /admin/file-inactive", s.handleFileInactive)
	s.mux.HandleFunc("POST /admin/archive-old", s.handleArchiveOld)

	// Runner WebSocket — long-lived connection from llm-bridge-runner
	// daemons on remote machines. Auth is bearer-token in the upgrade
	// header (see LLMBRIDGE_RUNNER_TOKEN) and re-validated on Hello.
	s.mux.HandleFunc("GET /api/runner/ws", s.handleRunnerWS)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleSearchSessions proxies /sessions/search to log-store's /api/v1/sessions/search.
func (s *Server) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	target := fmt.Sprintf("%s/api/v1/sessions/search", s.cfg.LogStoreURL)
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

// ReconcileAndResume clears stale 'running' state left over from the previous
// server lifetime and auto-restarts the harness for sessions whose last
// activity was inside autoResumeWindow. Sessions that went quiet before the
// window are left idle — the user can resume them on demand.
func (s *Server) ReconcileAndResume() {
	sessions, err := s.store.ReconcileRunningSessions()
	if err != nil {
		log.Printf("[reconcile] %v", err)
		return
	}
	if len(sessions) == 0 {
		return
	}
	log.Printf("[reconcile] reset %d stale running→idle", len(sessions))

	if s.harnessStore == nil {
		return
	}
	cutoff := time.Now().Add(-autoResumeWindow)
	var resumed int
	for i := range sessions {
		sess := sessions[i]
		if sess.UpdatedAt.Before(cutoff) {
			continue
		}
		resumed++
		go s.autoResume(sess)
	}
	if resumed > 0 {
		log.Printf("[reconcile] auto-resuming %d sessions active within %s", resumed, autoResumeWindow)
	}
}

// autoResume restarts a single session's harness process. Mirrors the flow in
// handleResumeSession minus the HTTP plumbing. If the previous turn was
// killed mid-flight (a user_message with no following result), the message
// text is re-sent once the harness is ready so the turn actually completes
// instead of sitting idle waiting for new input.
func (s *Server) autoResume(sess store.Session) {
	if sess.InstanceID == "" {
		return
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		log.Printf("[auto-resume] %s: instance %s not found: %v", sess.BridgeID, sess.InstanceID, err)
		return
	}
	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, err := s.startOnInstance(context.Background(), &sess, inst, credID); err != nil {
		log.Printf("[auto-resume] %s: start failed: %v", sess.BridgeID, err)
		return
	}
	log.Printf("[auto-resume] %s: resumed", sess.BridgeID)

	text, pending, err := s.store.PendingTurnMessage(sess.BridgeID)
	if err != nil {
		log.Printf("[auto-resume] %s: pending-turn check failed: %v", sess.BridgeID, err)
		return
	}
	if !pending {
		return
	}
	// Harness subprocess needs a moment to finish its start handshake before
	// it will accept a message on stdin. 2s is enough for Claude Code's
	// resume-load; shorter risks writing before the pipe is being drained.
	time.Sleep(2 * time.Second)
	if err := s.harness.Send(sess.BridgeID, text); err != nil {
		log.Printf("[auto-resume] %s: replay send failed: %v", sess.BridgeID, err)
		return
	}
	log.Printf("[auto-resume] %s: replayed interrupted turn", sess.BridgeID)
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
			source, folder := s.discoverySourceFolder(ds.Prompt)
			inserted, err := s.store.UpsertDiscoveredSession(ds.ID, displayName, string(ds.Harness), instanceID, source, folder, ds.CreatedAt, ds.UpdatedAt)
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
