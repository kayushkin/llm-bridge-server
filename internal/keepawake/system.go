package keepawake

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
)

// releaseWait is how long a released child gets to end on its own, after its
// standard input closes, before it is killed.
const releaseWait = 5 * time.Second

// childSettleWait is how long a new child must stay up before its block is
// taken as in force: a child that cannot hold one (no logind session to ask,
// no powershell.exe) exits at once, and that is reported, not treated as held.
const childSettleWait = 500 * time.Millisecond

// SystemInhibitor returns the inhibitor for the machine this process runs on:
// caffeinate on macOS, a powershell.exe child under WSL, and systemd-inhibit on
// any other Linux. Any other system has none.
func SystemInhibitor() (Inhibitor, error) {
	switch runtime.GOOS {
	case "darwin":
		return commandInhibitor{description: "caffeinate -i", killedToRelease: true, command: func(string) []string {
			// -w ends caffeinate when this server ends, however it ends. It
			// does not read its standard input, so a release kills it.
			return []string{"caffeinate", "-i", "-w", strconv.Itoa(os.Getpid())}
		}}, nil
	case "linux":
		if runningUnderWSL() {
			return commandInhibitor{description: "SetThreadExecutionState in powershell.exe", command: func(string) []string {
				return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsExecutionStateScript}
			}}, nil
		}
		return commandInhibitor{description: "systemd-inhibit --what=sleep:idle", command: func(reason string) []string {
			// cat holds the block until its standard input closes: when this
			// server releases it, or when this server dies.
			return []string{"systemd-inhibit", "--what=sleep:idle", "--who=llm-bridge-server", "--why=" + reason, "--mode=block", "cat"}
		}}, nil
	default:
		return nil, fmt.Errorf("keep-awake has no way to hold %s awake", runtime.GOOS)
	}
}

// windowsExecutionStateScript asks Windows to keep the system awake for as
// long as this PowerShell's thread lives, and lives until its standard input
// closes. ES_CONTINUOUS | ES_SYSTEM_REQUIRED, without ES_DISPLAY_REQUIRED: the
// screen may still turn off.
const windowsExecutionStateScript = `$ErrorActionPreference = 'Stop'
Add-Type -Namespace LlmBridge -Name Power -MemberDefinition '[DllImport("kernel32.dll")] public static extern uint SetThreadExecutionState(uint esFlags);'
if ([LlmBridge.Power]::SetThreadExecutionState([uint32]2147483649) -eq 0) { throw 'SetThreadExecutionState refused' }
[Console]::In.ReadToEnd() | Out-Null`

// runningUnderWSL reads the kernel's release string, which names Microsoft on
// every WSL kernel.
func runningUnderWSL() bool {
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft")
}

// commandInhibitor holds a block by keeping one child process alive.
type commandInhibitor struct {
	description string
	command     func(reason string) []string
	// killedToRelease is set for a child that does not end when its standard
	// input closes.
	killedToRelease bool
}

func (inhibitor commandInhibitor) Describe() string { return inhibitor.description }

func (inhibitor commandInhibitor) Hold(reason string) (Hold, error) {
	arguments := inhibitor.command(reason)
	child := exec.Command(arguments[0], arguments[1:]...)
	child.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	standardInput, err := child.StdinPipe()
	if err != nil {
		return nil, err
	}
	var standardError strings.Builder
	child.Stderr = &limitedWriter{builder: &standardError, remaining: 4096}
	if err := child.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", arguments[0], err)
	}
	ended := make(chan error, 1)
	go func() { ended <- child.Wait() }()
	select {
	case err := <-ended:
		standardInput.Close()
		return nil, fmt.Errorf("%s ended at once (%v): %s", arguments[0], err, strings.TrimSpace(standardError.String()))
	case <-time.After(childSettleWait):
	}
	return &childHold{name: arguments[0], child: child, standardInput: standardInput, ended: ended, killedToRelease: inhibitor.killedToRelease}, nil
}

// childHold is one running block-holding child.
type childHold struct {
	name          string
	child         *exec.Cmd
	standardInput io.WriteCloser
	ended         chan error
	// killedToRelease is set for a child that does not end when its standard
	// input closes.
	killedToRelease bool
}

// Release closes the child's standard input, which ends every child this
// package starts, and kills it if it has not ended within releaseWait.
func (hold *childHold) Release() error {
	hold.standardInput.Close()
	if !hold.killedToRelease {
		select {
		case <-hold.ended:
			return nil
		case <-time.After(releaseWait):
		}
	}
	if err := hold.child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill %s (pid %d): %w", hold.name, hold.child.Process.Pid, err)
	}
	<-hold.ended
	return nil
}

// limitedWriter keeps the first bytes a child writes to standard error, for
// the message when it fails.
type limitedWriter struct {
	builder   *strings.Builder
	remaining int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if writer.remaining > 0 {
		kept := data
		if len(kept) > writer.remaining {
			kept = kept[:writer.remaining]
		}
		writer.builder.Write(kept)
		writer.remaining -= len(kept)
	}
	return len(data), nil
}
