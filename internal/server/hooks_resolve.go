package server

import (
	"encoding/json"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

// hookResolveRequest is the body of POST /sessions/{id}/hooks/{request_id}/resolve.
// Mirrors the shape the harness's resolve_hook JSON-RPC method consumes.
type hookResolveRequest struct {
	Behavior     string          `json:"behavior"` // "allow" | "deny"
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	Message      string          `json:"message,omitempty"`
	ResolvedBy   string          `json:"resolved_by,omitempty"` // "user" / "auto" / "allow_always" / ...
}

// resolveHookParams is what the harness expects on stdin. Identical layout
// to llm-bridge-claudecode's ResolveHookParams; we keep our own copy so
// bridge-server never imports a harness package.
type resolveHookParams struct {
	RequestID    string          `json:"request_id"`
	Behavior     string          `json:"behavior"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	Message      string          `json:"message,omitempty"`
	ResolvedBy   string          `json:"resolved_by,omitempty"`
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

// handleResolveHook accepts a decision for an awaiting_resolution hook
// and forwards it to the harness via the resolve_hook JSON-RPC method.
// The harness is responsible for both replying to the parked tool/call
// (closing CC's permission prompt) and emitting the matching
// phase="completed" HookEvent.
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

	params, err := json.Marshal(resolveHookParams{
		RequestID:    requestID,
		Behavior:     req.Behavior,
		UpdatedInput: req.UpdatedInput,
		Message:      req.Message,
		ResolvedBy:   req.ResolvedBy,
	})
	if err != nil {
		http.Error(w, "marshal resolve_hook params: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.harness.SendJSONRPC(bridgeID, "resolve_hook", params); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "resolved"})
}

// bypassPermissionsRequest is the body of POST /bridge/bypass-permissions.
// When Enabled is true, every active claudecode harness flips its embedded
// permission MCP into bypass mode (every tool call resolves to allow without
// consulting permission-store). The caller is also expected to update
// bridge-prefs so newly created sessions launch with --permission-mode
// bypassPermissions for the same effect.
type bypassPermissionsRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetBypassPermissions broadcasts set_bypass_permissions to every
// running harness. Harnesses that don't recognize the method (anything
// other than claudecode today) respond with "unknown method"; we collect
// those errors and surface them in the response without failing the call,
// since the broadcast is intentionally best-effort.
func (s *Server) handleSetBypassPermissions(w http.ResponseWriter, r *http.Request) {
	var req bypassPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	params, err := json.Marshal(map[string]bool{"enabled": req.Enabled})
	if err != nil {
		http.Error(w, "marshal set_bypass_permissions params: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sessions := s.harness.ListActiveSessions()
	failures := map[string]string{}
	for _, sid := range sessions {
		if err := s.harness.SendJSONRPC(sid, "set_bypass_permissions", params); err != nil {
			failures[sid] = err.Error()
		}
	}
	writeJSON(w, map[string]any{
		"status":          "broadcast",
		"enabled":         req.Enabled,
		"active_sessions": len(sessions),
		"failures":        failures,
	})
}
