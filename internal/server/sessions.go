package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// displayNameFromMessage produces a compact session title from a user message:
// first non-empty line, truncated to 80 runes with an ellipsis.
func displayNameFromMessage(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	const maxRunes = 80
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}

// Request types are canonical — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type (
	CreateSessionRequest  = msg.CreateSessionRequest
	SendMessageRequest    = msg.SendMessageRequest
	ForkSessionRequest    = msg.ForkSessionRequest
	CompactSessionRequest = msg.CompactSessionRequest
	ConfigSessionRequest  = msg.ConfigSessionRequest
	RenameSessionRequest  = msg.RenameSessionRequest
)

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	writeJSON(w, sessions)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, sess)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	h := msg.Harness(req.Harness)
	if !isValidHarness(h) {
		http.Error(w, "invalid harness", http.StatusBadRequest)
		return
	}

	// Resolve instance (requires harness-store)
	var inst *msg.Instance
	if req.InstanceID != "" && s.harnessStore != nil {
		// Use specific instance
		var err error
		inst, err = s.harnessStore.GetInstance(req.InstanceID)
		if err != nil {
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}
		if !inst.Enabled {
			http.Error(w, "instance is disabled", http.StatusServiceUnavailable)
			return
		}
		if inst.HarnessType != req.Harness {
			http.Error(w, fmt.Sprintf("instance is for %s, not %s", inst.HarnessType, req.Harness), http.StatusBadRequest)
			return
		}
	} else if req.AutoStart && s.harnessStore != nil {
		// Find an available instance for this harness type
		instances, err := s.harnessStore.ListInstancesByHarness(h)
		if err == nil && len(instances) > 0 {
			for _, candidate := range instances {
				if candidate.Enabled {
					inst = &candidate
					break
				}
			}
		}
	}
	// If no instance found/available, fall back to local execution
	if inst == nil && req.AutoStart {
		if _, ok := harness.Available(h); !ok {
			http.Error(w, fmt.Sprintf("harness not available: %s", harness.BinaryName(h)), http.StatusServiceUnavailable)
			return
		}
	}

	if req.ClientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}

	sess := &store.Session{
		BridgeID:      generateBridgeID(),
		ClientID:      req.ClientID,
		DisplayName:   req.DisplayName,
		Harness:       req.Harness,
		State:         string(msg.SessionIdle),
		AgentID:       req.AgentID,
		SpawnerID:     req.SpawnerID,
		HarnessConfig: req.HarnessConfig,
	}

	if inst != nil {
		sess.InstanceID = inst.ID
	}

	if err := s.store.CreateSession(sess); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Start harness subprocess if requested
	if req.AutoStart {
		var startErr error
		if inst != nil && s.harnessStore != nil {
			credID := resolveCredential(s.harnessStore, inst.ID)
			_, startErr = s.harness.StartOnInstance(r.Context(), sess, inst, credID)
		} else {
			_, startErr = s.harness.Start(r.Context(), sess)
		}

		if startErr != nil {
			s.store.UpdateSessionState(sess.BridgeID, string(msg.SessionError))
			sess.State = string(msg.SessionError)
		} else {
			sess.State = string(msg.SessionRunning)
		}
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sess)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	var req RenameSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		http.Error(w, "display_name is required", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateSessionDisplayName(bridgeID, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, sess)
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if err := s.harness.Kill(bridgeID); err != nil {
		// Process might not be running, just update state
	}

	if err := s.store.UpdateSessionState(bridgeID, string(msg.SessionAborted)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionAborted)
	writeJSON(w, sess)
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if s.harness.Get(bridgeID) == nil {
		var startErr error
		if sess.InstanceID != "" && s.harnessStore != nil {
			inst, err := s.harnessStore.GetInstance(sess.InstanceID)
			if err != nil {
				http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusInternalServerError)
				return
			}
			credID := resolveCredential(s.harnessStore, inst.ID)
			_, startErr = s.harness.StartOnInstance(r.Context(), sess, inst, credID)
		} else {
			_, startErr = s.harness.Start(r.Context(), sess)
		}
		if startErr != nil {
			http.Error(w, fmt.Sprintf("failed to start harness: %v", startErr), http.StatusInternalServerError)
			return
		}
	}

	userEvent := msg.Event{
		Type:      msg.EventUserMessage,
		SessionID: bridgeID,
		BridgeID:  bridgeID,
		Timestamp: time.Now(),
		Result:    &msg.ResultEvent{Text: req.Message},
	}
	// BroadcastEvent stamps userEvent.MessageID, persists, and fans out so
	// other SSE subscribers see the user message immediately.
	if _, err := s.harness.BroadcastEvent(&userEvent); err != nil {
		log.Printf("[session] failed to broadcast user_message: %v", err)
	}
	if err := s.harness.PushEvent(userEvent); err != nil {
		log.Printf("[session] failed to push user_message to log-store: %v", err)
	}

	if name := displayNameFromMessage(req.Message); name != "" {
		if _, err := s.store.SetDisplayNameIfEmpty(bridgeID, name); err != nil {
			log.Printf("[session] failed to set display_name from first message: %v", err)
		}
	}

	if err := s.harness.Send(bridgeID, req.Message); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "sent", "message_id": userEvent.MessageID})
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	events := s.harness.Subscribe(bridgeID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.harness.Unsubscribe(bridgeID, events)
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	lastEventID := r.Header.Get("Last-Event-ID")
	var lastRowID int
	if lastEventID != "" {
		fmt.Sscanf(lastEventID, "%d", &lastRowID)
	}

	replayedIDs := make(map[int]bool)
	if lastRowID > 0 {
		if stored, err := s.store.ListEventsSinceID(bridgeID, lastRowID); err == nil {
			for _, ev := range stored {
				var parsed msg.Event
				if json.Unmarshal(ev.Data, &parsed) == nil {
					fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.RowID, parsed.Type, ev.Data)
					replayedIDs[ev.RowID] = true
				}
			}
			flusher.Flush()
		}
	} else {
		if stored, err := s.store.ListCurrentTurnEventsWithIDs(bridgeID); err == nil && len(stored) > 0 {
			for _, ev := range stored {
				var parsed msg.Event
				if json.Unmarshal(ev.Data, &parsed) == nil {
					fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.RowID, parsed.Type, ev.Data)
					replayedIDs[ev.RowID] = true
				}
			}
			flusher.Flush()
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			s.harness.Unsubscribe(bridgeID, events)
			return
		case stored, ok := <-events:
			if !ok {
				w.Write([]byte("event: close\ndata: {}\n\n"))
				flusher.Flush()
				return
			}
			if replayedIDs[int(stored.RowID)] {
				delete(replayedIDs, int(stored.RowID))
				continue
			}
			data, _ := json.Marshal(stored.Event)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", stored.RowID, stored.Event.Type, data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleInterruptSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if sess.State != string(msg.SessionRunning) {
		http.Error(w, "session not running", http.StatusConflict)
		return
	}

	if err := s.harness.Stop(bridgeID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.store.UpdateSessionState(bridgeID, string(msg.SessionIdle)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionIdle)
	writeJSON(w, sess)
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if sess.State != string(msg.SessionIdle) {
		http.Error(w, "session not idle", http.StatusConflict)
		return
	}

	if _, err := s.harness.Start(r.Context(), sess); err != nil {
		http.Error(w, fmt.Sprintf("failed to resume: %v", err), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionRunning)
	writeJSON(w, sess)
}

func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req CompactSessionRequest
	json.NewDecoder(r.Body).Decode(&req)

	cmd := "compact"
	if req.Summary != "" {
		cmd = "compact:" + req.Summary
	}
	if err := s.harness.SendCommand(bridgeID, cmd); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, sess)
}

