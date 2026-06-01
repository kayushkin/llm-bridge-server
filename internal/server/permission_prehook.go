package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/permclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// isUnattendedSession reports whether a session runs with no human watching to
// resolve permission prompts. Autonomous sessions (fire-and-forget agent runs
// like autoworker) qualify; parking a prehook ask for them would block until
// the idle reaper kills the process, so callers resolve asks deterministically
// instead. System and interactive sessions return false — system sessions are
// excluded deliberately so their asks aren't silently auto-allowed.
func isUnattendedSession(sess *store.Session) bool {
	return sess != nil && sess.Type == msg.SessionTypeAutonomous
}

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
// Two flavors of prehook ride this endpoint, distinguished by tool name:
//
//   - Permission gate (default): bypass short-circuit → permission-store
//     evaluate → park-for-human on ask. Source="permission_prompt".
//   - User-input solicitation (AskUserQuestion): always park for human.
//     Bypass and permission-store rules do not apply, since the "allow"
//     verdict's payload is the user's answer (delivered via updatedInput)
//     rather than a binary permission grant. Source="user_input".
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

	// Observability — log every prehook hit with the routing-critical fields
	// so we can tell at a glance whether codex/CC sessions are actually
	// reaching the gate. URL.Path distinguishes /permission/cc-prehook/
	// from /permission/codex-prehook/.
	log.Printf("[prehook] %s bridge=%s tool=%s tool_use_id=%s",
		r.URL.Path, bridgeID, payload.ToolName, payload.ToolUseID)

	// Fetch the session once up front: both the AskUserQuestion branch and
	// the permission-mode resolution below need it, and Type tells us whether
	// any human is attached to resolve a parked request.
	sess, _ := s.store.GetSession(bridgeID)
	unattended := isUnattendedSession(sess)

	// AskUserQuestion is a user-input solicitation, not a permission check
	// (the model wants the human's answer, not approval to run). No
	// permission mode applies: auto-allow would strip the answer payload,
	// auto-deny would deny the question. Park for the human — but an
	// unattended session has no human, so parking would hang the worker until
	// the idle reaper kills it. Deny with a clear message so it proceeds or
	// stops on its own instead of blocking.
	if payload.ToolName == "AskUserQuestion" {
		if unattended {
			writeHookDeny(w, "No human is attached to this autonomous session to answer AskUserQuestion; proceed without human input or stop.")
			return
		}
		s.parkPrehook(w, r, bridgeID, payload, msg.HookSourceUserInput)
		return
	}

	// Permission-mode short-circuit. Per-session override (set via PUT
	// /sessions/{id}/permission-mode) wins over the global. Both are read
	// live on every prehook request — no harness broadcast required since
	// bridge-server is the gating point.
	mode := s.permissionModeForSession(sess)

	switch mode {
	case msg.PermissionModeBlockAll:
		// Soft pause — agent sees the deny in its tool result and can
		// reason about it, ask the human, or stop. No rule consult.
		writeHookDeny(w, "Tool blocked by user (Block All mode). Ask the user before retrying.")
		return
	case msg.PermissionModePlan:
		if isPlanModeTool(payload.ToolName) {
			writeHookAllow(w, "permission-mode=plan:"+payload.ToolName)
			return
		}
		writeHookDeny(w, "Plan mode: only planning tools (Read/Glob/Grep/TodoWrite) are permitted.")
		return
	case msg.PermissionModeRead:
		if isReadOnlyTool(payload.ToolName) {
			writeHookAllow(w, "permission-mode=read:"+payload.ToolName)
			return
		}
		writeHookDeny(w, "Read-only mode: writes and shell execution are blocked.")
		return
	case msg.PermissionModeAskAll:
		// Skip permission-store entirely. Every tool call parks for the
		// human, regardless of any prior "always allow" rule.
		s.parkPrehook(w, r, bridgeID, payload, msg.HookSourcePermission)
		return
	case msg.PermissionModeBypass:
		writeHookAllow(w, "permission-mode=bypass")
		return
	case msg.PermissionModeAuto:
		if isAutoModeSafeTool(payload.ToolName) {
			writeHookAllow(w, "permission-mode=auto:"+payload.ToolName)
			return
		}
		// Fall through to the normal gating flow for non-safe tools.
	}
	// PermissionModeAsk + PermissionModeCustom fall through to
	// permission-store evaluation. Custom's raw approval/sandbox knobs
	// are harness-side concerns; rule evaluation is unchanged.

	if s.permClient == nil {
		writeHookAsk(w, "permission-store client not configured")
		return
	}

	res, err := s.permClient.Evaluate(r.Context(), permclient.Request{
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
		// An unattended session has no human to resolve a parked ask, so it
		// would hang until the idle reaper kills it — doing no work and
		// burning tokens. Per operator policy, unmatched tool calls in
		// autonomous sessions auto-allow. Deny rules already short-circuited
		// above (case "deny"), so guardrails like rm -rf / curl-external still
		// apply. Interactive sessions park for the human as before.
		if unattended {
			writeHookAllow(w, "auto-allow: unattended autonomous session, no permission rule matched")
			return
		}
		s.parkPrehook(w, r, bridgeID, payload, msg.HookSourcePermission)
	default:
		writeHookAsk(w, "permission-store returned unknown outcome "+res.Outcome)
	}
}

