package memories

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PrepareSession asks the caller's context twice, and the two answers mean
// different things. The check at the top means "you were already gone before we
// wrote anything". The check inside the recent-files error path means "you left
// while we were scanning" — and that one exists to stop a withdrawal being
// reported as a warning on stderr plus a successful prepare, after the identity
// and instruction memories have already been written on the caller's behalf.
//
// Measured before this file existed: deleting the second check entirely left the
// whole suite green. TestPrepareSessionFailsForACanceledCaller cancels before
// the call, so it is answered by the first check and never reaches the second.
// Both producers return a context error, so "PrepareSession returned
// context.Canceled" is exactly the assertion that cannot tell them apart.
//
// What tells them apart is what is in the store when the error arrives. The
// first check returns before loadIdentity; the second returns after it. So this
// test asserts the error AND the identity memory, and that pair names the
// producer.

// gitStandInThatBlocksUntilKilled puts a `git` on PATH that announces itself and
// then blocks. The scan spawns git through exec.CommandContext, so everything
// below the stand-in is real — real PATH lookup, real process, real kill on
// cancel. Announcing rather than sleeping a fixed time is what keeps the test
// off the clock: the caller cancels because the scan has demonstrably started,
// not because an estimate of how long it takes has elapsed.
//
// The script `exec`s the sleep rather than spawning it, so the blocking process
// is the one exec.CommandContext holds and kills. Spawned as a child it would
// outlive the kill still holding git's stdout pipe, and Wait would sit out the
// whole of waitDelayAfterGitExits before returning.
func gitStandInThatBlocksUntilKilled(t *testing.T) (started string) {
	t.Helper()

	binDir := t.TempDir()
	started = filepath.Join(binDir, "git-started")
	script := "#!/bin/sh\ntouch " + started + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write git stand-in: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return started
}

// newRepoShapedWorkspace gives the scan a root that takes the git strategy: the
// git path is chosen by a .git directory existing, nothing more.
func newRepoShapedWorkspace(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("make .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "recent.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return root
}

// cancelOnceTheScanHasStarted withdraws the caller the moment the stand-in
// announces itself. It reports nothing on its own — a stand-in that never ran
// leaves the announce file absent, and every caller below asserts on that file
// from the test goroutine, where a failure is allowed to stop the test.
func cancelOnceTheScanHasStarted(started string, cancel context.CancelFunc) {
	go func() {
		defer cancel()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func assertTheStandInRan(t *testing.T, started string) {
	t.Helper()

	if _, err := os.Stat(started); err != nil {
		t.Fatalf("the git stand-in never announced itself at %s — the rig did not run, so this result is about nothing", started)
	}
}

// The rig guard. A stand-in that cannot execute, or that PATH never reaches,
// would make the test below pass for the wrong reason: git would fail instantly,
// the scan would fall through to the mtime walk, and a green run would say
// nothing about cancellation. This asserts the stand-in is the git that runs and
// that canceling the caller is what ends it.
func TestTheGitStandInIsWhatTheScanSpawns(t *testing.T) {
	started := gitStandInThatBlocksUntilKilled(t)
	root := newRepoShapedWorkspace(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnceTheScanHasStarted(started, cancel)

	_, err := FindRecentlyModified(ctx, root, 24*time.Hour)
	assertTheStandInRan(t, started)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FindRecentlyModified returned %v, want context.Canceled", err)
	}
}

// The gap this file was written for.
func TestACallerWhoLeavesDuringTheScanIsNotToldTheSessionWasPrepared(t *testing.T) {
	started := gitStandInThatBlocksUntilKilled(t)
	store := newStoreForContextTest(t)

	cfg := prepareSessionConfigFor(newRepoShapedWorkspace(t))
	// No cap of its own, so the scan ends when the caller withdraws and not
	// because it ran out of its own time — those are the two events the check
	// under test exists to keep apart.
	cfg.RecencyScanTimeout = -1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnceTheScanHasStarted(started, cancel)

	err := store.PrepareSession(ctx, cfg)
	assertTheStandInRan(t, started)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareSession returned %v, want context.Canceled — a caller that withdrew mid-scan was told the session was prepared", err)
	}

	// The discriminating half. The entry check returns before any memory is
	// written; reaching this error with the identity already in the store is
	// what proves the mid-scan check answered, and not the entry check.
	identity, getErr := store.Get("identity")
	if getErr != nil || identity == nil {
		t.Fatalf("identity not in the store (%v) — this cancellation was caught at entry, so the mid-scan check is still unpinned", getErr)
	}
}
