package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/permclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// ccPrehookPayload is the stdin JSON Claude Code sends to PreToolUse hook
// commands. We unmarshal only what we need to evaluate; ToolUseID is kept
// for the audit log but not used for correlation (we mint our own hook
// request id so resolve flow stays uniform with the legacy MCP path).
type ccPrehookPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
}

// handleCCPermissionPrehook implements the PreToolUse permission gate for
// Claude Code, replacing the embedded MCP `approval_prompt` path. CC
// invokes this endpoint as a hook command via curl on every tool call;
// the response shape matches CC's PreToolUse hookSpecificOutput contract.
//
// Flow:
//
//   1. Decode the hook payload.
//   2. Short-circuit to allow if bridge-prefs has bypass_permissions=true.
//   3. POST to permission-store /evaluate. Transport failure collapses to
//      ask (engine default) so the human resolver sees the failure rather
//      than a silent allow.
//   4. allow/deny → respond immediately.
//   5. ask → mint a request_id, park on parkedAsks, emit an
//      awaiting_resolution HookEvent, block on the channel until
//      handleResolveHook delivers a decision (or the request context
//      cancels). Emit the matching completed HookEvent and respond.
//
// Not yet routed in mux — Step 1 ships the endpoint dark; Step 2
// prepends a curl-to-this-endpoint entry in buildClaudeCodeSettings so
// CC actually invokes it.
func (s *Server) handleCCPermissionPrehook(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("bridge_id")
	if bridgeID == "" {
		http.Error(w, "bridge_id is required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHookAsk(w, "read prehook body: "+err.Error())
		return
	}

	var payload ccPrehookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHookAsk(w, "decode prehook payload: "+err.Error())
		return
	}

	// Bypass short-circuit: per-session override (set via PATCH
	// /sessions/{id}/bypass-permissions) wins over the global toggle.
	// Both are read live on every prehook request — no harness broadcast
	// required since bridge-server is the gating point.
	sess, _ := s.store.GetSession(bridgeID)
	if s.bypassEnabledForSession(sess) {
		writeHookAllow(w, "bypass-mode")
		return
	}

	if s.permClient == nil {
		writeHookAsk(w, "permission-store client not configured")
		return
	}

	resolveCtx := r.Context()
	res, err := s.permClient.Evaluate(resolveCtx, permclient.Request{
		SessionID: bridgeID,
		Tool:     payload.ToolName,
		Input:    payload.ToolInput,
	})
	if err != nil {
		writeHookAsk(w, "permission-store unreachable: "+err.Error())
		return
	}

	switch res.Outcome {
	case "allow":
		writeHookAllow(w, res.Message)
		return
	case "deny":
		writeHookDeny(w, res.Message)
		return
	case "ask":
		// Fall through to park.
	default:
		writeHookAsk(w, "permission-store returned unknown outcome "+res.Outcome)
		return
	}

	requestID := ids.NewHookRequestID()
	ch := s.parkedAsks.park(bridgeID, requestID)

	// Emit awaiting_resolution so bridge-ui's banner picks it up via SSE.
	// BroadcastEvent records the pending hook for late-joining clients.
	if _, err := s.harness.BroadcastEvent(&msg.Event{
		Type:            msg.EventHook,
		Timestamp:       time.Now(),
		BridgeSessionID: bridgeID,
		Hook: &msg.HookEvent{
			Source:    "permission_prompt",
			Event:     "PreToolUse",
			ToolName:  payload.ToolName,
			Phase:     "awaiting_resolution",
			RequestID: requestID,
			Input:     payload.ToolInput,
		},
	}); err != nil {
		// Broadcast failure leaves the parked entry unreferenced — drop
		// it so a stale resolve doesn't deliver to a never-read channel.
		s.parkedAsks.cancel(bridgeID, requestID)
		writeHookAsk(w, "broadcast awaiting_resolution: "+err.Error())
		return
	}

	var decision permissionDecision
	select {
	case decision = <-ch:
	case <-resolveCtx.Done():
		// CC died, network drop, or the harness was killed mid-park.
		// Drop the parked entry so a later resolve doesn't deliver to a
		// dead channel; emit a synthetic completed-with-deny so the UI
		// banner clears.
		s.parkedAsks.cancel(bridgeID, requestID)
		decision = permissionDecision{
			Behavior:   "deny",
			Message:    "request canceled before resolution: " + resolveCtx.Err().Error(),
			ResolvedBy: "auto:context-canceled",
		}
		s.broadcastPermissionResolved(bridgeID, requestID, decision)
		// CC has already disconnected; writing a body here is harmless
		// but won't be observed.
		writeHookDeny(w, decision.Message)
		return
	}

	s.broadcastPermissionResolved(bridgeID, requestID, decision)

	switch decision.Behavior {
	case "allow":
		writeHookAllowWithInput(w, decision.Message, decision.UpdatedInput)
	default:
		writeHookDeny(w, decision.Message)
	}
}

// broadcastPermissionResolved emits the phase=completed HookEvent that
// closes a previously-emitted awaiting_resolution event. Symmetric with
// the harness-side handleResolveHook in llm-bridge-claudecode.
func (s *Server) broadcastPermissionResolved(bridgeID, requestID string, d permissionDecision) {
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
		log.Printf("[prehook] broadcast completed for %s/%s: %v", bridgeID, requestID, err)
	}
}

// writeHookAllow / writeHookDeny / writeHookAsk shape the response body to
// match CC's PreToolUse hookSpecificOutput contract. CC reads
// permissionDecision and acts accordingly; permissionDecisionReason is
// surfaced to the model on deny and shown in CC's output on allow.
func writeHookAllow(w http.ResponseWriter, reason string) {
	writeHookDecision(w, "allow", reason, nil)
}

// writeHookAllowWithInput is the variant the parked-ask resolve path uses
// when the human selected updatedInput along with an allow verdict. CC's
// PreToolUse hookSpecificOutput contract accepts an `updatedInput` field
// alongside `permissionDecision: "allow"`; the tool's input is replaced
// with the merged value before its call() executes. AskUserQuestion uses
// this to receive prefilled answers without ever prompting the CLI user.
func writeHookAllowWithInput(w http.ResponseWriter, reason string, updatedInput json.RawMessage) {
	writeHookDecision(w, "allow", reason, updatedInput)
}

func writeHookDeny(w http.ResponseWriter, reason string) {
	writeHookDecision(w, "deny", reason, nil)
}

func writeHookAsk(w http.ResponseWriter, reason string) {
	writeHookDecision(w, "ask", reason, nil)
}

func writeHookDecision(w http.ResponseWriter, decision, reason string, updatedInput json.RawMessage) {
	hookOut := map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       decision,
		"permissionDecisionReason": reason,
	}
	if len(updatedInput) > 0 {
		hookOut["updatedInput"] = updatedInput
	}
	writeJSON(w, map[string]any{
		"hookSpecificOutput": hookOut,
	})
}