// autoModeSafeTools is the canonical bridge-defined set of tools the
// permission-mode=auto flow auto-allows without consulting permission-store.
// Scope: read-only inspection, file edits, and planning. Anything that
// touches a shell, network, or spawns subagents stays gated.
//
// This is the single source of truth for what "auto" means at the prehook
// level. Each harness binary chooses its own translation of the mode for
// its native gate; the prehook decision here is the universal floor.
var autoModeSafeTools = map[string]struct{}{
	// Read-only inspection
	"Read":         {},
	"Glob":         {},
	"Grep":         {},
	"LS":           {},
	"NotebookRead": {},
	// File edits
	"Edit":         {},
	"Write":        {},
	"MultiEdit":    {},
	"NotebookEdit": {},
	// Planning / harness-side state
	"TodoWrite":    {},
	"ExitPlanMode": {},
}

func isAutoModeSafeTool(name string) bool {
	_, ok := autoModeSafeTools[name]
	return ok
}

// planModeTools is the strict whitelist for PermissionModePlan: read-only
// inspection plus the planning state tools. No writes, no shell, no
// subagent spawns. Matches CC's native "plan" mode tool surface.
var planModeTools = map[string]struct{}{
	"Read":         {},
	"Glob":         {},
	"Grep":         {},
	"LS":           {},
	"NotebookRead": {},
	"TodoWrite":    {},
	"ExitPlanMode": {},
}

func isPlanModeTool(name string) bool {
	_, ok := planModeTools[name]
	return ok
}

// readOnlyTools is the whitelist for PermissionModeRead: pure inspection.
// Excludes TodoWrite/ExitPlanMode (planning state mutations) and all
// shell/edit tools. Safe-Bash heuristics could be layered on top of this
// later (e.g. allow `ls`/`cat`/`git log` while denying `rm`).
var readOnlyTools = map[string]struct{}{
	"Read":         {},
	"Glob":         {},
	"Grep":         {},
	"LS":           {},
	"NotebookRead": {},
}

func isReadOnlyTool(name string) bool {
	_, ok := readOnlyTools[name]
	return ok
}

// parkPrehook implements the shared "mint request id, emit
// awaiting_resolution, block on parkedAsks, emit completed, write response"
// flow used by both the permission-prompt and user-input branches of the
// prehook. Source picks which HookEvent.Source value identifies the parked
// request to bridge-ui (so the banner picks the right card flavor).
func (s *Server) parkPrehook(w http.ResponseWriter, r *http.Request, bridgeID string, payload ccPrehookPayload, source string) {
	resolveCtx := r.Context()
	requestID := ids.NewHookRequestID()
	ch := s.parkedAsks.park(bridgeID, requestID)

	// Emit awaiting_resolution so bridge-ui's banner picks it up via SSE.
	// BroadcastEvent records the pending hook for late-joining clients.
	if _, err := s.harness.BroadcastEvent(&msg.Event{
		Type:            msg.EventHook,
		Timestamp:       time.Now(),
		BridgeSessionID: bridgeID,
		Hook: &msg.HookEvent{
			Source:    source,
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
		s.broadcastPrehookResolved(bridgeID, requestID, source, decision)
		// CC has already disconnected; writing a body here is harmless
		// but won't be observed.
		writeHookDeny(w, decision.Message)
		return
	}

	s.broadcastPrehookResolved(bridgeID, requestID, source, decision)

	switch decision.Behavior {
	case "allow":
		writeHookAllowWithInput(w, decision.Message, decision.UpdatedInput)
	default:
		writeHookDeny(w, decision.Message)
	}
}

// broadcastPrehookResolved emits the phase=completed HookEvent that
// closes a previously-emitted awaiting_resolution event. Source must
// match the value used at park time so consumers can pair the events.
func (s *Server) broadcastPrehookResolved(bridgeID, requestID, source string, d permissionDecision) {
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
			Source:     source,
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
