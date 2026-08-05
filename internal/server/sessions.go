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
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// folderForPurpose returns the effective folder for a purpose, or "" if the
// purpose is unmapped (leaving the session unfiled).
//
// Three layers, most specific first: the user's runtime overrides in the
// source_folders table, then LLMBRIDGE_PURPOSE_FOLDERS, then the folder the
// purpose registry declares. DB errors fall back to the lower layers — failing
// closed (returning "") would silently un-file new sessions.
//
// A superseded purpose resolves through to its current spelling, so sessions
// created under an old slug land in the same folder as new ones instead of
// splitting the folder in two.
func (s *Server) folderForPurpose(purpose string) string {
	if purpose == "" || s.cfg == nil {
		return ""
	}
	canonical := msg.CanonicalPurpose(purpose)
	if s.store != nil {
		if overrides, err := s.store.ListSourceFolders(); err == nil {
			if v, ok := overrides[purpose]; ok {
				return v
			}
			if v, ok := overrides[canonical]; ok {
				return v
			}
		} else {
			log.Printf("[purpose-folders] failed to load overrides: %v", err)
		}
	}
	if v, ok := s.cfg.PurposeFolders[purpose]; ok {
		return v
	}
	if v, ok := s.cfg.PurposeFolders[canonical]; ok {
		return v
	}
	// The registry is the floor, consulted directly rather than only through
	// the config map. config.Load() seeds that map from the registry, but a
	// Config built any other way — a test, an embedder — would otherwise file
	// nothing at all, and silently: every session would come back unfiled with
	// no error to explain it.
	return msg.FolderForPurpose(canonical)
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
		return conformance.SourceTag, s.folderForPurpose(conformance.SourceTag)
	}
	for _, p := range discoveryPromptPrefixes {
		if strings.HasPrefix(prompt, p.prefix) {
			return p.source, s.folderForPurpose(p.source)
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

	// Forensic logging: capture who created this session so we can identify
	// mystery clients (e.g. agent-driven Playwright runs against dash that
	// leave behind purposeless "hi" / "say hi" / "test" chats). Logged at
	// decode time so even rejected requests are visible.
	log.Printf("[sessions.create] ua=%q referer=%q xff=%q remote=%s harness=%s type=%s purpose=%s origin=%s display_name=%q session_id=%q",
		r.Header.Get("User-Agent"),
		r.Header.Get("Referer"),
		r.Header.Get("X-Forwarded-For"),
		r.RemoteAddr,
		req.Harness, req.Type, req.Purpose, req.Origin,
		req.DisplayName, req.SessionID,
	)

	h := msg.Harness(req.Harness)
	if !isValidHarness(h) {
		http.Error(w, "invalid harness", http.StatusBadRequest)
		return
	}

	// Classification. Type and Origin are rejected when absent or unknown;
	// Purpose is checked against the registry but never rejected.
	//
	// The split is deliberate. A type outside the enum cannot be stored
	// honestly — permission gating, folder routing and every frontend branch
	// on it — and an empty origin loses the only record of who asked for the
	// session, which is unrecoverable after the fact. Both are cheap for a
	// caller to get right and were already documented as required.
	//
	// Purpose is different: the registry is a list of slugs we happen to know
	// about, and a caller inventing a new one is how the list grows. Refusing
	// those would make adding a feature require a change to this repo first.
	// So an unknown purpose is stored and logged, and session-taxonomy-guard
	// reports it — visible, not fatal.
	if !msg.ValidSessionType(req.Type) {
		writeJSONError(w, http.StatusBadRequest, "invalid_session_type", fmt.Sprintf(
			"type must be one of %v, got %q — see SessionType in llm-bridge/msg/server.go",
			msg.SessionTypes(), req.Type))
		return
	}
	if strings.TrimSpace(req.Origin) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_origin",
			"origin is required: name the service or script creating this session (e.g. \"scheduler\", \"healthcheck\", \"frontend-dash\")")
		return
	}
	if problems := msg.ClassifyPurpose(req.Type, req.Purpose, req.Origin); len(problems) > 0 {
		for _, p := range problems {
			log.Printf("[sessions.create] taxonomy %s: %s (want=%q got=%q) type=%s purpose=%s origin=%s",
				p.Kind, p.Detail, p.Want, p.Got, req.Type, req.Purpose, req.Origin)
		}
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

	// Spend ceiling. Absent and zero both mean "no ceiling" — see the
	// warning on msg.ManagedSession.MaxBudgetUSD — so only a negative
	// value is an error. It is rejected rather than clamped because a
	// negative ceiling is always a caller bug, and silently reading it as
	// "unlimited" would turn an attempt to cap spending into the absence
	// of a cap, which is the one direction this gate must never fail in.
	maxBudgetUSD := 0.0
	if req.MaxBudget != nil {
		if *req.MaxBudget < 0 {
			http.Error(w, `{"error":{"code":"invalid_max_budget","message":"max_budget must not be negative; omit it or send 0 for no ceiling"}}`, http.StatusBadRequest)
			return
		}
		maxBudgetUSD = *req.MaxBudget
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
		HarnessConfig: req.HarnessConfig,
		Purpose:       req.Purpose,
		Type:          req.Type,
		Origin:        req.Origin,
		FolderName:    s.folderForPurpose(req.Purpose),
		Mode:          mode,
		MaxBudgetUSD:  maxBudgetUSD,
		WorkingDir:    req.WorkingDir,
	}

	// Snapshot the global permission mode into the session so the
	// per-session value is durable from creation onward. Skipped if the
	// caller already set permission_mode (or legacy bypass_permissions) in
	// req.HarnessConfig.
	s.snapshotPermissionModeIntoSession(sess)

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
			sess.State = string(msg.SessionStarting)
		}
	}

	w.WriteHeader(http.StatusCreated)

	// PTY sessions need an attach token so the browser can hand it to the
	// /attach WS upgrade. The token lives only on the in-memory AttachHub
	// (it dies with the pty), so we serialize it alongside the persisted
	// session via an inline-embedded wrapper rather than adding a transient
	// field to ManagedSession.
	if sess.Mode == msg.SessionModePTY {
		if hub := s.harness.AttachHubFor(sess.SessionID); hub != nil {
			writeJSON(w, createSessionResponse{Session: sess, AttachToken: hub.Token()})
			return
		}
	}
	writeJSON(w, sess)
}

