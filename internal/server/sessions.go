package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

type CreateSessionRequest struct {
	Harness         string `json:"harness"`
	InstanceID      string `json:"instance_id,omitempty"`       // specific instance to use
	DisplayName     string `json:"display_name,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	SpawnerID       string `json:"spawner_id,omitempty"`
	AutoStart       bool   `json:"auto_start,omitempty"`        // start harness immediately
	ClientRequestID string `json:"client_request_id,omitempty"` // frontend correlation ID, echoed back
}

type SendMessageRequest struct {
	Message string `json:"message"`
}

type ForkSessionRequest struct {
	DisplayName string `json:"display_name,omitempty"`
}

type CompactSessionRequest struct {
	Summary string `json:"summary,omitempty"`
}

type ConfigSessionRequest struct {
	Model         string   `json:"model,omitempty"`
	Effort        string   `json:"effort,omitempty"`
	DisabledTools []string `json:"disabled_tools,omitempty"`
	MaxBudget     *float64 `json:"max_budget,omitempty"`
}

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
		if string(inst.HarnessType) != req.Harness {
			http.Error(w, fmt.Sprintf("instance is for %s, not %s", inst.HarnessType, req.Harness), http.StatusBadRequest)
			return
		}
	} else if req.AutoStart && s.harnessStore != nil {
		// Find an available instance for this harness type
		instances, err := s.harnessStore.ListInstancesByHarness(h)
		if err == nil && len(instances) > 0 {
			// Pick first available instance with capacity
			for _, candidate := range instances {
				active, _ := s.store.CountSlotsByInstance(candidate.ID)
				if active < candidate.MaxConcurrentSessions {
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

	sess := &store.Session{
		ID:              generateID(),
		DisplayName:     req.DisplayName,
		Harness:         req.Harness,
		State:           string(msg.SessionIdle),
		AgentID:         req.AgentID,
		SpawnerID:       req.SpawnerID,
		ClientRequestID: req.ClientRequestID,
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
			// Get credential bindings from harness-store
			credBindings, _ := s.harnessStore.ListInstanceCredentials(inst.ID)

			// Acquire credential slot using runtime store
			credID, err := s.store.AcquireCredentialSlot(inst.ID, sess.ID, credBindings)
			if err != nil {
				// No credentials available - session created but can't start
				s.store.UpdateSessionState(sess.ID, string(msg.SessionError))
				sess.State = string(msg.SessionError)
				w.WriteHeader(http.StatusCreated)
				writeJSON(w, sess)
				return
			}

			_, startErr = s.harness.StartOnInstance(r.Context(), sess, inst, credID)
		} else {
			_, startErr = s.harness.Start(r.Context(), sess)
		}

		if startErr != nil {
			// Release credential slot on failure
			if inst != nil {
				s.store.ReleaseCredentialSlot(sess.ID)
			}
			s.store.UpdateSessionState(sess.ID, string(msg.SessionError))
			sess.State = string(msg.SessionError)
		} else {
			sess.State = string(msg.SessionRunning)
		}
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sess)
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Kill the harness process
	if err := s.harness.Kill(id); err != nil {
		// Process might not be running, just update state
	}

	// Release credential slot if session was on an instance
	if sess.InstanceID != "" {
		s.store.ReleaseCredentialSlot(id)
	}

	if err := s.store.UpdateSessionState(id, string(msg.SessionAborted)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionAborted)
	writeJSON(w, sess)
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Start harness if not running
	if s.harness.Get(id) == nil {
		if _, err := s.harness.Start(r.Context(), sess); err != nil {
			http.Error(w, fmt.Sprintf("failed to start harness: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Persist user message as an event for history
	userEvent := msg.Event{
		Type:      "user_message",
		SessionID: id,
		Timestamp: time.Now(),
		Result:    &msg.ResultEvent{Text: req.Message},
	}
	if data, err := json.Marshal(userEvent); err == nil {
		s.store.StoreEvent(id, "user_message", data)
	}
	if err := s.harness.PushEvent(userEvent); err != nil {
		log.Printf("[session] failed to push user_message to log-store: %v", err)
	}

	if err := s.harness.Send(id, req.Message); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "sent"})
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetSession(id); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Subscribe to live events fan-out
	events := s.harness.Subscribe(id)

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.harness.Unsubscribe(id, events)
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Check for Last-Event-ID for reconnection support
	lastEventID := r.Header.Get("Last-Event-ID")
	var lastRowID int
	if lastEventID != "" {
		fmt.Sscanf(lastEventID, "%d", &lastRowID)
	}

	// Replay events from DB — either since last event ID (reconnection)
	// or current turn events (initial connection).
	replayedIDs := make(map[int]bool)
	if lastRowID > 0 {
		// Reconnection: replay everything since last seen event
		if stored, err := s.store.ListEventsSinceID(id, lastRowID); err == nil {
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
		// Initial connection: replay current turn events
		if stored, err := s.store.ListCurrentTurnEventsWithIDs(id); err == nil && len(stored) > 0 {
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
			s.harness.Unsubscribe(id, events)
			return
		case event, ok := <-events:
			if !ok {
				w.Write([]byte("event: close\ndata: {}\n\n"))
				flusher.Flush()
				return
			}
			data, _ := json.Marshal(event)
			// Get the row ID for this event (it was just persisted by the manager)
			rowID, _ := s.store.MaxEventID(id)
			if replayedIDs[rowID] {
				delete(replayedIDs, rowID)
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", rowID, event.Type, data)
			flusher.Flush()
		}
	}
}


func (s *Server) handleInterruptSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if sess.State != string(msg.SessionRunning) {
		http.Error(w, "session not running", http.StatusConflict)
		return
	}

	if err := s.harness.Stop(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.store.UpdateSessionState(id, string(msg.SessionIdle)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionIdle)
	writeJSON(w, sess)
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if sess.State != string(msg.SessionIdle) {
		http.Error(w, "session not idle", http.StatusConflict)
		return
	}

	// Restart harness with resume flag
	if _, err := s.harness.Start(r.Context(), sess); err != nil {
		http.Error(w, fmt.Sprintf("failed to resume: %v", err), http.StatusInternalServerError)
		return
	}

	sess.State = string(msg.SessionRunning)
	writeJSON(w, sess)
}

func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req CompactSessionRequest
	json.NewDecoder(r.Body).Decode(&req)

	// Send compact command
	cmd := "compact"
	if req.Summary != "" {
		cmd = "compact:" + req.Summary
	}
	if err := s.harness.SendCommand(id, cmd); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, sess)
}

func (s *Server) handleForkSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	parent, err := s.store.GetSession(id)
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

	forked := &store.Session{
		ID:          generateID(),
		DisplayName: displayName,
		Harness:     parent.Harness,
		State:       string(msg.SessionIdle),
		AgentID:     parent.AgentID,
		SpawnerID:   parent.SpawnerID,
		ParentID:    parent.ID,
	}

	if err := s.store.CreateSession(forked); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Start forked session (harness will use parent_id to fork state)
	if _, err := s.harness.Start(context.Background(), forked); err != nil {
		s.store.UpdateSessionState(forked.ID, string(msg.SessionError))
		forked.State = string(msg.SessionError)
	} else {
		forked.State = string(msg.SessionRunning)
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, forked)
}

func (s *Server) handleConfigSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req ConfigSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Send config command to harness (the harness binary interprets the JSON params)
	params, _ := json.Marshal(req)
	if err := s.harness.SendCommand(id, "config:"+string(params)); err != nil {
		// Process might not be running — store config for next start
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
		// Use project path as display name, fall back to prompt snippet
		displayName := ds.Project
		if displayName == "" || displayName == "/" {
			displayName = ds.Prompt
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
		}
	}
	if imported > 0 {
		log.Printf("[discover] imported %d new sessions", imported)
	}

	writeJSON(w, sessions)
}

func generateID() string {
	return fmt.Sprintf("sess_%d", time.Now().UnixNano())
}
