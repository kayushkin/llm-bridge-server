package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The computer-use tool a runner can hand to a session.
//
// In WSL the tool is the Windows build of llm-bridge-computer-use: WSL cannot
// see the Windows desktop, but Claude Code inside WSL can start a Windows
// program through interop, and that program runs in the Windows session WSL
// was started from (tested on SSDawn, 2026-10-14). The runner keeps it under
// the Windows user's local app data, current with the bridge's build, and
// reports its path in the Hello; the bridge offers it to a session only where
// the machine's owner has switched computer use on.
//
// On macOS the tool is the darwin build, in ~/.local/bin beside the runner.
// It drives the screen through screencapture and osascript, so the runner's
// launchd agent needs Screen Recording and Accessibility, granted once on the
// Mac (tested on MacBook, 2026-10-14; see the README).
//
// Other systems report no tool.

const computerUseToolName = "computer-use" // its /api/runner/binary name

// installCurrentComputerUseTool installs or updates this machine's
// computer-use tool and describes it, or returns nil with no error on a
// machine that has none.
func installCurrentComputerUseTool(serverURL string) (*msg.ComputerUseTool, error) {
	command, goos, goarch, err := computerUseToolLocation()
	if err != nil || command == "" {
		return nil, err
	}
	endpoint, err := buildBinaryURLFor(serverURL, computerUseToolName, goos, goarch)
	if err != nil {
		return nil, err
	}
	if _, err := installCurrentBinary(endpoint, command); err != nil {
		return nil, fmt.Errorf("install the computer-use tool at %s: %w", command, err)
	}
	version, err := computerUseToolVersion(command)
	if err != nil {
		return nil, err
	}
	return &msg.ComputerUseTool{Command: command, Version: version}, nil
}

// computerUseToolLocation is where this machine keeps its computer-use tool
// and which build it is, or "" on a machine that has none.
func computerUseToolLocation() (command, goos, goarch string, err error) {
	switch {
	case runtime.GOOS == "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", "", err
		}
		return filepath.Join(home, ".local", "bin", "llm-bridge-computer-use"), "darwin", runtime.GOARCH, nil
	case runningInWSLWithInterop():
		localAppData, err := windowsLocalAppDataInWSL()
		if err != nil {
			return "", "", "", err
		}
		return filepath.Join(localAppData, "llm-bridge", "llm-bridge-computer-use.exe"), "windows", "amd64", nil
	}
	return "", "", "", nil
}

// runningInWSLWithInterop reports whether this is WSL and Windows programs can
// be started from it.
func runningInWSLWithInterop() bool {
	version, err := os.ReadFile("/proc/version")
	if err != nil || !strings.Contains(strings.ToLower(string(version)), "microsoft") {
		return false
	}
	_, err = os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

// windowsLocalAppDataInWSL is %LOCALAPPDATA% of the Windows user, as a WSL path.
func windowsLocalAppDataInWSL() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe", "/c", "echo %LOCALAPPDATA%")
	cmd.Dir = "/mnt/c" // cmd.exe complains when started in a Linux directory
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ask Windows for %%LOCALAPPDATA%%: %w", err)
	}
	windowsPath := strings.TrimSpace(string(out))
	if windowsPath == "" || strings.Contains(windowsPath, "%") {
		return "", fmt.Errorf("Windows gave no %%LOCALAPPDATA%% (%q)", windowsPath)
	}
	out, err = exec.CommandContext(ctx, "wslpath", "-u", windowsPath).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath -u %q: %w", windowsPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func computerUseToolVersion(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, "-version").Output()
	if err != nil {
		return "", fmt.Errorf("%s -version: %w", command, err)
	}
	return strings.TrimSpace(string(out)), nil
}
