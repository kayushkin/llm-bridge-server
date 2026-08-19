// Package conformance provides a harness conformance test framework.
//
// It tests harness binaries against the llm-bridge subprocess protocol,
// recording which features each harness supports in a structured matrix.
package conformance

import (
	"encoding/json"
	"os"

	"github.com/kayushkin/llm-bridge/msg"
)

const (
	// ── Lifecycle (control plane) ────────────────────────────────────────
	FeatureStart    Feature = "start"    // Can start a new session
	FeatureResume   Feature = "resume"   // Can resume an existing session
	FeatureFork     Feature = "fork"     // Can fork from a parent session
	FeatureCompact  Feature = "compact"  // Can compact context
	FeatureConfig   Feature = "config"   // Can update runtime config (model, effort, etc.)
	FeatureDiscover Feature = "discover" // Can discover on-disk sessions via -discover
	FeatureImport   Feature = "import"   // Can import history via -import-history

	// ── Message round-trip (the EventResult and EventStream pair) ────────
	FeatureMessage   Feature = "message"   // Can receive a message and emit an EventResult
	FeatureStreaming Feature = "streaming" // Emits EventStream deltas (not just result)

	// ── Content blocks emitted alongside the message round-trip ──────────
	FeatureBlock     Feature = "block"      // Emits EventBlock (whole finished content blocks)
	FeatureToolCalls Feature = "tool_calls" // Emits EventToolCall / EventToolResult
	FeatureThinking  Feature = "thinking"   // Emits EventThinking
	FeaturePlan      Feature = "plan"       // Emits EventPlan (structured task-planning)

	// ── Session metadata / observation ───────────────────────────────────
	FeatureSessionInfo  Feature = "session_info"  // Emits EventSessionInfo at start (system prompt, tools, MCP, …)
	FeatureUserMessage  Feature = "user_message"  // Emits EventUserMessage echo of the caller's input
	FeatureContextUsed  Feature = "context_used"  // Reports token usage in result events
	FeatureSystemPrompt Feature = "system_prompt" // Accepts a custom system_prompt at start
	FeatureReasoning    Feature = "reasoning"     // Accepts reasoning effort config

	// ── Hook / approval signalling ───────────────────────────────────────
	FeatureHook   Feature = "hook"   // Emits EventHook lifecycle events (PreToolUse, PostToolUse, …)
	FeatureErrors Feature = "errors" // Properly emits EventError on failure

	// ── Convenience events (derived centrally by llm-bridge-server) ──────
	// These are not emitted by harnesses directly — the conformance runner
	// spawns harnesses as direct subprocesses, so these always Skip with a
	// "server-derived" reason. They appear in the matrix purely so the UI
	// can document every event type in the protocol.
	FeatureUsageTotal   Feature = "usage_total"   // EventUsageTotal — cumulative session usage
	FeatureTurnComplete Feature = "turn_complete" // EventTurnComplete — coalesced turn summary
)

// AllFeatures lists every testable feature, ordered by category for stable UI
// presentation (see the FEATURE_GROUPS map in BridgeConformance.tsx).
var AllFeatures = []Feature{
	// Lifecycle
	FeatureStart,
	FeatureResume,
	FeatureFork,
	FeatureCompact,
	FeatureConfig,
	FeatureDiscover,
	FeatureImport,
	// Message round-trip
	FeatureMessage,
	FeatureStreaming,
	// Content blocks
	FeatureBlock,
	FeatureToolCalls,
	FeatureThinking,
	FeaturePlan,
	// Session metadata
	FeatureSessionInfo,
	FeatureUserMessage,
	FeatureContextUsed,
	FeatureSystemPrompt,
	FeatureReasoning,
	// Hooks / errors
	FeatureHook,
	FeatureErrors,
	// Server-derived convenience events
	FeatureUsageTotal,
	FeatureTurnComplete,
}

// The conformance record types are the canonical ones from llm-bridge's msg
// package, aliased rather than redefined.
//
// They used to be a second, parallel set of structs with the same JSON tags,
// bridged by a hand-written toMsgResult in internal/server. That copy agreed
// with the canonical shape only by luck, and stopped: when Unsupported was
// added here, the converter kept copying five fields out of six, so the API
// served an unsupported verdict as neither passed nor skipped — which every
// reader renders as FAILED. The duplication silently defeated the very
// distinction it was carrying.
//
// These are ALIASES (=), not new named types, so a value produced here IS a
// msg value: no conversion exists to fall behind, and adding a field to the
// canonical struct cannot leave this package on a stale copy. Verdict,
// AddResult and Supports live on the canonical types for the same reason —
// the summary invariant belongs to the shape, not to whoever fills it in.
type (
	Feature       = msg.ConformanceFeature
	TestResult    = msg.ConformanceTestResult
	HarnessResult = msg.ConformanceHarnessResult
	Summary       = msg.ConformanceSummary
	Matrix        = msg.ConformanceMatrix
)

// SaveMatrix writes the conformance matrix to a JSON file.
func SaveMatrix(path string, m *Matrix) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// LoadMatrix reads a conformance matrix from a JSON file.
func LoadMatrix(path string) (*Matrix, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Matrix
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
