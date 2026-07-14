package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	DBPath          string
	AgentStoreDB    string
	MemoryStoreDB   string
	HarnessStoreDB  string
	HookStoreDB     string
	ModelStoreDB    string
	ModelStoreURL   string
	AgentStoreURL   string
	ImagesDir       string
	BridgePrefsPath string
	ConformancePath string
	LogStoreURL     string
	// PublicURL is the externally-reachable bridge URL that runners use
	// to fetch backend binaries listed in HarnessService.BinaryURL. Empty
	// → manifests fall back to the runner's own server_url, which works
	// when the runner is reaching the bridge over a tunnel on
	// localhost:port (the WSL-via-SSH-tunnel case).
	PublicURL    string
	ToolStoreURL string
	// PermissionStoreURL is the base URL of the permission-store service
	// consulted by the PreToolUse permission-prehook handler. Defaults to
	// localhost:8304.
	PermissionStoreURL string
	SnapshotStoreDB    string
	SnapshotStoreGit   string
	// SourceFolders maps CreateSessionRequest.Purpose values to the folder a
	// newly created session should be auto-filed into. Configured via
	// LLMBRIDGE_SOURCE_FOLDERS (format: "source:folder,source:folder"). Any
	// source not in the map results in no auto-filing.
	SourceFolders map[string]string
	// PTYRingBufferBytes is the per-session ring buffer size (in bytes)
	// of recent pty output. Late attachers receive a replay of this
	// buffer on connect so xterm.js can paint the current screen state
	// without a full clear-and-redraw. Configured via
	// LLMBRIDGE_PTY_RING_BUFFER_BYTES; defaults to 65536 (64 KiB).
	PTYRingBufferBytes int
	// UnattendedIdleTimeout is how long a session with no human watching it
	// may sit with no new events — stream output OR telemetry, both land in
	// the events table — before the watchdog kills its harness process and
	// marks it aborted. Reaping reclaims the ~300MB a warm claude subprocess
	// holds while it waits on stdin for a follow-up turn that one-shot
	// autoworkers never send. Configured via LLMBRIDGE_UNATTENDED_IDLE_TIMEOUT
	// (Go duration, e.g. "15m"); <=0 disables reaping for unattended sessions.
	UnattendedIdleTimeout time.Duration
	// AttendedIdleTimeout is the same cutoff for sessions a human is sitting
	// in front of. It defaults much higher because a human is a legitimate
	// source of silence: someone reading output, thinking, or away from the
	// keyboard emits no events, and killing their warm process throws away
	// the context their next message was going to land in.
	//
	// "Attended" is a property of who is watching, not of the I/O contract:
	// it covers pty sessions (whose state never leaves "running", so the
	// activity timestamp is the sole liveness signal) AND events-mode
	// SessionTypeInteractive sessions like the dash chat. Reaping the latter
	// on the unattended timeout is what made an idle browser tab lose its
	// session. Configured via LLMBRIDGE_ATTENDED_IDLE_TIMEOUT; <=0 disables
	// reaping for attended sessions.
	AttendedIdleTimeout time.Duration
}

func Load() *Config {
	return &Config{
		ListenAddr:            envOr("LLMBRIDGE_LISTEN_ADDR", ":8160"),
		DBPath:                envOr("LLMBRIDGE_DB_PATH", filepath.Join(os.Getenv("HOME"), ".llm-bridge", "bridge.db")),
		AgentStoreDB:          envOr("LLMBRIDGE_AGENT_DB", filepath.Join(os.Getenv("HOME"), ".config", "agent-store", "agents.db")),
		MemoryStoreDB:         envOr("LLMBRIDGE_MEMORY_DB", filepath.Join(os.Getenv("HOME"), ".config", "memory-store", "memory.db")),
		HarnessStoreDB:        envOr("LLMBRIDGE_HARNESS_DB", filepath.Join(os.Getenv("HOME"), ".config", "harness-store", "harness.db")),
		HookStoreDB:           envOr("LLMBRIDGE_HOOK_DB", filepath.Join(os.Getenv("HOME"), ".config", "hook-store", "hooks.db")),
		ModelStoreDB:          envOr("LLMBRIDGE_MODEL_STORE_DB", filepath.Join(os.Getenv("HOME"), ".config", "model-store", "store.db")),
		ModelStoreURL:         os.Getenv("LLMBRIDGE_MODEL_STORE_URL"),
		AgentStoreURL:         os.Getenv("LLMBRIDGE_AGENT_STORE_URL"),
		ImagesDir:             envOr("LLMBRIDGE_IMAGES_DIR", "images"),
		BridgePrefsPath:       envOr("LLMBRIDGE_BRIDGE_PREFS", filepath.Join(os.Getenv("HOME"), ".config", "llm-bridge", "bridge-prefs.json")),
		ConformancePath:       envOr("LLMBRIDGE_CONFORMANCE_PATH", filepath.Join(os.Getenv("HOME"), ".config", "llm-bridge", "conformance.json")),
		LogStoreURL:           envOr("LLMBRIDGE_LOG_STORE_URL", "http://localhost:8175"),
		PublicURL:             os.Getenv("LLMBRIDGE_PUBLIC_URL"),
		ToolStoreURL:          envOr("LLMBRIDGE_TOOL_STORE_URL", "http://localhost:8302"),
		PermissionStoreURL:    envOr("LLMBRIDGE_PERMISSION_STORE_URL", "http://localhost:8304"),
		SnapshotStoreDB:       envOr("LLMBRIDGE_SNAPSHOT_DB", filepath.Join(os.Getenv("HOME"), ".config", "snapshot-store", "snapshots.db")),
		SnapshotStoreGit:      envOr("LLMBRIDGE_SNAPSHOT_GIT", filepath.Join(os.Getenv("HOME"), ".config", "snapshot-store", "snapshots.git")),
		SourceFolders:         parseSourceFolders(envOr("LLMBRIDGE_SOURCE_FOLDERS", "scheduler:Scheduled,autoworker:Scheduled,harness-watch:Scheduled,healthcheck:Scheduled,renamer:Auto-rename,conformance:Conformance,subagent:Subagents,workflow-subagent:Subagents")),
		PTYRingBufferBytes:    envInt("LLMBRIDGE_PTY_RING_BUFFER_BYTES", 64*1024),
		UnattendedIdleTimeout: envDuration("LLMBRIDGE_UNATTENDED_IDLE_TIMEOUT", 15*time.Minute),
		AttendedIdleTimeout:   envDuration("LLMBRIDGE_ATTENDED_IDLE_TIMEOUT", 60*time.Minute),
	}
}

// envDuration reads a Go duration string (e.g. "15m") from an env var,
// falling back to def if unset or unparseable. A parsed zero or negative
// duration is preserved — callers treat <=0 as "disabled".
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envInt reads an int from an env var, falling back to def if unset or
// unparseable.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// parseSourceFolders parses "source:folder,source:folder" into a map.
// Malformed pairs are skipped. Whitespace around keys and values is trimmed.
func parseSourceFolders(spec string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, ":")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
