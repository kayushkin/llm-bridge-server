package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// hookResolveRequest is the body of POST /sessions/{id}/hooks/{request_id}/resolve.
type hookResolveRequest struct {
	Behavior     string          `json:"behavior"` // "allow" | "deny"
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	Message      string          `json:"message,omitempty"`
	ResolvedBy   string          `json:"resolved_by,omitempty"` // "user" / "auto" / "allow_always" / ...
}

// handleListPendingHooks returns the awaiting_resolution HookEvents
// currently outstanding for a session, so bridge-ui can hydrate the
// pending-hook banner on a fresh connection without replaying the full
// SSE stream.
func (s *Server) handleListPendingHooks(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	pending := s.harness.PendingHooks(bridgeID)
	if pending == nil {
		pending = []msg.Event{}
	}
	writeJSON(w, pending)
}

// handleResolveHook delivers a decision for an awaiting_resolution hook to
// the parked PreToolUse handler. The prehook handler that's blocked on the
// channel emits the matching phase=completed HookEvent and returns the
// hookSpecificOutput response to Claude Code.
//
// If no parked entry exists (stale resolve after harness restart, the parked
// request was already canceled, or a duplicate click), we still emit a
// completed HookEvent so the UI banner clears — the underlying tool call is
// dead but the user's decision is recorded for audit.
func (s *Server) handleResolveHook(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	requestID := r.PathValue("request_id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if requestID == "" {
		http.Error(w, "request_id is required", http.StatusBadRequest)
		return
	}

	var req hookResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Behavior != "allow" && req.Behavior != "deny" {
		http.Error(w, "behavior must be \"allow\" or \"deny\"", http.StatusBadRequest)
		return
	}

	decision := permissionDecision{
		Behavior:     req.Behavior,
		UpdatedInput: req.UpdatedInput,
		Message:      req.Message,
		ResolvedBy:   req.ResolvedBy,
	}
	delivered := s.parkedAsks.deliver(bridgeID, requestID, decision)
	if !delivered {
		// Stale: emit a completed event ourselves so bridge-ui's banner clears.
		// The prehook handler that originally parked this request is gone, so
		// no one else will broadcast.
		stale := decision
		stale.Message = "stale request — no parked prehook (harness restart or duplicate resolve). Decision recorded for audit; tool call is dead."
		s.broadcastStaleResolution(bridgeID, requestID, stale)
	}

	writeJSON(w, map[string]any{
		"status":          "resolved",
		"parked_delivery": delivered,
	})
}

// broadcastStaleResolution emits phase=completed for a resolve that found
// no parked entry. Mirrors broadcastPermissionResolved but stamps the message
// to explain the no-op so the audit log is self-describing.
func (s *Server) broadcastStaleResolution(bridgeID, requestID string, d permissionDecision) {
	resolution := &msg.HookResolution{
		Behavior:     d.Behavior,
		UpdatedInput: d.UpdatedInput,
		Message:      d.Message,
		ResolvedBy:   d.ResolvedBy,
	}
	if _, err := s.harness.BroadcastEvent(&msg.Event{
		Type:            msg.EventHook,
		Timestamp:       time.Now(),
		BridgeSessionID: bridgeID,
		Hook: &msg.HookEvent{
			Source:     "permission_prompt",
			Event:      "PreToolUse",
			Phase:      "completed",
			RequestID:  requestID,
			Decision:   d.Behavior,
			Resolution: resolution,
		},
	}); err != nil {
		log.Printf("[resolve] broadcast stale completed for %s/%s: %v", bridgeID, requestID, err)
	}
}

// bypassPermissionsRequest is the body of POST /bridge/bypass-permissions.
// Persists the global bypass flag in bridge-prefs. The PreToolUse prehook
// handler reads bridge-prefs on every request, so the toggle takes effect
// immediately for every session — no harness broadcast needed.
type bypassPermissionsRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetBypassPermissions persists the bypass flag in bridge-prefs.
// Pre-migration this also broadcast set_bypass_permissions to every running
// harness so the embedded permission MCP would flip into bypass mode. The
// MCP is gone now; the prehook handler reads bridge-prefs on every call,
// so persistence is the entire effect.
func (s *Server) handleSetBypassPermissions(w http.ResponseWriter, r *http.Request) {
	var req bypassPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.bridgePrefs.setBypassPermissions(req.Enabled)

	writeJSON(w, map[string]any{
		"status":  "ok",
		"enabled": req.Enabled,
	})
}
