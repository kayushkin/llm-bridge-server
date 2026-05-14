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
// no parked entry. Mirrors broadcastPrehookResolved but stamps the message
// to explain the no-op so the audit log is self-describing. The original
// Source is unrecoverable at this point (parked entry already gone), so
// stale completions are stamped as permission_prompt — UI consumers only
// pair on request_id, not source, so the mislabel is audit-only.
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

// permissionModeRequest is the body of POST /bridge/permission-mode and
// PUT /sessions/{id}/permission-mode. The prehook reads the persisted
// value on every request, so the new mode takes effect immediately for
// every session — no harness broadcast needed.
//
// DisableNetwork is an orthogonal side-axis that lives alongside the mode
// for atomicity (one HTTP roundtrip changes both). nil means "leave the
// existing value alone"; pointer-bool lets the caller omit the field
// without it defaulting to false.
//
// PermissionModeCustom carries the raw harness-specific knobs the user
// picked in Custom mode (codex: approval + sandbox; other harnesses
// surface their own subset). Only honored when Mode == "custom"; the
// bridge writes the struct to HarnessConfig.permission_mode_custom and
// the harness reads it on next spawn. nil leaves any existing value
// alone; an empty struct clears it.
type permissionModeRequest struct {
	Mode                 string                       `json:"mode"`
	DisableNetwork       *bool                        `json:"disable_network,omitempty"`
	PermissionModeCustom *permissionModeCustomConfig  `json:"permission_mode_custom,omitempty"`
}

// permissionModeCustomConfig is the raw-knob payload Custom mode carries
// through to the harness. Codex consumes Approval + Sandbox; other
// harnesses get to define their own subset later.
type permissionModeCustomConfig struct {
	Approval string `json:"approval,omitempty"`
	Sandbox  string `json:"sandbox,omitempty"`
}

// bypassPermissionsRequest is the legacy body of POST /bridge/bypass-permissions
// and PUT /sessions/{id}/bypass-permissions. Retained as a thin alias that
// translates the boolean into the new mode enum so older bridge-ui builds
// keep working during the transition.
type bypassPermissionsRequest struct {
	Enabled bool `json:"enabled"`
}

func validPermissionMode(m string) bool {
	switch m {
	case msg.PermissionModeBlockAll,
		msg.PermissionModePlan,
		msg.PermissionModeRead,
		msg.PermissionModeAskAll,
		msg.PermissionModeAsk,
		msg.PermissionModeAuto,
		msg.PermissionModeBypass,
		msg.PermissionModeCustom:
		return true
	}
	return false
}

// permissionModeAllowedValues is the human-readable enum for 400 responses.
// Kept in lockstep with validPermissionMode so the error message stays
// honest about what's accepted.
const permissionModeAllowedValues = "block_all|plan|read|ask_all|ask|auto|bypass|custom"

// handleSetGlobalPermissionMode persists the global permission_mode in
// bridge-prefs. The prehook reads bridge-prefs on every call so the
// change takes effect immediately — no harness broadcast needed.
func (s *Server) handleSetGlobalPermissionMode(w http.ResponseWriter, r *http.Request) {
	var req permissionModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !validPermissionMode(req.Mode) {
		http.Error(w, "mode must be one of "+permissionModeAllowedValues, http.StatusBadRequest)
		return
	}

	s.bridgePrefs.setPermissionMode(req.Mode)

	writeJSON(w, map[string]any{
		"status": "ok",
		"mode":   req.Mode,
	})
}

// handleSetSessionPermissionMode persists the per-session permission_mode
// into harness_config. Survives harness restart and beats the global.
// The prehook reads this live, so the override takes effect on the next
// tool call without restarting the harness; the harness picks up the
// change on its next spawn/resume.
func (s *Server) handleSetSessionPermissionMode(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req permissionModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !validPermissionMode(req.Mode) {
		http.Error(w, "mode must be one of "+permissionModeAllowedValues, http.StatusBadRequest)
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			http.Error(w, "harness_config unparseable: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	cfg["permission_mode"] = json.RawMessage(`"` + req.Mode + `"`)
	delete(cfg, "bypass_permissions")
	if req.DisableNetwork != nil {
		if *req.DisableNetwork {
			cfg["disable_network"] = json.RawMessage(`true`)
		} else {
			// Explicit false → delete the key so harness defaults apply.
			// Distinguish from "omitted" (nil) where we leave it alone.
			delete(cfg, "disable_network")
		}
	}
	if req.PermissionModeCustom != nil {
		if req.PermissionModeCustom.Approval == "" && req.PermissionModeCustom.Sandbox == "" {
			// Empty struct → clear stored value.
			delete(cfg, "permission_mode_custom")
		} else {
			raw, err := json.Marshal(req.PermissionModeCustom)
			if err != nil {
				http.Error(w, "marshal permission_mode_custom: "+err.Error(), http.StatusInternalServerError)
				return
			}
			cfg["permission_mode_custom"] = raw
		}
	}
	merged, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "marshal harness_config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateSessionHarnessConfig(bridgeID, merged); err != nil {
		http.Error(w, "persist harness_config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"status":          "ok",
		"bridge_id":       bridgeID,
		"permission_mode": req.Mode,
	}
	if req.DisableNetwork != nil {
		resp["disable_network"] = *req.DisableNetwork
	}
	writeJSON(w, resp)
}

// handleSetBypassPermissions is the legacy alias for the global
// permission-mode endpoint. Translates the boolean (true → bypass, false →
// ask) and delegates. Retained so older bridge-ui builds keep working
// until they're rebuilt against the new endpoint.
func (s *Server) handleSetBypassPermissions(w http.ResponseWriter, r *http.Request) {
	var req bypassPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	s.bridgePrefs.setPermissionMode(bypassBoolToMode(req.Enabled))
	writeJSON(w, map[string]any{
		"status":  "ok",
		"enabled": req.Enabled,
	})
}

// handleSetSessionBypass is the legacy alias for the per-session
// permission-mode endpoint.
func (s *Server) handleSetSessionBypass(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req bypassPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			http.Error(w, "harness_config unparseable: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	cfg["permission_mode"] = json.RawMessage(`"` + bypassBoolToMode(req.Enabled) + `"`)
	delete(cfg, "bypass_permissions")
	merged, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "marshal harness_config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateSessionHarnessConfig(bridgeID, merged); err != nil {
		http.Error(w, "persist harness_config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"status":             "ok",
		"bridge_id":          bridgeID,
		"bypass_permissions": req.Enabled,
	})
}
