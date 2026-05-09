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
	"github.com/kayushkin/llm-bridge-server/conformance"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// folderForSource returns the effective folder for a given source tag, or
// "" if the source is unmapped (leaving the session unfiled). The effective
// map is the env-var defaults (config.SourceFolders) overlaid with runtime
// overrides from the source_folders table. DB errors fall back to defaults
// — failing closed (returning "") would silently un-file new sessions.
func (s *Server) folderForSource(source string) string {
	if source == "" || s.cfg == nil {
		return ""
	}
	if s.store != nil {
		if overrides, err := s.store.ListSourceFolders(); err == nil {
			if v, ok := overrides[source]; ok {
				return v
			}
		} else {
			log.Printf("[source-folders] failed to load overrides: %v", err)
		}
	}
	return s.cfg.SourceFolders[source]
}

// discoveryPromptPrefixes maps stable prompt prefixes to a source tag for
// sessions ingested via auto-discover. The live spawn path tags sessions via
// CreateSessionRequest.Purpose, but discovery only sees the on-disk session
// (prompt + harness id) — prefix recognition is the only signal that classifies
// these as scheduled jobs rather than user sessions. Producers own their
// prompts; if a prefix here drifts from the live prompt, new sessions land
// unfiled and the regression is immediately visible in /sessions.
var discoveryPromptPrefixes = []struct {
	prefix string
	source string
}{
	// scheduler/cmd/autoworker/main.go defaultPrompt
	{"You are the nightly todo-worker.", "autoworker"},
	// inber/scripts/harness-watch.sh
	{"You are running as a scheduled harness-watch job", "harness-watch"},
}

// discoverySourceFolder infers a (source, folder) pair for a freshly-discovered
// on-disk session. Conformance prompts are recognised by exact match; other
// scheduled sources are recognised by stable prompt prefixes registered in
// discoveryPromptPrefixes. Anything else is left unfiled — the harness CLI is
// the source of truth for the original prompt and we don't second-guess user
// sessions.
func (s *Server) discoverySourceFolder(prompt string) (string, string) {
	if conformance.IsConformancePrompt(prompt) {
		return conformance.SourceTag, s.folderForSource(conformance.SourceTag)
	}
	for _, p := range discoveryPromptPrefixes {
		if strings.HasPrefix(prompt, p.prefix) {
			return p.source, s.folderForSource(p.source)
		}
	}
	return "", ""
}

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
	state := r.URL.Query().Get("state")
	var (
		sessions []store.Session
		err      error
	)
	if state != "" {
		sessions, err = s.store.ListSessionsByState(state)
	} else {
		sessions, err = s.store.ListSessions()
	}
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

	// Mode validation. Empty defaults to events for backward compat.
	mode := req.Mode
	if mode == "" {
		mode = msg.SessionModeEvents
	}
	switch mode {
	case msg.SessionModeEvents:
		// always allowed
	case msg.SessionModePTY:
		if !harnessSupportsPTY[h] {
			http.Error(w, `{"error":{"code":"pty_unsupported","message":"harness does not support pty mode"}}`, http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("invalid mode: %q", mode), http.StatusBadRequest)
		return
	}

	// Every session must be bound to a harness instance — no local-spawn
	// fallback. harness-store is the single source of truth for which
	// instance runs a session.
	if s.harnessStore == nil {
		http.Error(w, "harness-store not configured", http.StatusServiceUnavailable)
		return
	}

	inst, err := resolveInstance(s.harnessStore, req.InstanceID, h)
	if err != nil {
		http.Error(w, err.message, err.status)
		return
	}

	// Caller-minted session id: workers (autoworker, scheduler, dispatcher)
	// pass their own session_id so they can persist a kanban link or queue
	// row before the create round-trip returns. Empty = bridge mints
	// br_<nanos> as before. Collisions return 409.
	bridgeID := req.SessionID
	if bridgeID == "" {
		bridgeID = generateBridgeID()
	} else if _, err := s.store.GetSession(bridgeID); err == nil {
		http.Error(w, "session_id already exists", http.StatusConflict)
		return
	}

	sess := &store.Session{
		SessionID:     bridgeID,
		DisplayName:   req.DisplayName,
		Harness:       req.Harness,
		InstanceID:    inst.ID,
		State:         string(msg.SessionIdle),
		AgentID:       req.AgentID,
		SpawnerID:     req.SpawnerID,
		HarnessConfig: req.HarnessConfig,
		Purpose:       req.Purpose,
		Type:          req.Type,
		Origin:        req.Origin,
		FolderName:    s.folderForSource(req.Purpose),
		Mode:          mode,
	}

	// Snapshot the global Bypass Permissions toggle into the session so the
	// per-session value is durable from creation onward. Skipped if the
	// caller already set bypass_permissions in req.HarnessConfig.
	s.snapshotBypassIntoSession(sess)

	if err := s.store.CreateSession(sess); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.AutoStart {
		credID := resolveCredential(s.harnessStore, inst.ID)
		if _, startErr := s.startOnInstance(r.Context(), sess, inst, credID); startErr != nil {
			s.store.UpdateSessionState(sess.SessionID, string(msg.SessionError))
			sess.State = string(msg.SessionError)
		} else {
			sess.State = string(msg.SessionRunning)
		}
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sess)
}

