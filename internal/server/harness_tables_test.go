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
