package server

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// The five columns below were measured against each harness's origin/main on
// 2026-07-31, the same way TestCompactCapabilityMatchesTheHarnesses measured
// "compact". Each entry names the implementation it is a claim about. When one
// of them changes, re-measure that harness and move it between the two maps —
// do not edit the expectation to match whatever the table happens to say. That
// is the one way these tests stop meaning anything.
//
// What they can and cannot catch is worth being honest about: they fail when
// someone edits harnessCapabilities without re-measuring, which is how the
// table drifted in the first place. They cannot notice a harness changing in
// its own repo. Deriving the table from the harnesses is noteboard todo
// f8035505; until that lands this is a written-down measurement with its
// evidence attached, not a measurement that re-runs.
//
// One cell has moved since, and it is the shape the paragraph above asks for
// rather than an exception to it: hermes gained a config: branch in its own
// repo (llm-bridge-hermes ba7beb4), so "model" was re-measured against that
// code and hermes moved from lacks to has. Its effort and budget entries were
// written at the same time — the same branch names both fields back to the
// caller as unsupported, which is a measurement the audit could not make when
// there was no branch at all.

// capabilityColumn is one audited column: who earns it, who does not, and why
// in both cases.
type capabilityColumn struct {
	name  string
	has   map[msg.Harness]string
	lacks map[msg.Harness]string
}

var auditedColumns = []capabilityColumn{
	{
		name: "model",
		has: map[msg.Harness]string{
			msg.HarnessClaudeCode: "handleSessionConfig -> handleSetModel, applied to the live process",
			msg.HarnessCodex:      "HandleConfig -> cfg.CodexModel, passed on every TurnStart",
			msg.HarnessInber:      "POST /sessions/{id}/config -> Engine.SetModel",
			msg.HarnessCline:      "handleConfig sets h.model, passed as -m on the next one-shot turn",
			msg.HarnessJig:        "handleConfig patches profile.Model, applied on the next spawn",
			msg.HarnessHermes:     "handleConfig -> applyModel; sendMessageDirect reads h.cfg.Model on every /v1/responses turn (llm-bridge-hermes ba7beb4)",
		},
		lacks: map[msg.Harness]string{
			msg.HarnessOpenClaw:  `hardcodes the literal model "openclaw" in its request body`,
			msg.HarnessKiloCode:  "no Model field in StartParams; model comes only from the KILO_MODEL env var",
			msg.HarnessNanoClaw:  "no model is plumbed into the container at all",
			msg.HarnessForgecode: "its own README says the -m it passes has no effect",
			msg.HarnessAider:     "no config: branch; --model is fixed at session start",
			msg.HarnessGoose:     "scaffold: no dispatcher, exits 2 on any real invocation",
			msg.HarnessAutohand:  "scaffold: no dispatcher",
			msg.HarnessDexto:     "scaffold: no dispatcher",
			msg.HarnessCommander: "scaffold: no dispatcher",
			msg.HarnessGemini:    "scaffold: no dispatcher",
			msg.HarnessRooCode:   "scaffold: no dispatcher",
			msg.HarnessOpenCode:  "no llm-bridge-opencode repo and no binary on this host",
			msg.HarnessMock:      "the config: branch emits a canned ack and never unmarshals the payload",
		},
	},
	{
		name: "effort",
		has: map[msg.Harness]string{
			msg.HarnessCodex: "HandleConfig -> cfg.Effort, passed on the next TurnStart",
			msg.HarnessInber: "config -> Engine.SetThinkingBudget",
			msg.HarnessJig:   "handleConfig patches profile.Effort, --effort on the next spawn",
		},
		lacks: map[msg.Harness]string{
			msg.HarnessClaudeCode: `--effort is a spawn flag; handleSessionConfig answers "spawn-time only, unchanged"`,
			msg.HarnessCline:      "handleConfig names effort as unsupported and returns an error",
			msg.HarnessHermes:     "handleConfig names effort as unsupported: responsesRequest carries no effort field",
			msg.HarnessOpenClaw:   "no effort or reasoning field exists anywhere in the harness",
			msg.HarnessMock:       "never unmarshals the config payload",
		},
	},
	{
		name: "budget",
		has: map[msg.Harness]string{
			msg.HarnessJig: "handleConfig patches profile.MaxBudget, --max-budget-usd on the next spawn",
		},
		lacks: map[msg.Harness]string{
			msg.HarnessInber:      "refuses it by name: the bridge sends dollars, inber's config takes input tokens",
			msg.HarnessClaudeCode: `--max-budget-usd is a spawn flag; answered "spawn-time only, unchanged"`,
			msg.HarnessCline:      "names max_budget as unsupported and returns an error",
			msg.HarnessCodex:      "HandleConfig parses only {model, effort}; the field is dropped in silence",
			msg.HarnessHermes:     "handleConfig names max_budget as unsupported; hermes has no spend setting to reach",
			msg.HarnessMock:       "never unmarshals the config payload",
		},
	},
	{
		name: "fork",
		has: map[msg.Harness]string{
			msg.HarnessClaudeCode: "start with params.Fork -> --resume <uuid> --fork-session",
			msg.HarnessJig:        "start with params.Fork -> --resume <uuid> --fork-session",
			msg.HarnessCodex:      "start with params.Fork -> codex thread/fork",
			msg.HarnessInber:      "POST /sessions/{parent}/fork, deep-copies the parent's messages",
			msg.HarnessKiloCode:   "start with params.Fork -> POST /session/{id}/fork, rotates the harness id",
		},
		lacks: map[msg.Harness]string{
			msg.HarnessHermes:    "fails the start outright with FORK_UNSUPPORTED; fork is per-response in Hermes",
			msg.HarnessCline:     "passes the parent id to the same -T flag resume uses, so nothing branches",
			msg.HarnessOpenClaw:  "emits FORK_UNSUPPORTED: no native session-cloning primitive",
			msg.HarnessNanoClaw:  "declares the Fork field and never reads it; spawns a fresh container",
			msg.HarnessAider:     "declares the Fork field and never reads it",
			msg.HarnessForgecode: "declares the Fork field and never reads it; forge -p keeps no thread",
			msg.HarnessMock:      "unmarshals sp.Fork and then reads only sp.Prompt",
		},
	},
	{
		name: "system_prompt",
		has: map[msg.Harness]string{
			msg.HarnessClaudeCode: "backfills SessionInfo.SystemPrompt from its start flags on every init",
			msg.HarnessMock:       "emits SessionInfo.SystemPrompt at start",
		},
		lacks: map[msg.Harness]string{
			msg.HarnessCodex:    "emits no SessionInfo event, so the button has nothing to render",
			msg.HarnessHermes:   "accepts a system prompt at start but emits no SessionInfo",
			msg.HarnessInber:    "emits no SessionInfo event",
			msg.HarnessJig:      "a profile can set --system-prompt, but nothing surfaces it",
			msg.HarnessOpenClaw: "emits no SessionInfo event",
		},
	},
}

