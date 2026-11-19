// llm-bridge-runner-launcher is the program launchd starts on a Mac. It runs
// ~/.local/bin/llm-bridge-runner as its child, passing its arguments through,
// and starts it again whenever it exits, as it does after replacing itself
// with a newer build.
//
// It exists for macOS privacy permissions. Screen Recording and Accessibility
// are granted to the program launchd started, and everything that program
// starts (the runner, Claude Code, the computer-use tool, osascript) acts
// under that grant. macOS identifies an unsigned program by a hash of its
// file, so a grant given to the runner itself was lost at every runner
// update. Granted to this launcher, which does not change, it holds.
//
// So this program must stay small and must not need changing. install.sh
// installs it only when it is missing; replacing it costs the Mac's owner a
// trip through System Settings to grant the permissions again.
package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// A runner that exits sooner than this after starting is failing, not
// updating; the launcher waits longer before the next start.
const (
	quickExit           = 10 * time.Second
	pauseAfterUpdate    = 1 * time.Second
	pauseAfterQuickExit = 10 * time.Second
)

func main() {
	log.SetPrefix("[launcher] ")
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("no home directory: %v", err)
	}
	runner := filepath.Join(home, ".local", "bin", "llm-bridge-runner")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	for {
		started := time.Now()
		cmd := exec.Command(runner, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("cannot start %s: %v; trying again in %s", runner, err, pauseAfterQuickExit)
			select {
			case <-signals:
				return
			case <-time.After(pauseAfterQuickExit):
			}
			continue
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()

		select {
		case sig := <-signals:
			// launchd is stopping the agent: pass it on and wait for the runner.
			cmd.Process.Signal(sig)
			<-exited
			return
		case err := <-exited:
			pause := pauseAfterUpdate
			if time.Since(started) < quickExit {
				pause = pauseAfterQuickExit
			}
			log.Printf("runner exited (%v); starting it again in %s", err, pause)
			select {
			case <-signals:
				return
			case <-time.After(pause):
			}
		}
	}
}
