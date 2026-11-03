//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func shellInput(t *testing.T, command string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestShellReturnsCommandOutput(t *testing.T) {
	out, err := Shell().Run(context.Background(), shellInput(t, "echo hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("want the command output, got %q", out)
	}
}

// The whole point of threading a context into the shell tool: an interrupt has
// to stop what the command started, not just the shell that started it.
func TestShellCancelStopsWhatTheCommandStarted(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		_, _ = Shell().Run(ctx, shellInput(t, "sleep 60 & echo $! >"+pidFile+"; sleep 60"))
		close(returned)
	}()

	grandchild := readPidWhenWritten(t, pidFile)
	cancel()

	select {
	case <-returned:
	case <-time.After(20 * time.Second):
		t.Fatal("the shell tool never returned after its context was canceled")
	}
	if processIsAlive(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("process %d started by the command outlived the canceled tool call", grandchild)
	}
}

// A command that daemonizes something and exits used to hang the tool call
// forever, because the daemon inherited the pipe the tool reads output from.
func TestShellReturnsWhenACommandLeavesSomethingRunning(t *testing.T) {
	returned := make(chan string, 1)
	go func() {
		out, _ := Shell().Run(context.Background(), shellInput(t, "echo done; sleep 60 &"))
		returned <- out
	}()

	select {
	case out := <-returned:
		if !strings.Contains(out, "done") {
			t.Fatalf("output produced before the wait was bounded got lost, got %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the shell tool never returned; a backgrounded process held its output pipe")
	}
}

func readPidWhenWritten(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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

func processIsAlive(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == nil
}

// Canceling a sequence stops the sequence. Without the check the loop would
// keep starting commands against a dead context and report each as a failure,
// which reads as "we tried and they broke" rather than "we stopped".
func TestShellDoesNotRunTheRestOfASequenceAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	marker := filepath.Join(t.TempDir(), "second-ran")
	raw, err := json.Marshal(map[string]any{
		"commands": []string{"sleep 60", "touch " + marker},
	})
	if err != nil {
		t.Fatal(err)
	}

	returned := make(chan string, 1)
	go func() {
		out, _ := Shell().Run(ctx, string(raw))
		returned <- out
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	var out string
	select {
	case out = <-returned:
	case <-time.After(20 * time.Second):
		t.Fatal("the shell tool never returned after its context was canceled")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command after the canceled one was run anyway")
	}
	if !strings.Contains(out, "1 of 2 commands not run") {
		t.Fatalf("the result does not say what was skipped, got %q", out)
	}
}

// A process that serves many sessions gives each session's commands that
// session's values, read at every command so a refreshed one applies to the
// next command, and they win over the process's own.
func TestShellWithSessionEnvironmentGivesEachCommandTheSessionsValues(t *testing.T) {
	t.Setenv("LLM_BRIDGE_SESSION_POSTING_TOKEN", "the-process-own")
	token := "spawn-1"
	shell := ShellWithSessionEnvironment(func() []string {
		return []string{"LLM_BRIDGE_SESSION_POSTING_TOKEN=" + token}
	})
	run := func() string {
		out, err := shell.Run(context.Background(), `{"command":"printf %s \"$LLM_BRIDGE_SESSION_POSTING_TOKEN\""}`)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := run(); got != "spawn-1" {
		t.Fatalf("command saw %q, want the session's spawn-1", got)
	}
	token = "spawn-2"
	if got := run(); got != "spawn-2" {
		t.Fatalf("after a refresh the command saw %q, want spawn-2", got)
	}
	if got, _ := Shell().Run(context.Background(), `{"command":"printf %s \"$LLM_BRIDGE_SESSION_POSTING_TOKEN\""}`); got != "the-process-own" {
		t.Fatalf("plain Shell saw %q, want the process's own value", got)
	}
}
