package executor

import (
	"bytes"
	"context"
	"errors"
	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"os/exec"
	"syscall"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
)

// DefaultTimeout applies to any job that has not set its own timeout.
const DefaultTimeout = 5 * time.Minute

// waitDelay bounds how long Run waits for the output pipes to drain after the
// child has been killed. A grandchild that outlives its parent inherits the
// write end of those pipes, and cmd.Wait blocks until every writer closes them
// — which is why the old 5-minute context timeout never actually bounded a run.
const waitDelay = 10 * time.Second

type Result struct {
	Status string
	Output string
}

func Run(command string, timeout time.Duration) Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// A job runs without the server's secrets in its environment.
	cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	// Put the child in its own process group so we can signal the whole tree.
	// Killing only the direct child leaves grandchildren alive, holding the
	// pipes open and stranding the run as "running" forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return Result{Status: runstatus.Error, Output: err.Error()}
	}
	// The child is the group leader, so its pid is the pgid. Sweep the group
	// once the run is over, however it ended: a grandchild that outlives a
	// normally-exited parent is never signalled by Cancel (there is no live
	// leader left to cancel), and would otherwise be orphaned to PID 1 — the
	// leak that had claude processes accumulating on this host.
	pgid := cmd.Process.Pid
	defer killGroup(pgid)

	err := cmd.Wait()
	output := buf.String()

	if err != nil {
		// ctx.Err() is the reliable signal here: on timeout the child is killed,
		// so cmd.Run reports a signal error rather than anything timeout-shaped.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{Status: runstatus.Timeout, Output: output + "\ntimed out after " + timeout.String()}
		}
		return Result{Status: runstatus.Error, Output: output + err.Error()}
	}
	return Result{Status: runstatus.Success, Output: output}
}

// killGroup SIGKILLs an entire process group. ESRCH just means the group is
// already gone, which is the normal case for a job that exited cleanly.
func killGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
