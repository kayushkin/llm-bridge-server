package server

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// claudeCodeShapedHarnesses names harnesses whose bridge spawns the Claude Code
// binary and therefore expects the claude-code-shaped --settings blob that
// injectClaudeCodeHookSettings synthesizes — the blob carrying the PreToolUse
// permission hook.
//
// copilot_cli is on this list because llm-bridge-copilotcli is an
// un-retargeted clone of llm-bridge-claudecode: its config.go resolves
// CLAUDE_PATH and spawns `claude`, and its handler appends --settings from the
// start params. It is currently held off by disabledHarnesses, so nothing
// reaches injectHookSettings for it today.
//
// ⚠️ This list is deliberately NOT "every harness that ought to be gated".
// llm-bridge-jig also spawns `claude`, but it accepts no settings param at all,
// so wiring a case here would inject a blob it cannot carry — a gap that lives
// in the harness, not in this switch. That is noteboard todo 49a697df's scope
// (the UI is told every harness is gated by the universal prehook when only
// claude_code is). Adding jig here would claim this file can fix it.
var claudeCodeShapedHarnesses = []msg.Harness{
	msg.HarnessCopilotCLI,
}

// TestEnablingAHarnessKeepsItsPermissionGate pins a precondition that was
// previously carried only in prose, and only in a noteboard card.
//
// injectHookSettings switches on the harness and has cases for claude_code and
// codex alone. copilot_cli is kept unreachable somewhere else entirely — the
// disabledHarnesses entry in health.go, whose stated reason is a stale binary
// on PATH and says nothing about permissions. So the two facts that jointly
// keep the gate honest sat in different files with no link between them:
// whoever deletes the disabledHarnesses entry to enable the harness would ship
// a harness with no permission-gate wiring, in the same commit, and every test
// here would stay green.
//
// This test is that link. While the harness is disabled it asserts the
// situation is exactly as described. The moment it is enabled, it demands the
// case in injectHookSettings — so enabling without wiring breaks the build
// rather than quietly opening the gate.
func TestEnablingAHarnessKeepsItsPermissionGate(t *testing.T) {
	for _, h := range claudeCodeShapedHarnesses {
		_, disabled := disabledHarnesses[h]
		wired := injectsClaudeCodeSettings(t, h)

		switch {
		case disabled && wired:
			// Not a failure, but the comment above is now wrong: the
			// harness gained a case while still held off.
			t.Logf("harness %q is disabled but injectHookSettings now wires it; enabling it is no longer gate-losing", h)
		case !disabled && !wired:
			t.Errorf("harness %q is enabled but injectHookSettings has no case for it, so its sessions spawn with no PreToolUse permission hook; "+
				"add a case to injectHookSettings (it spawns `claude` and takes --settings, so injectClaudeCodeHookSettings fits) or put it back in disabledHarnesses", h)
		case disabled && !wired:
			// The state this test was written in.
		}
	}
}

// TestClaudeCodeShapedHarnessesAreCanonical stops this file's list outliving
// the harnesses it names — the same failure disabledHarnesses has its own
// canonicality test for.
func TestClaudeCodeShapedHarnessesAreCanonical(t *testing.T) {
	canonical := make(map[msg.Harness]bool, len(msg.AllHarnesses))
	for _, h := range msg.AllHarnesses {
		canonical[h] = true
	}
	for _, h := range claudeCodeShapedHarnesses {
		if !canonical[h] {
			t.Errorf("claudeCodeShapedHarnesses names %q, which is not in msg.AllHarnesses; drop the stale entry", h)
		}
	}
}

// injectsClaudeCodeSettings reports whether injectHookSettings writes a
// claude-code "settings" blob for this harness. It asks by calling the real
// function rather than reading the switch, so a case that exists but does
// nothing counts as absent — which is what matters to the caller.
func injectsClaudeCodeSettings(t *testing.T, h msg.Harness) bool {
	t.Helper()
	srv, _ := testServerWithHookStore(t)

	sess := &store.Session{SessionID: "gate-precondition", Harness: h}
	srv.injectHookSettings(sess)

	if len(sess.HarnessConfig) == 0 {
		return false
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("HarnessConfig for %q is unparseable after injection: %v", h, err)
	}
	_, ok := cfg["settings"]
	return ok
}

// TestInjectsClaudeCodeSettingsDetectsTheWiredCase is the control for the
// helper above. Every assertion in this file rests on it, and a helper that
// always answered "not wired" would make the whole file vacuous while looking
// like it passed. claude_code is the case known to exist.
func TestInjectsClaudeCodeSettingsDetectsTheWiredCase(t *testing.T) {
	if !injectsClaudeCodeSettings(t, msg.HarnessClaudeCode) {
		t.Error("the helper reports claude_code as unwired; it has a case in injectHookSettings, so the detection is broken and every other assertion in this file is vacuous")
	}
}