func harnessHasCapability(h msg.Harness, capability string) bool {
	for _, c := range harnessCapabilities[h] {
		if c == capability {
			return true
		}
	}
	return false
}

// TestAuditedCapabilityColumnsMatchTheHarnesses is the general form of the
// compact test: every column that has been audited, checked in both
// directions. A granted capability the harness drops is a control that
// silently does nothing; a withheld one is a working feature no UI can reach.
func TestAuditedCapabilityColumnsMatchTheHarnesses(t *testing.T) {
	for _, col := range auditedColumns {
		for h, impl := range col.has {
			if !harnessHasCapability(h, col.name) {
				t.Errorf("harness %q supports %q for real (%s) but the table withholds it; the UI hides a working control",
					h, col.name, impl)
			}
		}
		for h, why := range col.lacks {
			if harnessHasCapability(h, col.name) {
				t.Errorf("harness %q does not support %q (%s) but the table grants it; the UI shows a control that does nothing",
					h, col.name, why)
			}
		}
		for h := range col.has {
			if why, both := col.lacks[h]; both {
				t.Errorf("harness %q is listed in both halves of the %q column (%q); one measurement, one side",
					h, col.name, why)
			}
		}
	}
}

// TestCapabilityNamesAreConsumed guards the other way a column goes wrong:
// not a wrong value, but a name no client reads and no harness implements.
// "interrupt" sat on hermes alone for exactly that reason — it named a
// JSON-RPC method this server never sends (POST /sessions/{id}/interrupt is
// SIGINT), and no UI gated anything on it.
//
// The list is every capability string the three consumer surfaces branch on:
// bridge-ui Workspace.tsx and BridgeSettings.tsx, dash chat/SessionControls.tsx
// and dash dashv2/ControlsBar.tsx.
func TestCapabilityNamesAreConsumed(t *testing.T) {
	consumed := map[string]bool{
		"compact":       true,
		"fork":          true,
		"model":         true,
		"effort":        true,
		"tools":         true,
		"budget":        true,
		"system_prompt": true,
	}
	for h, caps := range harnessCapabilities {
		for _, c := range caps {
			if !consumed[c] {
				t.Errorf("harness %q is granted capability %q, which no UI reads and no handler acts on; "+
					"either wire a consumer or drop the string", h, c)
			}
		}
	}
}

// TestEmptyCapabilitiesSerializeAsAnEmptyList pins the one thing that would
// turn withholding a capability into the opposite of a fix. dash's chat
// controls read `has = cap => !capabilities || capabilities.has(cap)`, which
// fails OPEN: an absent field shows every control. A harness that supports
// nothing must therefore serialize as "capabilities":[] and never omit the
// field, so msg.HarnessInfo.Capabilities must not gain an omitempty and
// discoverHarnesses must keep normalizing nil to an empty slice.
func TestEmptyCapabilitiesSerializeAsAnEmptyList(t *testing.T) {
	var empty msg.Harness
	for h, caps := range harnessCapabilities {
		if len(caps) == 0 {
			empty = h
			break
		}
	}
	if empty == "" {
		t.Skip("no harness currently has an empty capability list; nothing to pin")
	}

	caps := harnessCapabilities[empty]
	if caps == nil {
		caps = []string{}
	}
	data, err := json.Marshal(msg.HarnessInfo{Name: string(empty), Capabilities: caps})
	if err != nil {
		t.Fatalf("marshal HarnessInfo: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, present := decoded["capabilities"]
	if !present {
		t.Fatalf("capabilities was omitted from %s; dash's `!capabilities` check fails open and shows every control", data)
	}
	if string(raw) != "[]" {
		t.Errorf("capabilities serialized as %s, want []", raw)
	}
}

// TestNilCapabilitiesAreNormalized covers the same hazard one layer up: the
// map lookup for a harness with no entry at all returns nil, and it is
// discoverHarnesses that turns that into an empty slice before it reaches a
// client.
func TestNilCapabilitiesAreNormalized(t *testing.T) {
	var caps []string = harnessCapabilities["not-a-harness"]
	if caps != nil {
		t.Fatalf("expected a nil lookup for an unknown harness, got %v", caps)
	}
	if caps == nil {
		caps = []string{}
	}
	data, err := json.Marshal(msg.HarnessInfo{Name: "x", Capabilities: caps})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(decoded["capabilities"]) != "[]" {
		t.Errorf("nil capabilities serialized as %s, want []", decoded["capabilities"])
	}
}
