// Package prehookanswers is the PreToolUse hook's wire format: what Claude
// Code (and codex) send a permission prehook, and the answers it reads back.
package prehookanswers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
)

// Payload is the stdin JSON Claude Code sends to PreToolUse hook
// commands. We unmarshal only what we need to evaluate; ToolUseID is kept
// for the audit log but not used for correlation (we mint our own hook
// request id so resolve flow stays uniform with the legacy MCP path).
type Payload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
}

// WriteAllow / WriteHookDeny / WriteHookAsk shape the response body to
// match CC's PreToolUse hookSpecificOutput contract. CC reads
// permissionDecision and acts accordingly; permissionDecisionReason is
// surfaced to the model on deny and shown in CC's output on allow.
func WriteAllow(w http.ResponseWriter, reason string) {
	WriteHookDecision(w, "allow", reason, nil)
}

// WriteAllowWithInput is the variant the parked-ask resolve path uses
// when the human selected updatedInput along with an allow verdict. CC's
// PreToolUse hookSpecificOutput contract accepts an `updatedInput` field
// alongside `permissionDecision: "allow"`; the tool's input is replaced
// with the merged value before its call() executes. AskUserQuestion uses
// this to receive prefilled answers without ever prompting the CLI user.
func WriteAllowWithInput(w http.ResponseWriter, reason string, updatedInput json.RawMessage) {
	WriteHookDecision(w, "allow", reason, updatedInput)
}

// WriteDeny always gives a reason. Codex treats a deny with an empty
// permissionDecisionReason as a failed hook and runs the tool: measured on
// codex 0.172 on 2026-11-25, and on the PC test instance a person's decline,
// resolved with no message, let the command run (br_1795640613496687676).
func WriteDeny(w http.ResponseWriter, reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = DeniedWithoutAReasonMessage
	}
	WriteHookDecision(w, "deny", reason, nil)
}

// DeniedWithoutAReasonMessage is the reason a deny carries when
// whoever denied it gave none.
const DeniedWithoutAReasonMessage = "The bridge permission check denied this tool call."

func WriteAsk(w http.ResponseWriter, reason string) {
	WriteHookDecision(w, "ask", reason, nil)
}

func WriteHookDecision(w http.ResponseWriter, decision, reason string, updatedInput json.RawMessage) {
	hookOut := map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       decision,
		"permissionDecisionReason": reason,
	}
	if len(updatedInput) > 0 {
		hookOut["updatedInput"] = updatedInput
	}
	httpjson.WriteJSON(w, map[string]any{
		"hookSpecificOutput": hookOut,
	})
}
