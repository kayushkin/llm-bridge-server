package server

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestHarnessTablesCoverCanonicalSet pins the invariant that this gateway's
// per-harness tables stay in step with the canonical harness list in llm-bridge.
//
// These tables used to be hand-copied duplicates of msg.AllHarnesses. When
// HarnessCopilotCLI was added upstream, nobody updated them, so the harness
// silently disappeared from /harnesses, from conformance runs, and from
// session-create validation — a missing entry read exactly like a deliberate
// omission. This test removes that failure mode: a harness added upstream now
// breaks the build here until it is either described or explicitly disabled.
func TestHarnessTablesCoverCanonicalSet(t *testing.T) {
	for _, h := range msg.AllHarnesses {
		if reason, disabled := disabledHarnesses[h]; disabled {
			if reason == "" {
				t.Errorf("harness %q is disabled with an empty reason; state why so the omission stays a decision", h)
			}
			continue
		}
		if _, ok := harnessMetadata[h]; !ok {
			t.Errorf("harness %q is enabled but missing from harnessMetadata; add an entry or list it in disabledHarnesses", h)
		}
		if _, ok := harnessCapabilities[h]; !ok {
			t.Errorf("harness %q is enabled but missing from harnessCapabilities; add an entry or list it in disabledHarnesses", h)
		}
	}
}

// TestCompactCapabilityMatchesTheHarnesses pins the "compact" column to what
// each harness's dispatcher was measured to do on 2026-07-31. The chat UI
// gates its Compact button on this column, so an entry that drifts from the
// harness is a button that lies in one direction or the other — and nothing
// else in this repo can notice, because the harnesses live in other repos.
//
// Each name below is a claim about a specific implementation. If one of them
// changes, re-measure that harness and move it, rather than adjusting the
// expectation to match whatever the table happens to say.
func TestCompactCapabilityMatchesTheHarnesses(t *testing.T) {
	// Harnesses that send a real compaction request to their backend.
	compacts := map[msg.Harness]string{
		msg.HarnessClaudeCode: "writes /compact to the CC process",
		msg.HarnessCodex:      "HandleCompact on the codex app-server",
		msg.HarnessInber:      "POST /sessions/{id}/compact",
		msg.HarnessKiloCode:   "server.Summarize",
		msg.HarnessJig:        "writes /compact to the CC process (llm-bridge-jig 9ac0b0e)",
		msg.HarnessMock:       "reference harness, acknowledges with the summary",
	}
	// Harnesses that do not, whether they refuse honestly (hermes, cline,
	// aider, forgecode) or claim a delegation that never happens (openclaw,
	// nanoclaw). Either way the user must not be shown a Compact button.
	doesNotCompact := map[msg.Harness]string{
		msg.HarnessHermes:    "emits an UNSUPPORTED error event",
		msg.HarnessCline:     "compact_unsupported ack; cline manages context internally",
		msg.HarnessAider:     "compact_ack no-op",
		msg.HarnessForgecode: "compact_ack no-op",
		msg.HarnessOpenClaw:  `says "compaction delegated to OpenClaw" and writes nothing`,
		msg.HarnessNanoClaw:  `says "compaction delegated to NanoClaw" and writes nothing`,
	}

	has := func(h msg.Harness) bool {
		for _, c := range harnessCapabilities[h] {
			if c == "compact" {
				return true
			}
		}
		return false
	}

	for h, impl := range compacts {
		if !has(h) {
			t.Errorf("harness %q compacts for real (%s) but the table withholds \"compact\"; the UI hides a working button", h, impl)
		}
	}
	for h, why := range doesNotCompact {
		if has(h) {
			t.Errorf("harness %q does not compact (%s) but the table grants \"compact\"; the UI shows a button that does nothing", h, why)
		}
	}
}

// TestDisabledHarnessesAreCanonical catches a disable entry that outlives the
// harness it names — e.g. one left behind after a harness is renamed or removed
// upstream, which would otherwise sit here forever suppressing nothing.
func TestDisabledHarnessesAreCanonical(t *testing.T) {
	canonical := make(map[msg.Harness]bool, len(msg.AllHarnesses))
	for _, h := range msg.AllHarnesses {
		canonical[h] = true
	}
	for h := range disabledHarnesses {
		if !canonical[h] {
			t.Errorf("disabledHarnesses names %q, which is not in msg.AllHarnesses; drop the stale entry", h)
		}
	}
}

// TestAllHarnessesExcludesDisabled checks the derivation itself: everything the
// gateway will surface, validate and spawn is canonical and enabled. isValidHarness
// gates session create, so a disabled harness leaking in here is what would let
// POST /sessions spawn its binary.
func TestAllHarnessesExcludesDisabled(t *testing.T) {
	seen := make(map[msg.Harness]bool, len(allHarnesses))
	for _, h := range allHarnesses {
		if _, off := disabledHarnesses[h]; off {
			t.Errorf("harness %q is in disabledHarnesses but still present in allHarnesses; isValidHarness would accept it and spawn its binary", h)
		}
		if seen[h] {
			t.Errorf("harness %q appears twice in allHarnesses", h)
		}
		seen[h] = true
	}
	if want := len(msg.AllHarnesses) - len(disabledHarnesses); len(allHarnesses) != want {
		t.Errorf("allHarnesses has %d entries, want %d (canonical %d minus %d disabled)",
			len(allHarnesses), want, len(msg.AllHarnesses), len(disabledHarnesses))
	}
	if isValidHarness(msg.HarnessCopilotCLI) {
		t.Error("isValidHarness accepts copilot_cli; session create would spawn the stale llm-bridge-copilotcli binary on PATH")
	}
	if !isValidHarness(msg.HarnessClaudeCode) {
		t.Error("isValidHarness rejects claude_code; the derivation dropped an enabled harness")
	}
}
