//go:build unix

package childprocess

import (
	"os"
	"os/exec"
	"syscall"
)

// useOwnProcessGroup puts the child in a process group of its own, whose id is
// the child's pid. Without it the child joins this process's group and a signal
// aimed at the group would hit this process too.
func useOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// interruptProcessGroup asks the whole group to stop. It reports
// os.ErrProcessDone when the group is already gone, which os/exec treats as a
// command that finished on its own rather than as a failed cancellation.
func interruptProcessGroup(process *os.Process) error {
	return signalProcessGroup(process, syscall.SIGINT)
}

// killProcessGroup stops the whole group without asking.
func killProcessGroup(process *os.Process) error {
	return signalProcessGroup(process, syscall.SIGKILL)
}

func signalProcessGroup(process *os.Process, signal syscall.Signal) error {
	if process == nil {
		return os.ErrProcessDone
	}
	// The group id equals the pid of the process that led the group, which
	// useOwnProcessGroup made this child.
	err := syscall.Kill(-process.Pid, signal)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

// processGroupExists reports whether the group still has a member. Signal 0
// delivers nothing and only reports whether a signal could be sent.
//
// A group id is a pid, and a pid becomes reusable once nothing holds it. While
// the group has members it is pinned, which covers the case this package cares
// about — grandchildren that outlived the shell. A group that empties between
// this check and the kill that follows it could in principle have had its id
// taken by a process that has since made itself a group leader; that is a
// sub-millisecond window inside a two-second grace period, and the alternative
// is leaving the grandchildren running.
func processGroupExists(process *os.Process) bool {
	return signalProcessGroup(process, syscall.Signal(0)) == nil
}