// httpErr carries a message and the HTTP status to return.
type httpErr struct {
	status  int
	message string
}

// resolveInstance picks the harness instance that a session should run on.
// Callers pass an explicit instance_id or "" to auto-pick any enabled instance
// for the given harness type. Returns an error suitable for http.Error when
// no usable instance is found.
func resolveInstance(hs *harnessstore.Store, instanceID string, h msg.Harness) (*msg.Instance, *httpErr) {
	if instanceID != "" {
		inst, err := hs.GetInstance(instanceID)
		if err != nil {
			return nil, &httpErr{http.StatusNotFound, "instance not found"}
		}
		if !inst.Enabled {
			return nil, &httpErr{http.StatusServiceUnavailable, "instance is disabled"}
		}
		if inst.HarnessType != h {
			return nil, &httpErr{http.StatusBadRequest, fmt.Sprintf("instance is for %s, not %s", inst.HarnessType, h)}
		}
		return inst, nil
	}

	instances, err := hs.ListInstancesByHarness(h)
	if err != nil {
		return nil, &httpErr{http.StatusInternalServerError, err.Error()}
	}
	for i := range instances {
		if instances[i].Enabled {
			return &instances[i], nil
		}
	}
	return nil, &httpErr{http.StatusServiceUnavailable, fmt.Sprintf("no enabled instance for harness: %s", h)}
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
	// User-set name wins. Drop any in-flight renamer reservation so its eventual
	// /auto-rename callback no-ops (ApplyAutoRename will see the cleared slot).
	if err := s.store.ClearRenamerSlot(bridgeID); err != nil {
		log.Printf("[session] clear renamer slot on rename: %v", err)
	}
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.broadcastDisplayNameChanged(bridgeID, sess.DisplayName)
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

	// Message and Blocks are mutually exclusive — fail fast at the boundary
	// so harnesses never see both populated.
	if req.Message != "" && len(req.Blocks) > 0 {
		http.Error(w, "message and blocks are mutually exclusive", http.StatusBadRequest)
		return
	}
	if req.Message == "" && len(req.Blocks) == 0 {
		http.Error(w, "message or blocks must be set", http.StatusBadRequest)
		return
	}

	if s.harness.Get(bridgeID) == nil {
		if sess.InstanceID == "" || s.harnessStore == nil {
			http.Error(w, "session has no instance bound", http.StatusInternalServerError)
			return
		}
		inst, err := s.harnessStore.GetInstance(sess.InstanceID)
		if err != nil {
			http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusInternalServerError)
			return
		}
		credID := resolveCredential(s.harnessStore, inst.ID)
		if _, startErr := s.startOnInstance(r.Context(), sess, inst, credID); startErr != nil {
			http.Error(w, fmt.Sprintf("failed to start harness: %v", startErr), http.StatusInternalServerError)
			return
		}
	}

	userEvent := msg.Event{
		Type:            msg.EventUserMessage,
		BridgeSessionID: bridgeID,
		ClientRequestID: req.ClientRequestID,
		Timestamp:       time.Now(),
		Result:          &msg.ResultEvent{Text: req.Message, Blocks: req.Blocks},
	}
	// Persist before forwarding to the harness. If either store can't take
	// the user_message, refuse the send — otherwise the assistant runs against
	// a prompt that's missing from the durable log (log-store is what the
	// /messages endpoint reads). Caller can retry safely; the harness has not
	// seen the message yet.
	if _, err := s.harness.BroadcastEvent(&userEvent); err != nil {
		http.Error(w, fmt.Sprintf("persist user_message: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.harness.PushEvent(userEvent); err != nil {
		http.Error(w, fmt.Sprintf("push user_message to log-store: %v", err), http.StatusInternalServerError)
		return
	}

	if name := displayNameFromMessage(req.Message); name != "" {
		if _, err := s.store.SetDisplayNameIfEmpty(bridgeID, name); err != nil {
			log.Printf("[session] failed to set display_name from first message: %v", err)
		}
	}

	if err := s.harness.Send(bridgeID, req.Message, req.Blocks); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	go s.maybeAutoRename(bridgeID)

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

	if sess.InstanceID == "" || s.harnessStore == nil {
		http.Error(w, "session has no instance bound", http.StatusInternalServerError)
		return
	}
	inst, err := s.harnessStore.GetInstance(sess.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusInternalServerError)
		return
	}
	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, err := s.startOnInstance(r.Context(), sess, inst, credID); err != nil {
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

	if parent.InstanceID == "" || s.harnessStore == nil {
		http.Error(w, "parent session has no instance bound", http.StatusInternalServerError)
		return
	}
	if parent.HarnessSessionID == "" {
		http.Error(w, "parent session has no harness_session_id yet (not initialized)", http.StatusConflict)
		return
	}
	inst, err := s.harnessStore.GetInstance(parent.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("instance not found: %v", err), http.StatusInternalServerError)
		return
	}

	// ParentID feeds buildStartParams' params.Fork, which the harness uses
	// as the --resume / thread-fork target. Harnesses need the parent's
	// harness UUID, not the bridge_session_id (cf. renamer.go's note that
	// parent_id is "CC-fork plumbing"). The forward link to the parent's
	// stable bridge_id is implied by the chain in the harness's state.db.
	// Inherit Type / Purpose / Origin from parent unless caller overrides —
	// a fork of an interactive session is itself interactive; an autonomous
	// worker forking off a sub-task stays autonomous; the originator of the
	// fork is the parent's originator unless an explicit override is given.
	sessionType := req.Type
	if sessionType == "" {
		sessionType = parent.Type
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = parent.Purpose
	}
	origin := req.Origin
	if origin == "" {
		origin = parent.Origin
	}
	forkedID := generateBridgeID()
	forked := &store.Session{
		SessionID:   forkedID,
		DisplayName: displayName,
		Harness:     parent.Harness,
		InstanceID:  parent.InstanceID,
		State:       string(msg.SessionIdle),
		AgentID:     parent.AgentID,
		SpawnerID:   parent.SpawnerID,
		ParentID:    parent.HarnessSessionID,
		Type:        sessionType,
		Purpose:     purpose,
		Origin:      origin,
	}

	if err := s.store.CreateSession(forked); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	credID := resolveCredential(s.harnessStore, inst.ID)
	if _, err := s.startOnInstance(context.Background(), forked, inst, credID); err != nil {
		s.store.UpdateSessionState(forked.SessionID, string(msg.SessionError))
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
	localInstances := s.localInstancesByHarness([]msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex})

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
		// Prefer the adapter's structural source tag (e.g. codex marks
		// conformance-leaked chains by bridge_session_id prefix; claudecode
		// marks Task()-spawned subagents from the on-disk layout) over our
		// prompt-prefix heuristic. Fall back to prefix inference only when
		// the adapter has no structural signal. Mirrors AutoDiscover.
		source, folder := ds.Source, s.folderForSource(ds.Source)
		if source == "" {
			source, folder = s.discoverySourceFolder(ds.Prompt)
		}

		bridgeID, inserted, err := s.store.UpsertDiscoveredSession(
			ds.HarnessSessionID,
			ds.BridgeSessionID,
			displayName,
			string(ds.Harness),
			instanceID,
			source,
			folder,
			ds.CreatedAt,
			ds.UpdatedAt,
		)
		if err != nil {
			log.Printf("[discover] failed to upsert session %s: %v", ds.HarnessSessionID, err)
			continue
		}
		if inserted {
			imported++
			// Import history to log-store for new sessions
			go func(h msg.Harness, brID, sid string) {
				n, err := s.harness.ImportHistory(context.Background(), brID, h, sid)
				if err != nil {
					log.Printf("[discover] failed to import history for %s: %v", sid, err)
				} else if n > 0 {
					log.Printf("[discover] imported %d events for session %s", n, sid)
				}
			}(ds.Harness, bridgeID, ds.HarnessSessionID)
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

