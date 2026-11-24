//go:build unix

package childprocess

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
)

// signalProcessGroup answers a caller in two different registers and os/exec
// reads the difference. os.ErrProcessDone means "the group is already gone",
// which exec treats as a command that finished on its own; any other error is a
// cancellation that genuinely failed. Both of the ways the group can be gone
// are reported through that one value, and they arrive by different routes: a
// process that never started at all, and a live pid whose kill comes back
// ESRCH.
//
// The suite reaches both routes and, before this test, could not tell either
// of them from an arbitrary failure: swapping os.ErrProcessDone for
// syscall.EINVAL in either arm left every test passing.
//
// Signal 0 is deliberate. It delivers nothing and only reports whether a
// signal could have been sent, so the ESRCH arm is exercised without aiming a
// real signal at a group id whose pid the kernel is free to have reissued.
func TestAGroupThatIsAlreadyGoneIsReportedAsFinishedRatherThanAsAFailure(t *testing.T) {
	neverStarted := signalProcessGroup(nil, syscall.Signal(0))
	if !errors.Is(neverStarted, os.ErrProcessDone) {
		t.Errorf("a process that never started: want os.ErrProcessDone, got %v", neverStarted)
	}

	exited := NewCommand(context.Background(), "bash", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	alreadyReaped := signalProcessGroup(exited.Cmd.Process, syscall.Signal(0))
	if !errors.Is(alreadyReaped, os.ErrProcessDone) {
		t.Errorf("a group that has exited: want os.ErrProcessDone, got %v", alreadyReaped)
	}
}

// processGroupExists is the whole reason the ESRCH mapping above has to be
// exact: it reads that error back as a boolean, so an ESRCH reported as
// anything other than os.ErrProcessDone would make an empty group look
// populated and the kill escalation would fire at nothing.
func TestAnEmptyGroupDoesNotLookPopulated(t *testing.T) {
	if processGroupExists(nil) {
		t.Error("a process that never started reads as a live group")
	}

	exited := NewCommand(context.Background(), "bash", "-c", "exit 0")
	if err := exited.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if processGroupExists(exited.Cmd.Process) {
		t.Error("a group whose only member has exited reads as a live group")
	}
}
