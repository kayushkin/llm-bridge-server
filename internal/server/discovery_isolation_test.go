package server

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateHarnessSessionDiscovery points every harness binary's session
// discovery at a throwaway HOME holding exactly one planted Claude Code
// rollout, and returns that rollout's session id.
//
// Why this exists, measured rather than assumed. `GET /sessions/discover`
// shells out to `llm-bridge-claudecode -discover`, which walks
// `$HOME/.claude/projects/**` and cold-imports every `<uuid>.jsonl` it has
// not seen. The handler then starts ONE `-import-history` subprocess per
// row the test's fresh database calls new — and a fresh database calls all
// of them new. On the development box that is 4,440 rollout files, so the
// two tests that call the route were spawning roughly 4,400 subprocesses
// each and taking 141 seconds apiece. That was 282 of the package's 478
// seconds, against Go's 600-second default package timeout: a margin that
// shrank every time the box started another session, and whose overrun
// arrives as a timeout panic naming whichever innocent test happened to be
// running.
//
// The isolation is HOME rather than CLAUDE_CONFIG_DIR on purpose.
// llm-bridge-claudecode documents CLAUDE_CONFIG_DIR as the override, but
// only its transcript path honours it; discover.go re-derives
// `~/.claude/projects` from os.UserHomeDir() and ignores the variable, and
// the state.db that records which rollouts are already known hangs off the
// home directory too. HOME moves all three at once. Measured against the
// real binary: 4,440 sessions in 141s before, 1 session in 0.035s after.
//
// The planted rollout is also what makes the discovery tests assert
// anything. Both of them skip when discovery comes back empty, so on a host
// with no Claude Code history they were passing by doing nothing.
func isolateHarnessSessionDiscovery(t *testing.T) string {
	t.Helper()

	const sessionID = "11111111-2222-3333-4444-555555555555"

	home := t.TempDir()
	projectDir := filepath.Join(home, ".claude", "projects", "-tmp-fixture")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create fixture project dir: %v", err)
	}
	// One `user` entry is all parseSessionHead needs to report a prompt, a
	// creation time and a turn count.
	rollout := `{"type":"user","timestamp":"2026-08-22T00:00:00Z","message":{"role":"user","content":"fixture prompt"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatalf("write fixture rollout: %v", err)
	}

	t.Setenv("HOME", home)
	return sessionID
}
