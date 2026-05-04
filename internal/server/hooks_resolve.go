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

// handleResolveHook accepts a decision for an awaiting_resolution hook.
//
// During the MCP→PreToolUse-hook migration two delivery paths coexist:
//
//   1. The legacy MCP path: forward as a resolve_hook JSON-RPC to the
//      harness, which replies to the parked tool/call inside its embedded
//      permission MCP and emits the matching phase=completed HookEvent.
//
//   2. The new PreToolUse-hook path: deliver the decision to the parked
//      ask in the bridge-server-local parkedAsks map. The handleCCPermissionPrehook
//      handler that's blocked on the channel emits the completed HookEvent
//      and returns the response to CC.
//
// We attempt both: the JSONRPC path is a no-op when the harness has no
// matching parked request (post-step-3, when the harness drops the MCP
// entirely, this becomes "unknown method" which we ignore); the parkedAsks
// delivery is a no-op when no entry is parked. The dual-path design lets
// step 1 ship without forcing every session onto either side.
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

	delivered := s.parkedAsks.deliver(bridgeID, requestID, permissionDecision{
		Behavior:     req.Behavior,
		UpdatedInput: req.UpdatedInput,
		Message:      req.Message,
		ResolvedBy:   req.ResolvedBy,
	})

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

	jsonrpcErr := s.harness.SendJSONRPC(bridgeID, "resolve_hook", params)
	// Failures from the JSONRPC path are only fatal when nothing else
	// delivered the decision. With parkedAsks delivery the harness is
	// either gone (post-step-3) or never had the parked request — either
	// way the resolve has been served and the caller should see success.
	if jsonrpcErr != nil && !delivered {
		http.Error(w, jsonrpcErr.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"status":          "resolved",
		"parked_delivery": delivered,
	})
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

// handleSetBypassPermissions persists the bypass flag in bridge-prefs AND
// broadcasts set_bypass_permissions to every running harness. The pref is
// what BridgeChat reads on session create (to launch CC with
// --permission-mode bypassPermissions vs. default); the broadcast is what
// flips already-running sessions' embedded MCPs without a respawn.
//
// Harnesses that don't recognize the method (anything other than claudecode
// today) respond with "unknown method"; failures are surfaced in the
// response without failing the call, since the broadcast is best-effort.
func (s *Server) handleSetBypassPermissions(w http.ResponseWriter, r *http.Request) {
	var req bypassPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Persist the pref. We write directly to the prefs store rather than
	// going through the bridgePrefsStore.set() merge, because that merge
	// can't distinguish "user set false" from "field not present".
	s.bridgePrefs.setBypassPermissions(req.Enabled)

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
