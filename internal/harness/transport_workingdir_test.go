package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The three transports each hand the resolved directory to a different thing —
// a cd in a remote shell command, a field on a spawn message, an env var on a
// sidecar process — so each needs its own end-to-end check that the session
// level survives the trip. Testing only the resolver leaves three call sites
// where the session can be dropped without a single test noticing.

// TestSSHRunsTheSessionInItsOwnWorkingDirectory drives the real startSSH
// against a stand-in ssh binary that records its argument list, so the
// assertion is about the command the remote host would actually run.
func TestSSHRunsTheSessionInItsOwnWorkingDirectory(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "ssh-args.txt")
	installFakeSSHOnPath(t, recordTo)

	manager := newManagerForWorkingDirTest(t)
	sess := &store.Session{SessionID: "sess-ssh", Harness: msg.HarnessClaudeCode, WorkingDir: "/srv/session"}
	inst := &msg.Instance{
		ID:         "inst-cc-remote",
		WorkingDir: "/srv/instance",
		Machine: &msg.Machine{
			ID:                "m_remote",
			Name:              "remote",
			Transport:         msg.TransportSSH,
			Hostname:          "example.invalid",
			SSHUser:           "agent",
			DefaultWorkingDir: "/srv/machine",
		},
	}

	proc, err := manager.startSSH(context.Background(), sess, inst, "")
	if err != nil {
		t.Fatalf("startSSH: %v", err)
	}
	defer proc.Kill()

	args := waitForRecordedFile(t, recordTo)
	if !strings.Contains(args, "cd /srv/session &&") {
		t.Errorf("ssh command %q does not cd to the session working directory", args)
	}
	if strings.Contains(args, "/srv/instance") || strings.Contains(args, "/srv/machine") {
		t.Errorf("ssh command %q used a lower cascade level than the session's own directory", args)
	}
}

// TestARunnerSpawnCarriesTheSessionWorkingDirectory reads the spawn message off
// the connection's outgoing queue. RunnerSpawn.WorkingDir is the only thing
// that tells the remote runner where to start the harness, so a session
// directory that never reaches this field is a session directory the runner
// transport ignores.
func TestARunnerSpawnCarriesTheSessionWorkingDirectory(t *testing.T) {
	manager := newManagerForWorkingDirTest(t)
	registry := NewRunnerRegistry()
	conn := &RunnerConnection{
		MachineName: "remote",
		Hello:       &msg.RunnerHello{OS: "linux", Arch: "amd64"},
		registry:    registry,
		outgoing:    make(chan *msg.RunnerMessage, 4),
		sessions:    map[string]*RunnerProcess{},
	}
	registry.register(conn)
	manager.runners = registry

	sess := &store.Session{SessionID: "sess-runner", Harness: msg.HarnessClaudeCode, WorkingDir: "/srv/session"}
	inst := &msg.Instance{
		ID:         "inst-cc-runner",
		WorkingDir: "/srv/instance",
		Machine:    &msg.Machine{ID: "m_remote", Name: "remote", Transport: msg.TransportRunner},
	}

	if _, err := manager.startRunner(context.Background(), sess, inst, ""); err != nil {
		t.Fatalf("startRunner: %v", err)
	}

	select {
	case m := <-conn.outgoing:
		if m.Spawn == nil {
			t.Fatalf("runner message %q carries no spawn", m.Type)
		}
		if m.Spawn.WorkingDir != "/srv/session" {
			t.Errorf("spawn working_dir = %q, want the session's own /srv/session", m.Spawn.WorkingDir)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no spawn message was queued for the runner")
	}
}

// TestThePTYSidecarIsToldTheSessionWorkingDirectory closes the gap between
// ptyChildWorkingDir being correct and being called with the right argument. The
// sidecar tails the rollout file under the directory it is given, so a sidecar
// told bridge-server's own directory while the child runs elsewhere produces a
// session with no telemetry at all and nothing logged to say so.
func TestThePTYSidecarIsToldTheSessionWorkingDirectory(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "sidecar-cwd.txt")
	installFakeHarnessWithSidecarOnPath(t, msg.HarnessClaudeCode, recordTo)
	sessionDir := t.TempDir()

	manager := newManagerForWorkingDirTest(t)
	// A non-empty localBridgeURL is what makes the manager spawn a sidecar
	// at all; with it empty the sidecar branch is skipped entirely.
	manager.localBridgeURL = "http://127.0.0.1:8160"

	sess := &store.Session{
		SessionID:  "sess-pty-sidecar",
		Harness:    msg.HarnessClaudeCode,
		Mode:       msg.SessionModePTY,
		WorkingDir: sessionDir,
	}
	inst := &msg.Instance{
		ID:         "inst-cc-local",
		WorkingDir: t.TempDir(),
		Machine:    &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal},
	}

	proc, err := manager.StartOnInstance(context.Background(), sess, inst, "")
	if err != nil {
		t.Fatalf("StartOnInstance (pty): %v", err)
	}
	defer proc.Kill()

	got := strings.TrimSpace(waitForRecordedFile(t, recordTo))
	if got != sessionDir {
		t.Errorf("sidecar was told to tail %q, want the session working directory %q", got, sessionDir)
	}
	if got == mustGetwd(t) {
		t.Errorf("sidecar resolved bridge-server's own directory %q independently of the child", got)
	}
}

// installFakeSSHOnPath puts a stand-in `ssh` on PATH that records the argument
// list it was called with, then blocks so the caller controls its lifetime.
func installFakeSSHOnPath(t *testing.T, recordTo string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" > " + recordTo + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installFakeHarnessWithSidecarOnPath installs a wrapper that plays both roles
// the manager asks of one binary in pty mode: invoked with -otel-sidecar it
// records LLMBRIDGE_PTY_CWD and completes the endpoint handshake; invoked
// without it, it is the harness child.
func installFakeHarnessWithSidecarOnPath(t *testing.T, h msg.Harness, recordTo string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-otel-sidecar\" ]; then\n" +
		"  printf '%s\\n' \"$LLMBRIDGE_PTY_CWD\" > " + recordTo + "\n" +
		"  echo 'http://127.0.0.1:1/v1'\n" +
		"  exec sleep 30\n" +
		"fi\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, msg.HarnessBinaryName(h)), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	t.Setenv("PATH", binDir)
}

func waitForRecordedFile(t *testing.T, recordTo string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordTo)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing was recorded at %s", recordTo)
	return ""
}
