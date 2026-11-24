//go:build !unix

package childprocess

import (
	"os"
	"os/exec"
)

// These platforms have no process group to signal, so a canceled command
// reaches the direct child only and grandchildren are left running — the
// behavior every platform had before this package existed. WaitDelayAfterExit
// still applies, so a grandchild holding an output pipe cannot wedge Wait.

func useOwnProcessGroup(cmd *exec.Cmd) {}

func interruptProcessGroup(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}

func killProcessGroup(process *os.Process) error {
	return interruptProcessGroup(process)
}

// processGroupExists reports false because there is no group to escalate to;
// interruptProcessGroup has already killed the direct child.
func processGroupExists(process *os.Process) bool { return false }