func (s *Server) handleForkSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	parent, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req ForkSessionRequest
	json.NewDecoder(r.Body).Decode(&req)

	displayName := req.DisplayName
	if displayName == "" {
		displayName = parent.DisplayName + " (fork)"
	}

	if req.ClientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}

	forked := &store.Session{
		BridgeID:    generateBridgeID(),
		ClientID:    req.ClientID,
		DisplayName: displayName,
		Harness:     parent.Harness,
		State:       string(msg.SessionIdle),
		AgentID:     parent.AgentID,
		SpawnerID:   parent.SpawnerID,
		ParentID:    parent.BridgeID,
	}

	if err := s.store.CreateSession(forked); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := s.harness.Start(context.Background(), forked); err != nil {
		s.store.UpdateSessionState(forked.BridgeID, string(msg.SessionError))
		forked.State = string(msg.SessionError)
	} else {
		forked.State = string(msg.SessionRunning)
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, forked)
}

func (s *Server) handleConfigSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req ConfigSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	params, _ := json.Marshal(req)
	if err := s.harness.SendCommand(bridgeID, "config:"+string(params)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, sess)
}

func (s *Server) handleDiscoverSessions(w http.ResponseWriter, r *http.Request) {
	harnessFilter := msg.Harness(r.URL.Query().Get("harness"))

	sessions, err := s.harness.DiscoverSessions(r.Context(), harnessFilter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []msg.StoredSession{}
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

	// Persist discovered sessions to the store so they appear in GET /sessions
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

		// Sessions discovered locally belong to the local instance
		instanceID := localInstances[ds.Harness]

		inserted, err := s.store.UpsertDiscoveredSession(
			ds.ID,
			displayName,
			string(ds.Harness),
			instanceID,
			ds.CreatedAt,
			ds.UpdatedAt,
		)
		if err != nil {
			log.Printf("[discover] failed to upsert session %s: %v", ds.ID, err)
			continue
		}
		if inserted {
			imported++
			// Import history to log-store for new sessions
			go func(h msg.Harness, sid string) {
				n, err := s.harness.ImportHistory(context.Background(), h, sid)
				if err != nil {
					log.Printf("[discover] failed to import history for %s: %v", sid, err)
				} else if n > 0 {
					log.Printf("[discover] imported %d events for session %s", n, sid)
				}
			}(ds.Harness, ds.ID)
		}
	}
	if imported > 0 {
		log.Printf("[discover] imported %d new sessions", imported)
	}

	writeJSON(w, sessions)
}

func generateBridgeID() string {
	return fmt.Sprintf("br_%d", time.Now().UnixNano())
}

// resolveCredential returns the highest-priority enabled credential ID for an instance,
// or empty string if none are bound.
func resolveCredential(hs *harnessstore.Store, instanceID string) string {
	bindings, err := hs.ListInstanceCredentials(instanceID)
	if err != nil || len(bindings) == 0 {
		return ""
	}
	for _, b := range bindings {
		if b.Enabled {
			return b.CredentialID
		}
	}
	return ""
}

