package harness

import (
	"testing"
	"time"
)

// A mode switch kills a session's process and starts another for the same
// session. The old process's exit cleanup removes the session's process entry,
// zeroes its PID and, for pty, records it completed — so it must finish before
// the new process exists. KillAndWaitForCleanup is that ordering; these hold
// it without spawning anything, by standing a blocked function in for
// readEvents / watchPTYExit.

func TestKillAndWaitForCleanupWaitsForTheExitCleanup(t *testing.T) {
	m := NewManager(nil, "", "", "", 0, nil)
	release := make(chan struct{})

	m.mu.Lock()
	cleanupDone := m.trackProcessCleanupLocked("br_test")
	m.mu.Unlock()
	go m.runProcessCleanupTracked("br_test", cleanupDone, func() { <-release })

	returned := make(chan error, 1)
	go func() { returned <- m.KillAndWaitForCleanup("br_test", 5*time.Second) }()

	select {
	case err := <-returned:
		t.Fatalf("returned (%v) while the old process's cleanup was still running: the next spawn would be overwritten by it", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("KillAndWaitForCleanup: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return after the cleanup finished")
	}
}

func TestKillAndWaitForCleanupReportsACleanupThatDoesNotFinish(t *testing.T) {
	m := NewManager(nil, "", "", "", 0, nil)
	release := make(chan struct{})
	defer close(release)

	m.mu.Lock()
	cleanupDone := m.trackProcessCleanupLocked("br_test")
	m.mu.Unlock()
	go m.runProcessCleanupTracked("br_test", cleanupDone, func() { <-release })

	if err := m.KillAndWaitForCleanup("br_test", 50*time.Millisecond); err == nil {
		t.Fatal("no error for a cleanup still running at the timeout; the caller would spawn into it")
	}
}

func TestKillAndWaitForCleanupReturnsAtOnceWithNoProcess(t *testing.T) {
	m := NewManager(nil, "", "", "", 0, nil)
	if err := m.KillAndWaitForCleanup("br_never_started", time.Second); err != nil {
		t.Fatalf("KillAndWaitForCleanup with no process: %v", err)
	}
}

func TestAFinishedCleanupLeavesTheNewerProcessesChannel(t *testing.T) {
	m := NewManager(nil, "", "", "", 0, nil)

	m.mu.Lock()
	older := m.trackProcessCleanupLocked("br_test")
	newer := m.trackProcessCleanupLocked("br_test")
	m.mu.Unlock()

	m.runProcessCleanupTracked("br_test", older, func() {})

	m.mu.Lock()
	current := m.processCleanups["br_test"]
	m.mu.Unlock()
	if current != newer {
		t.Fatal("the older process's cleanup removed the newer process's channel, so a later wait would not wait for it")
	}
}
