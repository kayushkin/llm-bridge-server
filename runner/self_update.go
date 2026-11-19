package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// The runner keeps itself current with the build the bridge serves.
//
// Before each connection it asks for its own binary with If-Modified-Since,
// as it does for a wrapper. When the bridge has a newer one the runner
// replaces its executable and stops; its service manager (systemd's
// Restart=always, launchd's KeepAlive) starts the new one. The check runs
// before the connection, when the runner holds no session: it already stops
// every session when a connection ends. Until 2026-10-14 the only way to
// update a runner was to run the installer on its machine again.

// errRunnerReplaced ends Run so the service manager starts the new binary.
var errRunnerReplaced = errors.New("the runner binary was replaced with the bridge's newer build")

// installCurrentRunner replaces this runner's executable with the bridge's
// newer build and reports whether it did. It touches only the copy the
// installer put in ~/.local/bin, never a binary run from anywhere else
// (a development build, say).
func installCurrentRunner(serverURL string) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	installed := filepath.Join(home, ".local", "bin", "llm-bridge-runner")
	if executable != installed {
		return false, nil
	}
	endpoint, err := buildBinaryURLFor(serverURL, "runner", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return false, err
	}
	replaced, err := installCurrentBinary(endpoint, installed)
	if err != nil {
		return false, fmt.Errorf("update %s: %w", installed, err)
	}
	return replaced, nil
}
