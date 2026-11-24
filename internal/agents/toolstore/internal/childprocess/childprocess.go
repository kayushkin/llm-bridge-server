// Package childprocess spawns child processes that can actually be stopped.
//
// A plain exec.CommandContext gives the child the parent's process group and
// cancels it with a kill aimed at the direct child alone. Neither is enough for
// a tool that runs a shell command:
//
//   - The command is `bash -c ...`, so anything it starts is a grandchild.
//     Killing bash leaves those grandchildren running. Measured: a canceled
//     `sleep 60 & sleep 60` leaves the backgrounded sleep alive.
//
//   - Grandchildren inherit the write end of the pipe the parent reads output
//     from. Wait does not return until that pipe is closed, so one backgrounded
//     process holding it blocks Wait forever — after the child is dead, and
//     after the context is canceled. Measured: Wait was still blocked five
//     seconds after cancellation with the direct child gone.
//
// Every command here is therefore its own process group, cancellation signals
// the whole group, and Wait is bounded so a held-open pipe cannot wedge the
// caller.
package childprocess

import (
	"bytes"
	"context"
	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"os/exec"
	"time"
)

// InterruptToKillGrace is how long a canceled process group has to exit after
// SIGINT before it is sent SIGKILL. It is short because the caller has already
// asked for the command to stop; it is not zero because a shell running a build
// should get the chance to leave its own children and temporary files tidy.
const InterruptToKillGrace = 2 * time.Second

// WaitDelayAfterExit bounds how long Wait blocks on output pipes after the
// command has exited or its context is done. It only elapses when a process
// outside the group, or one that survived SIGKILL, is still holding a pipe
// open. When it does elapse the output collected so far is kept and Wait
// returns exec.ErrWaitDelay rather than blocking forever.
const WaitDelayAfterExit = 5 * time.Second

// Command is an exec.Cmd whose process group is stopped as a unit.
//
// The embedded *exec.Cmd is exposed so callers can set Dir, Env and the rest.
// Run and CombinedOutput are redeclared here and must be called on Command, not
// on the embedded Cmd: the embedded methods do not run the escalation to
// SIGKILL, so calling them reintroduces the surviving-grandchild bug.
type Command struct {
	*exec.Cmd

	// ctx is kept because exec.Cmd does not expose the context it was built
	// with, and the escalation has to know when cancellation happened.
	ctx context.Context
}

// NewCommand builds a command that runs in its own process group. Canceling
// ctx sends SIGINT to that whole group, then SIGKILL to the whole group after
// InterruptToKillGrace.
func NewCommand(ctx context.Context, name string, arguments ...string) *Command {
	cmd := exec.CommandContext(ctx, name, arguments...)
	// The tools run inside the bridge server, so a child starts from the
	// server's environment without its secrets; a caller may replace Env.
	cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	useOwnProcessGroup(cmd)
	cmd.WaitDelay = WaitDelayAfterExit
	cmd.Cancel = func() error { return interruptProcessGroup(cmd.Process) }
	return &Command{Cmd: cmd, ctx: ctx}
}

// Run starts the command and waits for it, escalating a canceled context to
// SIGKILL on the whole process group.
func (c *Command) Run() error {
	if err := c.Cmd.Start(); err != nil {
		return err
	}
	return c.wait()
}

// CombinedOutput runs the command and returns its interleaved stdout and
// stderr. Output written before a cancellation is kept: the buffer the child
// wrote into is the buffer returned, so stopping the command cannot edit what
// the caller already produced.
func (c *Command) CombinedOutput() ([]byte, error) {
	var output bytes.Buffer
	c.Cmd.Stdout = &output
	c.Cmd.Stderr = &output
	err := c.Run()
	return output.Bytes(), err
}

// Output runs the command and returns its stdout, leaving stderr to whatever
// the caller assigned.
func (c *Command) Output() ([]byte, error) {
	var stdout bytes.Buffer
	c.Cmd.Stdout = &stdout
	err := c.Run()
	return stdout.Bytes(), err
}

func (c *Command) wait() error {
	awaitEscalation := c.killProcessGroupAfterGrace()
	err := c.Cmd.Wait()
	awaitEscalation()
	return err
}

// killProcessGroupAfterGrace sends SIGKILL to the process group if the context
// is canceled and the group is still running InterruptToKillGrace later.
//
// The escalation deliberately outlives the direct child. A shell exits on
// SIGINT while the jobs it backgrounded do not — a non-interactive bash sets
// SIGINT to ignored in a background job, so `sleep 60 &` survives the signal
// that stops its own shell. Disarming the escalation when Wait returns leaves
// exactly those processes running, which is the leak this package exists to
// close.
//
// The returned function blocks until the escalation has been delivered or is no
// longer needed. Call it after Wait, so a canceled command has nothing of its
// own left running by the time Run returns.
func (c *Command) killProcessGroupAfterGrace() (awaitEscalation func()) {
	commandReturned := make(chan struct{})
	escalationFinished := make(chan struct{})

	go func() {
		defer close(escalationFinished)

		select {
		case <-c.ctx.Done():
		case <-commandReturned:
			// Never canceled: the group stopped on its own terms.
			return
		}

		// SIGINT has been sent to the group by Cancel. Give it the grace
		// period, but stop waiting as soon as the group is empty so an
		// ordinary cancellation is not padded out to the full period.
		graceExpired := time.After(InterruptToKillGrace)
		poll := time.NewTicker(50 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case <-graceExpired:
				killProcessGroup(c.Cmd.Process)
				return
			case <-poll.C:
				if !processGroupExists(c.Cmd.Process) {
					return
				}
			}
		}
	}()

	return func() {
		close(commandReturned)
		<-escalationFinished
	}
}
