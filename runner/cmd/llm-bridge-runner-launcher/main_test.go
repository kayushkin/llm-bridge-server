package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The stand-in runner logs each start and its arguments; the first one exits
// at once, as a runner that replaced itself does, and the second one waits.
const fakeRunner = `#!/bin/sh
echo "start $*" >> "$HOME/starts"
[ "$(wc -l < "$HOME/starts")" -ge 2 ] || exit 0
trap 'echo "stopped" >> "$HOME/starts"; exit 0' TERM
while :; do sleep 0.1; done
`

func TestLauncherRestartsTheRunnerAndPassesStopOn(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "bin", "llm-bridge-runner"), []byte(fakeRunner), 0755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "launcher")
	if out, err := exec.Command("go", "build", "-o", launcher, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(launcher, "run")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(home, "starts")
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, _ := os.ReadFile(starts)
		if strings.Count(string(got), "start run") == 2 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the runner was not started twice: %q", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("launcher exit: %v", err)
	}
	if got, _ := os.ReadFile(starts); !strings.HasSuffix(string(got), "stopped\n") {
		t.Errorf("the stop never reached the runner: %q", got)
	}
}
