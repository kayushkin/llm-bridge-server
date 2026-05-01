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

// permissionModeRequest is the body of POST /sessions/{id}/permission-mode.
// Mirrors the harness's set_permission_mode JSON-RPC params.
type permissionModeRequest struct {
	Mode string `json:"mode"`
}

// handleSetPermissionMode forwards a runtime permission-mode change to the
// harness via the existing set_permission_mode JSON-RPC method. For Claude
// Code the meaningful values are "default" (consult the permission-prompt
// tool / bridge-ui) and "bypassPermissions" (auto-approve everything);
// "acceptEdits" and "plan" are passthroughs.
func (s *Server) handleSetPermissionMode(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	if _, err := s.store.GetSession(bridgeID); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	var req permissionModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Mode == "" {
		http.Error(w, "mode is required", http.StatusBadRequest)
		return
	}
	params, err := json.Marshal(req)
	if err != nil {
		http.Error(w, "marshal set_permission_mode params: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.harness.SendJSONRPC(bridgeID, "set_permission_mode", params); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "set", "mode": req.Mode})
}
