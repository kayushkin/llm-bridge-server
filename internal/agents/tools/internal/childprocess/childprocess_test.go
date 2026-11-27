//go:build unix

package childprocess

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunReturnsOutputAndStatusOfAnOrdinaryCommand(t *testing.T) {
	output, err := NewCommand(context.Background(), "bash", "-c", "echo out; echo err >&2").CombinedOutput()
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}
	if got := string(output); !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("want both streams, got %q", got)
	}

	err = NewCommand(context.Background(), "bash", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("want exit status 3, got %v", err)
	}
}

// Canceling must reach past the shell to what the shell started. Without
// useOwnProcessGroup the grandchild is in this process's group, the cancel hits
// the direct child alone, and the grandchild runs to its own completion.
func TestCancelStopsAGrandchildOfTheCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	command := NewCommand(ctx, "bash", "-c", "sleep 60 & echo $! >"+pidFile+"; sleep 60")
	if err := command.Cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	grandchild := waitForRecordedPid(t, pidFile)
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.wait() }()

	cancel()

	select {
	case <-waitDone:
	case <-time.After(InterruptToKillGrace + 5*time.Second):
		t.Fatal("wait did not return after cancel")
	}
	if alive := waitForProcessToExit(grandchild, InterruptToKillGrace+3*time.Second); alive {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the cancel", grandchild)
	}
}

// SIGINT is a request, and a process is free to ignore it. The escalation to
// SIGKILL is what makes cancellation a guarantee rather than a suggestion.
func TestCancelKillsAGrandchildThatIgnoresTheInterrupt(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	command := NewCommand(ctx, "bash", "-c",
		"bash -c 'trap \"\" INT; echo $$ >"+pidFile+"; sleep 60' & trap '' INT; sleep 60")
	if err := command.Cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	grandchild := waitForRecordedPid(t, pidFile)
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.wait() }()

	start := time.Now()
	cancel()

	select {
	case <-waitDone:
	case <-time.After(InterruptToKillGrace + WaitDelayAfterExit + 5*time.Second):
		t.Fatal("wait did not return after cancel")
	}
	if alive := waitForProcessToExit(grandchild, InterruptToKillGrace+3*time.Second); alive {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d ignored SIGINT and was never killed", grandchild)
	}
	if elapsed := time.Since(start); elapsed < InterruptToKillGrace {
		t.Fatalf("killed after %v, before the %v grace period had run", elapsed, InterruptToKillGrace)
	}
}

// A command that exits while something it backgrounded still holds the output
// pipe used to block Wait forever. WaitDelayAfterExit bounds that.
func TestWaitReturnsWhenABackgroundProcessHoldsTheOutputPipe(t *testing.T) {
	command := NewCommand(context.Background(), "bash", "-c", "echo before; sleep 60 &")
	returned := make(chan struct{})
	var output []byte
	var err error
	go func() {
		output, err = command.CombinedOutput()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(WaitDelayAfterExit + 5*time.Second):
		t.Fatal("CombinedOutput never returned; a background process held the pipe open")
	}
	// The backgrounded sleep is this test's subject and is still running: the
	// command was never canceled, so nothing else will stop it.
	defer killProcessGroup(command.Process)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("want exec.ErrWaitDelay, got %v", err)
	}
	// The bytes the command did produce must survive the bounded wait.
	if got := string(output); !strings.Contains(got, "before") {
		t.Fatalf("output written before the delay was lost, got %q", got)
	}
}

// Stopping a command must not edit what it already produced.
func TestCancelKeepsTheOutputWrittenBeforeIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	command := NewCommand(ctx, "bash", "-c", "echo written-before-cancel; sleep 60")
	returned := make(chan struct{})
	var output []byte
	go func() {
		output, _ = command.CombinedOutput()
		close(returned)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-returned:
	case <-time.After(InterruptToKillGrace + WaitDelayAfterExit + 5*time.Second):
		t.Fatal("CombinedOutput never returned after cancel")
	}
	if got := string(output); !strings.Contains(got, "written-before-cancel") {
		t.Fatalf("cancel dropped output the command had already written, got %q", got)
	}
}

func waitForRecordedPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(contents))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid was recorded in %s", pidFile)
	return 0
}

// waitForProcessToExit reports whether the process was still running when the
// timeout ran out. A killed process that nobody has reaped yet counts as
// exited: the grandchildren here are orphans, so reaping is up to whichever
// process adopted them, and on a loaded host that can take longer than the
// timeout.
func waitForProcessToExit(pid int, timeout time.Duration) (stillAlive bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processIsRunning(pid) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processIsRunning(pid)
}

// Cancellation asks before it insists: the group gets SIGINT, so a command that
// traps it runs its cleanup and exits on its own terms. Leaving os/exec's
// default Cancel in place would SIGKILL the shell instead and the trap would
// never run.
func TestCancelLetsTheCommandRunItsCleanupHandler(t *testing.T) {
	cleanupMarker := filepath.Join(t.TempDir(), "cleaned")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	command := NewCommand(ctx, "bash", "-c",
		"trap 'echo cleaned >"+cleanupMarker+"; exit 0' INT; echo ready; sleep 60")
	returned := make(chan struct{})
	go func() {
		_, _ = command.CombinedOutput()
		close(returned)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-returned:
	case <-time.After(InterruptToKillGrace + WaitDelayAfterExit + 5*time.Second):
		t.Fatal("CombinedOutput never returned after cancel")
	}
	if _, err := os.Stat(cleanupMarker); err != nil {
		t.Fatalf("the command was never given the chance to clean up: %v", err)
	}
}

// Run returns the error from starting the command and the error from waiting
// for it through the same single value, and the two mean opposite things: a
// command that never launched has produced no output and no verdict, while a
// command that launched and exited badly has produced both. A caller that can
// only see "it failed" cannot tell a bad command line from a command whose own
// answer was failure.
//
// The wait side was already pinned by the exit-status and ErrWaitDelay
// assertions above. The start side was not: replacing Start's error with any
// other well-formed error left every test passing.
func TestRunTellsACommandThatNeverLaunchedFromOneThatRanAndFailed(t *testing.T) {
	neverLaunched := NewCommand(context.Background(), filepath.Join(t.TempDir(), "no-such-program")).Run()
	if !errors.Is(neverLaunched, os.ErrNotExist) {
		t.Errorf("a missing program: want a not-exist error, got %v", neverLaunched)
	}

	ranAndFailed := NewCommand(context.Background(), "bash", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(ranAndFailed, &exitErr) {
		t.Errorf("a command that ran and exited 3: want an *exec.ExitError, got %v", ranAndFailed)
	}
	if errors.Is(ranAndFailed, os.ErrNotExist) {
		t.Error("a command that ran and exited 3 was reported as a missing program")
	}
}

// A context already canceled when Run is called stops the command before it
// is started, so the failure comes out of Start rather than out of Wait. It
// still has to say that it was canceled: findRecentlyModified reads exactly
// this distinction to decide whether "git could not answer" should fall back
// to a full tree walk, and a cancellation reported as an ordinary failure
// would start the expensive walk the caller just asked to stop.
func TestRunReportsACancellationThatArrivedBeforeTheCommandStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewCommand(ctx, "bash", "-c", "exit 0").Run()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
