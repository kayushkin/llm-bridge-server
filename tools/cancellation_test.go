package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The two spawn sites in this package used to take no context at all, so an
// interrupt, a deadline and a timeout all had nothing to reach. runBuild is the
// expensive one: it is a `go build ./...` fired by the task plan when the last
// task is checked off, and before this it ran to completion whatever the caller
// wanted. Measured on the old code: still blocked eight seconds after the
// caller canceled, on a command that runs for thirty.

// cancellationDeadline bounds every test here. It is well above the time a
// canceled command needs (SIGINT, then SIGKILL after the two-second grace) and
// well below the runtime of the commands being canceled, so a test that hits
// it has found a command that ignored its context rather than a slow machine.
const cancellationDeadline = 10 * time.Second

func TestRunBuildStopsWhenTheCallerCancels(t *testing.T) {
	restore := useBuildCommand(t, "sleep 60")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan BuildResult, 1)
	go func() { finished <- runBuild(ctx, t.TempDir()) }()

	// Let the shell actually start before canceling, so this exercises
	// stopping a running command rather than refusing to start one.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case result := <-finished:
		if !result.Stopped {
			t.Errorf("a canceled build reported Stopped=false: %+v", result)
		}
	case <-time.After(cancellationDeadline):
		t.Fatal("runBuild ignored its canceled context")
	}
	restore()
}

// A build that is stopped has decided nothing about the code. Reporting it as a
// failure is what makes the caller write a "Fix build error" task describing a
// build that never ran, so the two outcomes have to stay distinguishable.
func TestCanceledBuildIsNotReportedAsAFailedBuild(t *testing.T) {
	restore := useBuildCommand(t, "sleep 60")
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result := runBuild(ctx, t.TempDir())
	if result.Success {
		t.Errorf("a canceled build reported success: %+v", result)
	}
	if !result.Stopped {
		t.Errorf("a canceled build is indistinguishable from a failing build: %+v", result)
	}
	if !strings.Contains(result.Output, "stopped") {
		t.Errorf("output does not say the build was stopped: %q", result.Output)
	}
}

// A build that genuinely fails must still be a failure — otherwise the
// assertion above could pass by calling everything "stopped".
func TestFailingBuildIsStillAFailure(t *testing.T) {
	restore := useBuildCommand(t, "exit 3")
	defer restore()

	result := runBuild(context.Background(), t.TempDir())
	if result.Success || result.Stopped {
		t.Errorf("a genuinely failing build was not reported as a failure: %+v", result)
	}
}

func TestSucceedingBuildIsStillASuccess(t *testing.T) {
	restore := useBuildCommand(t, "echo built")
	defer restore()

	result := runBuild(context.Background(), t.TempDir())
	if !result.Success || result.Stopped {
		t.Errorf("a passing build was not reported as a success: %+v", result)
	}
}

// The direct child is `bash -c`, so everything the build command starts is a
// grandchild, and killing bash alone leaves those grandchildren running. This
// is the leak internal/childprocess exists to close, pinned here at the call
// site rather than only in that package's own tests.
//
// The assertion is that the process is gone, checked by pid. A marker file
// would be a proxy and a misleading one: interrupting `(sleep 3; touch X) &`
// writes X immediately, because SIGINT ends the sleep and the shell moves on to
// the next command in the list rather than stopping. Measured — the marker
// appeared 200ms after the interrupt, not three seconds later.
func TestCanceledBuildLeavesNothingRunning(t *testing.T) {
	workingDirectory := t.TempDir()
	pidFile := filepath.Join(workingDirectory, "backgrounded.pid")
	// A backgrounded simple command is the shape that genuinely outlives the
	// interrupt: a non-interactive bash sets SIGINT to ignored in a background
	// job, so only the escalation to SIGKILL on the whole group reaches it.
	restore := useBuildCommand(t, "sleep 47 & echo $! > "+pidFile+"; sleep 60")
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runBuild(ctx, workingDirectory)

	backgrounded := readPidWhenWritten(t, pidFile)
	if processIsAlive(backgrounded, 5*time.Second) {
		_ = syscall.Kill(backgrounded, syscall.SIGKILL) // do not leak it out of the test
		t.Errorf("a process started by the canceled build (pid %d) was still running when runBuild returned", backgrounded)
	}
}

func TestRecentFilesGitStopsWhenTheCallerCancels(t *testing.T) {
	repository := initRepository(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: the command must not run to completion

	// Asserting only that an error came back would leave the cancellation
	// indistinguishable from "this is not a git repository", and those two go
	// opposite ways one level up: findRecentlyModified serves the second with a
	// full mtime walk and must not serve the first with anything.
	_, err := findRecentlyModifiedGit(ctx, repository, time.Hour)
	if err == nil {
		t.Fatal("git log ran to completion against a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want the cancellation reported as context.Canceled, got %v", err)
	}
}

// The mtime scan is the fallback for "git could not answer". A canceled git
// log also fails, and failing is exactly what used to send the caller into a
// full tree walk — so canceling the cheap half would have started the
// expensive one.
func TestCancelingDoesNotFallBackToTheFilesystemWalk(t *testing.T) {
	repository := initRepository(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	files, err := findRecentlyModified(ctx, repository, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected the cancellation to be reported, got err=%v files=%d", err, len(files))
	}
	if len(files) != 0 {
		t.Errorf("the fallback tree walk ran anyway and returned %d files", len(files))
	}
}

// The fallback itself must still work, so the assertion above cannot pass by
// never falling back at all.
func TestFilesystemWalkStillRunsWhenGitCannotAnswer(t *testing.T) {
	notARepository := t.TempDir()
	if err := os.WriteFile(filepath.Join(notARepository, "recent.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := findRecentlyModified(context.Background(), notARepository, time.Hour)
	if err != nil {
		t.Fatalf("the mtime fallback failed: %v", err)
	}
	if len(files) == 0 {
		t.Error("the mtime fallback found nothing in a directory with a fresh file")
	}
}

func useBuildCommand(t *testing.T, command string) (restore func()) {
	t.Helper()
	previous := TaskPlanBuildCommand
	TaskPlanBuildCommand = command
	return func() { TaskPlanBuildCommand = previous }
}

func initRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	run := func(name string, arguments ...string) {
		command := exec.Command(name, arguments...)
		command.Dir = directory
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, arguments, err, output)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "test@example.invalid")
	run("git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(directory, "tracked.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return directory
}
