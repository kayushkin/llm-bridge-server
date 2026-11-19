package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
)

// SavedConfig is the on-disk runner state — written by `enroll` and read
// by every subsequent runner start. The runner_token is the long-lived
// per-machine bearer used in WS Authorization.
//
// Endpoint preserves the original install-time bridge spec (URL or
// user@host SSH target) so the installer can re-derive the correct
// systemd unit shape on update without re-prompting the user.
type SavedConfig struct {
	ServerURL   string `json:"server_url"`
	Endpoint    string `json:"endpoint,omitempty"`
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
	RunnerToken string `json:"runner_token"` // sensitive; file mode 0600
}

// Config is the runtime configuration assembled from SavedConfig + live
// environment readings (hostname/user/etc.).
type Config struct {
	ServerURL   string
	MachineID   string
	MachineName string
	RunnerToken string
	WorkingDir  string
	Hostname    string
	OS          string
	Arch        string
	User        string
}

// configPath resolves the path to the saved config file. Honors
// $XDG_CONFIG_HOME and $LLMBRIDGE_RUNNER_CONFIG.
func configPath() string {
	if p := os.Getenv("LLMBRIDGE_RUNNER_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if u, err := user.Current(); err == nil {
			base = filepath.Join(u.HomeDir, ".config")
		} else {
			base = filepath.Join(os.Getenv("HOME"), ".config")
		}
	}
	return filepath.Join(base, "llm-bridge-runner", "config.json")
}

// LoadSavedConfig reads the on-disk SavedConfig. Returns the zero value
// and a not-exist error when the file is absent (so callers can detect
// "not yet enrolled" cleanly).
func LoadSavedConfig() (*SavedConfig, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg SavedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// SaveConfig writes a SavedConfig to disk with mode 0600. Creates the
// parent directory if needed. The file contains a bearer token; mode
// matters.
func SaveConfig(cfg *SavedConfig) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// LoadRuntimeConfig combines SavedConfig with live process state for
// passing to the WS client. Returns an error if the saved config is
// missing/invalid (i.e. the runner has not been enrolled yet).
func LoadRuntimeConfig() (*Config, error) {
	saved, err := LoadSavedConfig()
	if err != nil {
		return nil, fmt.Errorf("read runner config: %w (run `llm-bridge-runner enroll` first)", err)
	}
	if saved.ServerURL == "" || saved.RunnerToken == "" || saved.MachineName == "" {
		return nil, fmt.Errorf("runner config at %s is incomplete (server_url / runner_token / machine_name missing)", configPath())
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user: %w", err)
	}
	workdir := os.Getenv("LLMBRIDGE_WORKING_DIR")
	if workdir == "" {
		workdir = u.HomeDir
	}

	return &Config{
		ServerURL:   saved.ServerURL,
		MachineID:   saved.MachineID,
		MachineName: saved.MachineName,
		RunnerToken: saved.RunnerToken,
		WorkingDir:  workdir,
		Hostname:    hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		User:        u.Username,
	}, nil
}
