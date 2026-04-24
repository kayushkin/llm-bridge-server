package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"time"

	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge/msg"
)

// hookExecTimeout is the maximum wall-clock time a registered hook's shell
// command may run before the server kills it. Matches Claude Code's default
// hook timeout so behavior is consistent whether CC runs the hook directly or
// via the bridge.
const hookExecTimeout = 60 * time.Second

// handleCreateHook registers a new hook. Body: the fields of msg.Hook except
// id, timestamps — those are server-assigned.
func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	var req msg.Hook
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := validateHook(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.ID = ids.NewHookID()
	req.Enabled = true
	if err := s.hookStore.CreateHook(&req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, req)
}

// handleListHooks lists hooks. Query params (all optional): harness, event,
// scope_kind, scope_id, enabled (true|false).
func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := hookstore.ListFilter{
		Harness:   msg.Harness(q.Get("harness")),
		Event:     q.Get("event"),
		ScopeKind: msg.HookScope(q.Get("scope_kind")),
		ScopeID:   q.Get("scope_id"),
	}
	if v := q.Get("enabled"); v != "" {
		filter.EnabledSet = true
		filter.Enabled = v == "true" || v == "1"
	}
	hooks, err := s.hookStore.ListHooks(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hooks == nil {
		hooks = []msg.Hook{}
	}
	writeJSON(w, hooks)
}

func (s *Server) handleGetHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, err := s.hookStore.GetHook(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "hook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, hook)
}

// handleUpdateHook merges the supplied fields onto an existing hook. Fields
// left empty in the request body keep their existing value. The special case
// of toggling `enabled` is supported via PATCH with just that field set.
func (s *Server) handleUpdateHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.hookStore.GetHook(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "hook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var patch struct {
		Harness   *msg.Harness   `json:"harness,omitempty"`
		Event     *string        `json:"event,omitempty"`
		Matcher   *string        `json:"matcher,omitempty"`
		Command   *string        `json:"command,omitempty"`
		ScopeKind *msg.HookScope `json:"scope_kind,omitempty"`
		ScopeID   *string        `json:"scope_id,omitempty"`
		Enabled   *bool          `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if patch.Harness != nil {
		existing.Harness = *patch.Harness
	}
	if patch.Event != nil {
		existing.Event = *patch.Event
	}
	if patch.Matcher != nil {
		existing.Matcher = *patch.Matcher
	}
	if patch.Command != nil {
		existing.Command = *patch.Command
	}
	if patch.ScopeKind != nil {
		existing.ScopeKind = *patch.ScopeKind
	}
	if patch.ScopeID != nil {
		existing.ScopeID = *patch.ScopeID
	}
	if patch.Enabled != nil {
		existing.Enabled = *patch.Enabled
	}
	if err := validateHook(existing); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.hookStore.UpdateHook(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, existing)
}

func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.hookStore.DeleteHook(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "hook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleExecHook is invoked by the underlying harness (via curl from the
// synthesized settings JSON) when a registered hook fires. The request body
// is the harness's native hook stdin payload, passed through verbatim to the
// registered shell command. The command's stdout is written back as the
// response — the harness reads it in its native dialect with no translation.
//
// Side effect: a HookEvent{Phase="started"} and HookEvent{Phase="completed"}
// are emitted onto the session's canonical event stream so the UI and other
// downstream subscribers observe the registered invocation just like a
// native-observed hook.
func (s *Server) handleExecHook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hook, err := s.hookStore.GetHook(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "hook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !hook.Enabled {
		http.Error(w, "hook disabled", http.StatusGone)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var meta struct {
		SessionID string `json:"session_id"`
		ToolName  string `json:"tool_name"`
	}
	_ = json.Unmarshal(body, &meta)

	s.broadcastHookEvent(hook, meta.SessionID, meta.ToolName, &msg.HookEvent{
		Event:    hook.Event,
		Matcher:  hook.Matcher,
		ToolName: meta.ToolName,
		HookID:   hook.ID,
		Phase:    "started",
		Input:    json.RawMessage(body),
	})

	// Capture before/after file state for Edit/Write alongside the user's
	// hook command. Runs concurrently so git-hash-object latency doesn't
	// extend the exec response time.
	go s.maybeCaptureSnapshot(hook.Event, meta.SessionID, body)

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), hookExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", hook.Command)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	duration := time.Since(start).Milliseconds()

	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		exitCode = exitErr.ExitCode()
	default:
		// Command failed to launch or context expired — mark with -1 so
		// downstream consumers can distinguish from a real exit code.
		exitCode = -1
	}

	var outputJSON json.RawMessage
	var decision string
	if out := stdout.Bytes(); len(out) > 0 && json.Valid(out) {
		outputJSON = json.RawMessage(out)
		var d struct {
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(out, &d)
		decision = d.Decision
	}
	hookErr := ""
	if runErr != nil && exitCode <= 0 {
		hookErr = runErr.Error()
		if es := stderr.String(); es != "" {
			hookErr = hookErr + ": " + es
		}
	}

	s.broadcastHookEvent(hook, meta.SessionID, meta.ToolName, &msg.HookEvent{
		Event:      hook.Event,
		Matcher:    hook.Matcher,
		ToolName:   meta.ToolName,
		HookID:     hook.ID,
		Phase:      "completed",
		Output:     outputJSON,
		Decision:   decision,
		ExitCode:   exitCode,
		DurationMS: duration,
		Error:      hookErr,
	})

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(stdout.Bytes()); err != nil {
		return
	}
}

// broadcastHookEvent persists and fans out a HookEvent on the session's
// canonical stream. No-op when sessionID is empty (no session to target).
func (s *Server) broadcastHookEvent(hook *msg.Hook, sessionID, toolName string, h *msg.HookEvent) {
	if sessionID == "" {
		return
	}
	_, _ = s.harness.BroadcastEvent(&msg.Event{
		Type:      msg.EventHook,
		Harness:   hook.Harness,
		SessionID: sessionID,
		Timestamp: time.Now().UTC(),
		Hook:      h,
	})
}

// validateHook checks the fields that are structurally required for any
// persisted hook. CHECK constraints at the storage layer reject malformed
// scopes as a backstop; doing it here produces a 400 with a clearer message.
func validateHook(h *msg.Hook) error {
	if h.Harness == "" {
		return errors.New("harness is required")
	}
	if h.Event == "" {
		return errors.New("event is required")
	}
	if h.Command == "" {
		return errors.New("command is required")
	}
	switch h.ScopeKind {
	case msg.HookScopeGlobal:
		// scope_id must be empty for global
	case msg.HookScopeInstance, msg.HookScopeSession:
		if h.ScopeID == "" {
			return errors.New("scope_id is required for instance/session scope")
		}
	default:
		return errors.New("scope_kind must be global | instance | session")
	}
	return nil
}
