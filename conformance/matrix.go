// Package conformance provides a harness conformance test framework.
//
// It tests harness binaries against the llm-bridge subprocess protocol,
// recording which features each harness supports in a structured matrix.
package conformance

import (
	"encoding/json"
	"os"
	"time"
)

// Feature is a capability that a harness may or may not support.
type Feature string

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

// TestResult records the outcome of a single feature test.
type TestResult struct {
	Feature Feature `json:"feature"`
	Passed  bool    `json:"passed"`

	// Skipped means the test could not reach a verdict: a precondition failed,
	// or the feature cannot be exercised by this runner at all. It says nothing
	// about the harness.
	Skipped bool `json:"skipped,omitempty"`

	// Unsupported means the test ran, the harness answered, and it does not do
	// this. That is a verdict about the harness, and it is the one the old
	// grading could not express — every absence was filed as Skipped, so a
	// harness missing a feature looked identical to a feature nobody could
	// test. Sixty per cent of the matrix was Skip, and reading it as coverage
	// was reading mostly nothing.
	Unsupported bool `json:"unsupported,omitempty"`

	Error    string `json:"error,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// Verdict names the outcome in one word, so callers do not each re-derive it
// from three booleans and disagree about the precedence.
func (r TestResult) Verdict() string {
	switch {
	case r.Skipped:
		return "skipped"
	case r.Unsupported:
		return "unsupported"
	case r.Passed:
		return "passed"
	default:
		return "failed"
	}
}

// HarnessResult records all test results for a single harness.
type HarnessResult struct {
	Harness  string       `json:"harness"`
	Binary   string       `json:"binary"`
	TestedAt time.Time    `json:"tested_at"`
	Results  []TestResult `json:"results"`
	Summary  Summary      `json:"summary"`
}

// Summary counts test outcomes.
type Summary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Unsupported counts features the harness answered for and does not
	// implement. Kept apart from Skipped so a reader can tell "this harness
	// cannot do it" from "we never found out".
	Unsupported int `json:"unsupported"`
}

// Matrix holds conformance results for all tested harnesses.
type Matrix struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Harnesses   []HarnessResult `json:"harnesses"`
}

// AddResult records a feature test result for a harness.
func (hr *HarnessResult) AddResult(r TestResult) {
	hr.Results = append(hr.Results, r)
	hr.Summary.Total++
	// Skipped stays first: a test that never reached a verdict cannot also
	// report one. Unsupported comes before Passed so a result carrying both
	// counts as the narrower claim.
	switch {
	case r.Skipped:
		hr.Summary.Skipped++
	case r.Unsupported:
		hr.Summary.Unsupported++
	case r.Passed:
		hr.Summary.Passed++
	default:
		hr.Summary.Failed++
	}
}

// Supports returns true if the harness passed the given feature test.
func (hr *HarnessResult) Supports(f Feature) bool {
	for _, r := range hr.Results {
		if r.Feature == f {
			return r.Passed
		}
	}
	return false
}

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
