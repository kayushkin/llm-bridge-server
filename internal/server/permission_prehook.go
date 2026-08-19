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
// instead. Herald sessions (agent-initiated question/alert relays — see the
// `ask` CLI) also qualify: no human is attached during the relay turn, so a
// parked tool ask would wedge it before it ever reaches the user. System and
// interactive sessions return false — system sessions are excluded deliberately
// so their asks aren't silently auto-allowed.
//
// External sessions also return false, and that is a decision rather than a
// fallthrough. An external session ran outside the bridge, so ordinarily no
// prehook fires for it at all; one can only fire if somebody resumed the
// session through the bridge, and the overwhelmingly likely somebody is a
// person who clicked resume. Between the two ways to be wrong here — parking
// an ask nobody answers until the reaper collects it, or auto-allowing a tool
// call on a session a human is driving — the first is loud and recoverable and
// the second is silent. Prefer the loud one.
func isUnattendedSession(sess *store.Session) bool {
	return sess != nil && (sess.Type == msg.SessionTypeAutonomous || sess.Type == msg.SessionTypeHerald)
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

	// Fetch the session before anything that can fail: the AskUserQuestion
	// branch and the permission-mode resolution below both need it, Type tells
	// us whether any human is attached to resolve a parked request, and the
	// two decode failures below need the same answer to pick their verdict.
	sess, _ := s.store.GetSession(bridgeID)
	unattended := isUnattendedSession(sess)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeHookNoVerdict(w, unattended, "read prehook body: "+err.Error())
		return
	}

	var payload ccPrehookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.writeHookNoVerdict(w, unattended, "decode prehook payload: "+err.Error())
		return
	}

	// Observability — log every prehook hit with the routing-critical fields
	// so we can tell at a glance whether codex/CC sessions are actually
	// reaching the gate. URL.Path distinguishes /permission/cc-prehook/
	// from /permission/codex-prehook/.
	log.Printf("[prehook] %s bridge=%s tool=%s tool_use_id=%s",
		r.URL.Path, bridgeID, payload.ToolName, payload.ToolUseID)

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
		s.parkPrehook(w, r, bridgeID, sess, payload, msg.HookSourceUserInput, unattended)
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
		s.parkPrehook(w, r, bridgeID, sess, payload, msg.HookSourcePermission, unattended)
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
		s.writeHookNoVerdict(w, unattended, "permission-store client not configured")
		return
	}

	res, err := s.permClient.Evaluate(r.Context(), permclient.Request{
		SessionID: bridgeID,
		Tool:      payload.ToolName,
		Input:     payload.ToolInput,
	})
	if err != nil {
		// Pass the client's error through rather than labelling it. Evaluate
		// distinguishes five failures — unreachable, non-2xx, unreadable body,
		// undecodable body, outcome outside the allowed set — and the fixed
		// "unreachable" prefix this used to carry reported four of them as the
		// one they were not.
		s.writeHookNoVerdict(w, unattended, "permission-store evaluate failed: "+err.Error())
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
		s.parkPrehook(w, r, bridgeID, sess, payload, msg.HookSourcePermission, unattended)
	default:
		s.writeHookNoVerdict(w, unattended, "permission-store returned unknown outcome "+res.Outcome)
	}
}

// writeHookNoVerdict answers a prehook that could not reach a verdict at all:
// the payload would not decode, the permission-store client is missing, the
// store did not answer, or it answered with an outcome this build does not
// know. None of those is a permission decision, so the interactive answer is
// "ask" — hand it to the human at the keyboard, reason attached.
//
// An unattended session has no such human. This file already establishes that
// twice, and handles it in both branches that reach a verdict: AskUserQuestion
// denies with an explanation rather than parking, and an "ask" outcome from
// the store auto-allows. The error paths were left on the interactive default,
// so an autonomous session met an "ask" nobody could answer and the tool call
// died with a raw transport error instead of a decision it could act on
// (measured: bridge session br_1785306448183126697, a Task call that reported
// `permission-store unreachable: dial tcp 127.0.0.1:1` and gave up — the
// prehook was pointed at a deliberately dead address by scripts/e2e-subagent.sh
// at the time, which is exactly the outage this path has to answer for).
//
// Deny, not allow, and the asymmetry with the "ask" outcome above is the
// point. That auto-allow is justified by a specific fact — deny rules already
// ran and short-circuited, so the guardrails still applied. When the store
// never answered, no rule ran at all, and there is nothing left holding the
// floor. So the gate fails closed, and says why, so the agent can route around
// the tool or stop rather than hang.
func (s *Server) writeHookNoVerdict(w http.ResponseWriter, unattended bool, reason string) {
	if unattended {
		writeHookDeny(w, reason+" — no human is attached to this autonomous session to resolve it, and no permission rule was evaluated, so the call is denied. Proceed without this tool or stop.")
		return
	}
	writeHookAsk(w, reason)
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
//
// A user-input park also writes the canonical signal rows for the questions
// it carries (SESSION-SIGNALS.md P1). sess supplies the session type the
// signal's surface is derived from; it may be nil for a session the store
// has no row for, in which case the signal surfaces to chat.
//
// unattended is the caller's answer to isUnattendedSession, threaded through
// so the no-verdict path below answers a watcherless session the same way the
// rest of the prehook does rather than parking it a second time.
func (s *Server) parkPrehook(w http.ResponseWriter, r *http.Request, bridgeID string, sess *store.Session, payload ccPrehookPayload, source string, unattended bool) {
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
		s.writeHookNoVerdict(w, unattended, "broadcast awaiting_resolution: "+err.Error())
		return
	}

	// Record the signal only once the request is genuinely parked. Doing it
	// before the broadcast would leave an open signal row behind on the
	// failure path above, where the park is cancelled and no human ever sees
	// the question.
	if source == msg.HookSourceUserInput {
		s.recordAskUserQuestionSignals(bridgeID, sess, requestID, payload.ToolInput)
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
			Behavior:        "deny",
			Message:         "request canceled before resolution: " + resolveCtx.Err().Error(),
			ResolvedBy:      "auto:context-canceled",
			SessionWentAway: true,
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
// closes a previously-emitted awaiting_resolution event, and closes out any
// signal rows the request minted. Source must match the value used at park
// time so consumers can pair the events.
func (s *Server) broadcastPrehookResolved(bridgeID, requestID, source string, d permissionDecision) {
	s.resolveSignalsForRequest(bridgeID, requestID, d)

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
