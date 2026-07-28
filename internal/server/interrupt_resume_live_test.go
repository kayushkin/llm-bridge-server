package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// buildMockHarnessOnPath builds cmd/mock-harness as `llm-bridge-mock` (the
// name msg.HarnessBinaryName(HarnessMock) resolves to) into a temp dir and
// prepends that dir to PATH so the manager's exec.LookPath finds it. Returns
// the binary path. This gives the interrupt/resume handlers a REAL live
// subprocess to act on — the whole point of the Bug-2 fix is that the gate
// keys off the live process registry, which can only be exercised with an
// actual registered process.
func buildMockHarnessOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, msg.HarnessBinaryName(msg.HarnessMock))
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/mock-harness")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build mock-harness: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// waitForSessionState polls the store until the session reaches want or the
// deadline elapses. A Go-test poll (not a Bash leading sleep) is the right
// tool for a subprocess-driven state transition.
func waitForSessionState(t *testing.T, st *store.Store, bridgeID string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(bridgeID)
		if err == nil && sess != nil && sess.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess, _ := st.GetSession(bridgeID)
	got := "<none>"
	if sess != nil {
		got = sess.State
	}
	t.Fatalf("session %s never reached state %q within %s (last=%q)", bridgeID, want, timeout, got)
}

// TestInterruptSession_LiveProcessDuringToolRunning is the Bug-2 regression.
// With a live process parked in tool_running (the mock's "hang" mode), the old
// handler 409'd because state != "running"; the fixed handler consults the
// process registry via Stop(), succeeds, and returns 200. Without the mock now
// emitting tool_running this failing state was unreachable (finding §5).
func TestInterruptSession_LiveProcessDuringToolRunning(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st := testServer(t)

	const bridgeID = "br_int_live"
	sess := &store.Session{SessionID: bridgeID, Harness: msg.HarnessMock, State: "idle"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := srv.harness.Start(context.Background(), sess); err != nil {
		t.Fatalf("start mock process: %v", err)
	}
	t.Cleanup(func() { srv.harness.Kill(bridgeID) })

	// "hang" parks the mock in tool_running until it is interrupted.
	if err := srv.harness.Send(bridgeID, "please hang", nil); err != nil {
		t.Fatalf("send message: %v", err)
	}
	waitForSessionState(t, st, bridgeID, string(msg.SessionToolRunning), 5*time.Second)

	resp := doJSON(t, srv, "POST", "/sessions/"+bridgeID+"/interrupt", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("interrupt during tool_running: status = %d, want 200", resp.StatusCode)
	}
	got := decodeJSON[msg.ManagedSession](t, resp)
	if got.State != string(msg.SessionIdle) {
		t.Errorf("post-interrupt state = %q, want idle", got.State)
	}
}

// TestResumeSession_AlreadyRunning proves the Bug-2 resume gate keys off the
// live process registry: a session with a live process is already running and
// has nothing to resume → 409.
func TestResumeSession_AlreadyRunning(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st := testServer(t)

	const bridgeID = "br_res_live"
	sess := &store.Session{SessionID: bridgeID, Harness: msg.HarnessMock, State: "running"}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := srv.harness.Start(context.Background(), sess); err != nil {
		t.Fatalf("start mock process: %v", err)
	}
	t.Cleanup(func() { srv.harness.Kill(bridgeID) })

	resp := doJSON(t, srv, "POST", "/sessions/"+bridgeID+"/resume", nil)
	if resp.StatusCode != 409 {
		t.Errorf("resume with live process: status = %d, want 409 (already running)", resp.StatusCode)
	}
}

// TestResumeSession_ResumableStartsProcess proves the happy path is preserved:
// a session with NO live process but a bound instance is genuinely resumable and
// spawns a fresh process → 200, state running. (Old code would have 409'd this
// the moment its stale state string wasn't "idle".)
func TestResumeSession_ResumableStartsProcess(t *testing.T) {
	buildMockHarnessOnPath(t)
	srv, st, instID := testServerWithInstance(t, msg.HarnessMock)

	const bridgeID = "br_res_resumable"
	// State "running" but no live process == a crashed/interrupted session.
	sess := &store.Session{SessionID: bridgeID, Harness: msg.HarnessMock, State: "running", InstanceID: instID}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { srv.harness.Kill(bridgeID) })

	resp := doJSON(t, srv, "POST", "/sessions/"+bridgeID+"/resume", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("resume resumable session: status = %d, want 200", resp.StatusCode)
	}
	if !srv.harness.HasProcess(bridgeID) {
		t.Errorf("resume returned 200 but no live process was registered")
	}
}
