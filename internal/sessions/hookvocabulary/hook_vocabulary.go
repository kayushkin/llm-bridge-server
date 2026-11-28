// Package hookvocabulary is the names each harness gives its hooks: the
// events it fires, the harness_config key its hook settings go in, and the
// permission gate route it calls.
package hookvocabulary

import "github.com/kayushkin/llm-bridge/msg"

// HarnessHookEvents lists the hook lifecycle events each harness can register
// handlers for via the bridge. Claude Code has the full lifecycle because its
// native hook engine runs in-process and supports deny/modify. Harnesses that
// only emit observation-style lifecycle notifications (e.g. Codex) or run
// agents remotely without any local hook point are absent here.
var HarnessHookEvents = map[msg.Harness][]string{
	// devin runs PreToolUse hooks from its config in every permission mode,
	// over ACP too (measured on devin 3000.12.43). Its other hook events are
	// not yet measured under ACP, so they are not offered.
	msg.HarnessDevin: {"PreToolUse"},
	msg.HarnessClaudeCode: {
		"PreToolUse",
		"PostToolUse",
		"UserPromptSubmit",
		"Notification",
		"Stop",
		"SubagentStop",
		"PreCompact",
		"SessionStart",
		"SessionEnd",
	},
}

// InjectHookSettings synthesizes a per-harness hook config blob and merges
// it into the session's HarnessConfig. The mutation is in-memory only
// (not persisted) so hook-store changes take effect on the next spawn.
//
// Per-harness storage key:
//
//   - claude_code → "settings" (consumed by --settings flag; the synthesized
//     JSON is the full CC settings.json shape with `hooks` block)
//   - codex       → "codex_hooks" (codex bridge translates the JSON tree
//     into `-c hooks.<EventName>=<inline-toml>` args at app-server spawn)
//
// If the caller has already set the harness-specific key, it wins.
//
// A nil hookStore is NOT a reason to skip this. The permission gate is
// synthesized from the session id alone and does not read the hook registry;
// only the user-hook section does, and both builders guard hookStore
// separately where they need it (BuildClaudeCodeSettings, BuildCodexHookConfig).
// Returning early here would turn a hook-registry outage into a silent
// permission-gate outage. main() no longer starts without the hook-store
// module, but a server built without one (as tests build it) must still gate.
// HookConfigKeyByHarness names the harnesses this server wires hooks into and
// the harness_config key each one's synthesized block is written under. It is
// what GET /hook-options serves, and TestEveryHarnessInTheHookOptionsIsWired
// holds it to the switch below: a harness listed here that the switch does not
// handle would be offered by the editor and never run.
var HookConfigKeyByHarness = map[msg.Harness]string{
	msg.HarnessClaudeCode: "settings",
	msg.HarnessCodex:      "codex_hooks",
	msg.HarnessDevin:      "devin_hooks",
}

// PermissionGateRouteByHarness names the /permission/<route>/<session> gate of
// each harness whose gate hook reads its address from
// PermissionGateURLEnvironmentVariable.
var PermissionGateRouteByHarness = map[msg.Harness]string{
	msg.HarnessCodex: "codex-prehook",
	msg.HarnessDevin: "devin-prehook",
}