// createSessionResponse is the JSON shape returned by POST /sessions. It
// embeds *store.Session so existing fields are unchanged for events-mode
// callers; AttachToken is set only for pty sessions that started cleanly.
type createSessionResponse struct {
	*store.Session
	AttachToken string `json:"attach_token,omitempty"`
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

	// Refuse to spend past the session's ceiling. Checked before the
	// harness is started below, so an over-budget session does not get a
	// process spawned for a message it will not be allowed to send.
	if s.writeRefusalIfOverBudget(w, bridgeID) {
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
	// seen the message yet. BroadcastEvent now writes both stores and returns
	// the log-store error if it fails.
	if _, err := s.harness.BroadcastEvent(&userEvent); err != nil {
		http.Error(w, fmt.Sprintf("persist user_message: %v", err), http.StatusInternalServerError)
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

	// Replying to a chat that was marked done reopens it: pull it back out of
	// the Archive folder so it rejoins the active list. This mirrors, for the
	// folder half of "done", the state half that re-engagement already undoes
	// through derivation (see handleMarkSessionDone). State is owned by the
	// derivation pipeline, so we deliberately touch only the folder here.
	if sess.FolderName == store.ArchiveFolder {
		if err := s.store.SetSessionFolder(bridgeID, ""); err != nil {
			log.Printf("[session] failed to reopen archived session %s on reply: %v", bridgeID, err)
		}
	}

	go s.maybeAutoRename(bridgeID)

	writeJSON(w, map[string]string{"status": "sent", "message_id": userEvent.MessageID})
}

// writeRefusalIfOverBudget refuses the request when the session has spent its
// spend ceiling, writing the refusal and reporting true so the caller
// returns. Reports false — write nothing — for every session that is
// under its ceiling or has none.
//
// 402 Payment Required is the honest status: the request is well formed
// and the caller is permitted to make it, and the only thing standing in
// the way is money. The body names both numbers so a client can say what
// happened without a second round trip.
func (s *Server) writeRefusalIfOverBudget(w http.ResponseWriter, bridgeID string) bool {
	over, spendUSD, maxBudgetUSD := s.harness.SessionOverBudget(bridgeID)
	if !over {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":           msg.ErrCodeBudgetExceeded,
			"message":        fmt.Sprintf("session has spent $%.2f of its $%.2f ceiling; raise max_budget to continue", spendUSD, maxBudgetUSD),
			"spend_usd":      spendUSD,
			"max_budget_usd": maxBudgetUSD,
		},
	})
	return true
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

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			s.harness.Unsubscribe(bridgeID, events)
			return
		case <-keepalive.C:
			// An idle session emits nothing between turns, so keep the stream
			// warm or an intermediary's idle read timeout will reap it. See
			// handleSessionListEvents for the full rationale.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				s.harness.Unsubscribe(bridgeID, events)
				return
			}
			flusher.Flush()
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

	// The live process registry — not the denormalised state cache — is the
	// authority on whether there is anything to interrupt. Stop() returns
	// "session not running" when no live process exists; surface that as the
	// 409. Interrupting a live-but-idle process is a harmless no-op. The old
	// `sess.State != running` guard 409'd whenever the session was in
	// tool_running (a tool in flight — the most common moment a user hits
	// Stop), silently failing the interrupt. See
	// docs/findings/2026-07-27-interrupt-dual-emit-turn-hijack.md §2/§5.
	if err := s.harness.Stop(bridgeID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// `paused` — "user-interrupted, can be resumed" — not `idle`. The two
	// are different facts: idle is a turn that finished on its own terms,
	// paused is one a person stopped. Writing idle threw that away, so
	// every client that wanted to show it had to remember locally what it
	// had asked for; bridge-ui kept a localStorage set of interrupted ids
	// for exactly this reason.
	//
	// Through the manager rather than straight to the store, so the
	// derivation moves with the row and SSE subscribers hear it live.
	// See docs/findings/2026-07-27-interrupt-dual-emit-turn-hijack.md §7.
	//
	// NOTE: paused here means the TURN was stopped, not that the process
	// died. `Manager.Stop` calls `proc.Interrupt()` and leaves the process
	// registered, so a paused session still has a live harness and is NOT
	// resumable — resume 409s on the process registry, and sending the
	// next message is what continues it.
	s.harness.ForceSessionState(bridgeID, msg.SessionPaused, "user_interrupt")

	sess.State = string(msg.SessionPaused)
	writeJSON(w, sess)
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// The live process registry is the authority on whether the session is
	// resumable, not the denormalised state cache. A session with a live
	// process is already running and has nothing to resume (409); one with
	// no live process is resumable regardless of a stale state string — a
	// crashed or interrupted session can read "running"/"error" yet have no
	// process. The old `sess.State != idle` guard mis-classified both. Same
	// cache-as-authority mistake as interrupt; see
	// docs/findings/2026-07-27-interrupt-dual-emit-turn-hijack.md §2/§5.
	if s.harness.Get(bridgeID) != nil {
		http.Error(w, "session already running", http.StatusConflict)
		return
	}

	// A session that spent its ceiling does not come back by being
	// resumed. Raise the ceiling first (POST /sessions/{id}/config).
	if s.writeRefusalIfOverBudget(w, bridgeID) {
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

	sess.State = string(msg.SessionStarting)
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

	params, err := harness.BuildCompactParams(bridgeID, req.Summary)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.harness.SendJSONRPC(bridgeID, "compact", params); err != nil {
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
		// ParentID carries the parent's HARNESS uuid — the value `--fork` needs.
		// ForkedFromSessionID is the honest lineage link: the parent's bridge id.
		// Once the fork plumbing resolves the harness id from the parent row,
		// ParentID goes away (§21).
		ParentID:            parent.HarnessSessionID,
		ForkedFromSessionID: parent.SessionID,
		Type:                sessionType,
		Purpose:             purpose,
		Origin:              origin,
		// Inherit the parent's spend ceiling. Not inheriting it would make
		// forking the way to get an uncapped session out of a capped one,
		// which is the whole gate defeated by one button. The fork does
		// start its own spend at zero, so a capped session can still be
		// forked into a second full allowance — a per-tree pot, rather
		// than a per-session one, is the follow-up this does not attempt.
		MaxBudgetUSD: parent.MaxBudgetUSD,
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
		forked.State = string(msg.SessionStarting)
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, forked)
}

// carriesHarnessConfig reports whether a config request asks the harness to
// change anything, as opposed to only moving the spend ceiling, which is server
// state and never leaves this process.
//
// DisabledTools is tested for nil, not for length. It is the whole set of names
// to exclude, so an empty list is the request that re-enables every tool — the
// loudest thing this endpoint can say about a tool set, and a length test reads
// it as silence. A budget-only fast path that swallowed it would drop the tool
// change on exactly the sessions the escape hatch exists to revive.
func carriesHarnessConfig(req ConfigSessionRequest) bool {
	return req.Model != "" || req.Effort != "" || req.DisabledTools != nil
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

	// The spend ceiling is server state, not harness state, so it is
	// persisted here as well as forwarded. This is also the only way to
	// revive a session the gate has halted: raising the ceiling above the
	// spend puts it back under, and the next send is allowed through.
	// Setting it to 0 removes the ceiling entirely.
	if req.MaxBudget != nil {
		if *req.MaxBudget < 0 {
			http.Error(w, `{"error":{"code":"invalid_max_budget","message":"max_budget must not be negative; send 0 to remove the ceiling"}}`, http.StatusBadRequest)
			return
		}
		if err := s.store.SetSessionMaxBudgetUSD(bridgeID, *req.MaxBudget); err != nil {
			http.Error(w, fmt.Sprintf("persist max_budget: %v", err), http.StatusInternalServerError)
			return
		}
		sess.MaxBudgetUSD = *req.MaxBudget

		// Raising the ceiling on a session with no live process is the
		// normal shape of the escape hatch — the gate interrupted it, so
		// there is nothing left to forward the config to. Forwarding
		// anyway would fail with "session not running" and report the
		// revival as an error even though the ceiling was saved. Only
		// budget-only requests take this path; anything that also
		// carries harness config still needs a harness to carry it to.
		if !carriesHarnessConfig(req) && s.harness.Get(bridgeID) == nil {
			writeJSON(w, sess)
			return
		}
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
	var imported, linkedCount int
	var pendingLinks []discoveredLink
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
		source, folder := ds.Source, s.folderForPurpose(ds.Source)
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
		// Lineage is resolved after every row exists — discovery has no
		// parent-before-child ordering, so linking inline drops every subagent
		// walked before its parent. Mirrors AutoDiscover.
		if ds.ParentHarnessSessionID != "" {
			pendingLinks = append(pendingLinks, discoveredLink{bridgeID: bridgeID, parentHarnessID: ds.ParentHarnessSessionID})
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
	for _, l := range pendingLinks {
		linked, err := s.store.LinkDiscoveredSessionParent(l.bridgeID, l.parentHarnessID)
		if err != nil {
			log.Printf("[discover] failed to link %s to parent %s: %v", l.bridgeID, l.parentHarnessID, err)
		} else if linked {
			linkedCount++
		}
	}
	if linkedCount > 0 {
		log.Printf("[discover] linked %d sessions to the parent that spawned them", linkedCount)
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
