package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// These tests drive the real StartOnInstance rather than StartProcess, because
// resolving the working directory correctly and then handing it to the spawn
// are two separate things and only the second one is observable to a user.
//
// Without them the package's whole suite still passes when the local branch
// passes "" to StartProcess — which is the very shape of the bug being fixed:
// a value that is read, stored, displayed, and then dropped on the way to the
// process that needed it.

func TestStartOnInstanceRunsALocalHarnessInTheInstanceWorkingDirectory(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	workingDir := t.TempDir()

	manager := newManagerForWorkingDirTest(t)
	sess := &store.Session{SessionID: "sess-on-instance", Harness: msg.HarnessClaudeCode}
	inst := &msg.Instance{
		ID:         "inst-cc-local",
		WorkingDir: workingDir,
		Machine:    &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal},
	}

	proc, err := manager.StartOnInstance(context.Background(), sess, inst, "")
	if err != nil {
		t.Fatalf("StartOnInstance: %v", err)
	}
	defer proc.Kill()

	got := waitForRecordedCwd(t, recordTo)
	if want := resolvePath(t, workingDir); got != want {
		t.Errorf("harness ran in %q, want the instance working directory %q", got, want)
	}
}

func TestStartOnInstanceFallsBackToTheMachineDefaultWorkingDirectory(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	machineDefault := t.TempDir()

	manager := newManagerForWorkingDirTest(t)
	sess := &store.Session{SessionID: "sess-machine-default", Harness: msg.HarnessClaudeCode}
	inst := &msg.Instance{
		ID:      "inst-cc-local",
		Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal, DefaultWorkingDir: machineDefault},
	}

	proc, err := manager.StartOnInstance(context.Background(), sess, inst, "")
	if err != nil {
		t.Fatalf("StartOnInstance: %v", err)
	}
	defer proc.Kill()

	got := waitForRecordedCwd(t, recordTo)
	if want := resolvePath(t, machineDefault); got != want {
		t.Errorf("harness ran in %q, want the machine default working directory %q", got, want)
	}
}

// A misconfigured directory must stop the spawn rather than quietly starting
// the session somewhere else. Before this fix the directory was ignored
// entirely, so a typo in it was invisible; failing loudly is only useful if
// nothing is left running behind the error.
func TestStartOnInstanceRefusesAWorkingDirectoryThatDoesNotExist(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	manager := newManagerForWorkingDirTest(t)
	sess := &store.Session{SessionID: "sess-bad-dir", Harness: msg.HarnessClaudeCode}
	inst := &msg.Instance{
		ID:         "inst-cc-local",
		WorkingDir: missing,
		Machine:    &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal},
	}

	proc, err := manager.StartOnInstance(context.Background(), sess, inst, "")
	if err == nil {
		proc.Kill()
		t.Fatalf("StartOnInstance started a session in a directory that does not exist")
	}
	if !strings.Contains(err.Error(), "inst-cc-local") {
		t.Errorf("error %q does not name the instance the operator has to fix", err)
	}
	if _, statErr := os.Stat(recordTo); statErr == nil {
		t.Errorf("a harness process was spawned despite the refused working directory")
	}
}

// The pty branch spawns through a different function with its own copy of the
// argument list, so it needs its own end-to-end test: passing the directory to
// one spawn and not the other is a single-word mistake that nothing else here
// would notice.
func TestStartOnInstanceRunsAPTYHarnessInTheInstanceWorkingDirectory(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	workingDir := t.TempDir()

	// localBridgeURL is empty, so the OTel sidecar is skipped and this
	// exercises the pty spawn on its own.
	manager := newManagerForWorkingDirTest(t)
	sess := &store.Session{
		SessionID: "sess-pty",
		Harness:   msg.HarnessClaudeCode,
		Mode:      msg.SessionModePTY,
	}
	inst := &msg.Instance{
		ID:         "inst-cc-local",
		WorkingDir: workingDir,
		Machine:    &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal},
	}

	proc, err := manager.StartOnInstance(context.Background(), sess, inst, "")
	if err != nil {
		t.Fatalf("StartOnInstance (pty): %v", err)
	}
	defer proc.Kill()

	got := waitForRecordedCwd(t, recordTo)
	if want := resolvePath(t, workingDir); got != want {
		t.Errorf("pty harness ran in %q, want the instance working directory %q", got, want)
	}
}

func newManagerForWorkingDirTest(t *testing.T) *Manager {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewManager(st, "", "", "", 0, nil)
}

// installFakeHarnessOnPath puts a stand-in wrapper binary under the name the
// server looks up for this harness type, and points PATH at it, so Available()
// resolves to something this test controls.
func installFakeHarnessOnPath(t *testing.T, h msg.Harness, recordTo string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\npwd > " + recordTo + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, msg.HarnessBinaryName(h)), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", binDir)
}
