package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge/msg"
)

type Config struct {
	ListenAddr       string
	DBPath           string
	AgentStoreDB     string
	MemoryStoreDB    string
	HarnessStoreDB   string
	HookStoreDB      string
	ModelStoreDB     string
	ModelStoreURL    string
	AgentStoreURL    string
	ImagesDir        string
	BridgePrefsPath  string
	ConformancePath  string
	LogStoreURL      string
	// PublicURL is the externally-reachable bridge URL that runners use
	// to fetch backend binaries listed in HarnessService.BinaryURL. Empty
	// → manifests fall back to the runner's own server_url, which works
	// when the runner is reaching the bridge over a tunnel on
	// localhost:port (the WSL-via-SSH-tunnel case).
	PublicURL        string
	ToolStoreURL     string
	// PermissionStoreURL is the base URL of the permission-store service
	// consulted by the PreToolUse permission-prehook handler. Defaults to
	// localhost:8304.
	PermissionStoreURL string
	SnapshotStoreDB  string
	SnapshotStoreGit string
	// PurposeFolders maps CreateSessionRequest.Purpose values to the folder a
	// newly created session should be auto-filed into. Defaults come from the
	// purpose registry (msg.KnownPurposes); LLMBRIDGE_PURPOSE_FOLDERS overlays
	// them (format: "purpose:folder,purpose:folder"). Any purpose not in the
	// map results in no auto-filing.
	PurposeFolders map[string]string
	// PTYRingBufferBytes is the per-session ring buffer size (in bytes)
	// of recent pty output. Late attachers receive a replay of this
	// buffer on connect so xterm.js can paint the current screen state
	// without a full clear-and-redraw. Configured via
	// LLMBRIDGE_PTY_RING_BUFFER_BYTES; defaults to 65536 (64 KiB).
	PTYRingBufferBytes int
	// IdleTimeout is how long an events-mode session may sit with no new
	// events — stream output OR telemetry, both land in the events table —
	// before the watchdog kills its harness process and marks it aborted.
	// Reaping reclaims the ~150MB a warm claude subprocess holds while it
	// waits on stdin for a follow-up turn that one-shot autoworkers never
	// send. Configured via LLMBRIDGE_IDLE_TIMEOUT (Go duration, e.g.
	// "15m"); <=0 disables reaping for events-mode sessions.
	IdleTimeout time.Duration
	// PTYIdleTimeout is the same cutoff for pty-mode (interactive)
	// sessions. PTY session state is not derived from telemetry (it stays
	// "running"), so the activity timestamp is the sole liveness signal —
	// and a human reading output between prompts emits nothing — so this
	// defaults much higher than IdleTimeout. Configured via
	// LLMBRIDGE_PTY_IDLE_TIMEOUT; <=0 disables reaping for pty sessions.
	PTYIdleTimeout time.Duration
}

// Load reads the process environment, falling back to the addresses in
// internal/productiondefaults for anything unset.
//
// Those fallbacks are what make the ordinary case work with no configuration,
// and they are also what would let a test open the live databases and write to
// the live event log. Load therefore ends by asking productiondefaults whether
// any of them survived, and panics if one did inside a `go test` binary. The
// check is inert in a real gateway process.
func Load() *Config {
	cfg := &Config{
		ListenAddr:     envOr("LLMBRIDGE_LISTEN_ADDR", productiondefaults.ListenAddr),
		DBPath:         envOr("LLMBRIDGE_DB_PATH", productiondefaults.BridgeDatabasePath()),
		AgentStoreDB:   envOr("LLMBRIDGE_AGENT_DB", productiondefaults.AgentStoreDatabasePath()),
		MemoryStoreDB:  envOr("LLMBRIDGE_MEMORY_DB", productiondefaults.MemoryStoreDatabasePath()),
		HarnessStoreDB: envOr("LLMBRIDGE_HARNESS_DB", productiondefaults.HarnessStoreDatabasePath()),
		HookStoreDB:    envOr("LLMBRIDGE_HOOK_DB", productiondefaults.HookStoreDatabasePath()),
		ModelStoreDB:    envOr("LLMBRIDGE_MODEL_STORE_DB", productiondefaults.ModelStoreDatabasePath()),
		ModelStoreURL:   os.Getenv("LLMBRIDGE_MODEL_STORE_URL"),
		AgentStoreURL:   os.Getenv("LLMBRIDGE_AGENT_STORE_URL"),
		ImagesDir:       envOr("LLMBRIDGE_IMAGES_DIR", "images"),
		BridgePrefsPath: envOr("LLMBRIDGE_BRIDGE_PREFS", productiondefaults.BridgePreferencesPath()),
		ConformancePath: envOr("LLMBRIDGE_CONFORMANCE_PATH", productiondefaults.ConformancePath()),
		LogStoreURL:     envOr("LLMBRIDGE_LOG_STORE_URL", productiondefaults.LogStoreURL),
		PublicURL:       os.Getenv("LLMBRIDGE_PUBLIC_URL"),
		ToolStoreURL:    envOr("LLMBRIDGE_TOOL_STORE_URL", productiondefaults.ToolStoreURL),
		PermissionStoreURL: envOr("LLMBRIDGE_PERMISSION_STORE_URL", productiondefaults.PermissionStoreURL),
		SnapshotStoreDB:  envOr("LLMBRIDGE_SNAPSHOT_DB", productiondefaults.SnapshotStoreDatabasePath()),
		SnapshotStoreGit: envOr("LLMBRIDGE_SNAPSHOT_GIT", productiondefaults.SnapshotStoreGitPath()),
		PurposeFolders:  parsePurposeFolders(os.Getenv("LLMBRIDGE_PURPOSE_FOLDERS")),
		PTYRingBufferBytes: envInt("LLMBRIDGE_PTY_RING_BUFFER_BYTES", 64*1024),
		IdleTimeout:        envDuration("LLMBRIDGE_IDLE_TIMEOUT", 15*time.Minute),
		PTYIdleTimeout:     envDuration("LLMBRIDGE_PTY_IDLE_TIMEOUT", 60*time.Minute),
	}
	productiondefaults.PanicIfUsedUnderTest(cfg.GuardedAddresses())
	return cfg
}

// GuardedAddresses reports the value this config holds for every field
// internal/productiondefaults knows a production address for, keyed by field
// name. Callers hand it to productiondefaults.PanicIfUsedUnderTest.
//
// A Config built as a literal — which is how every test in this repo builds
// one — never passes through Load, so this is also the way such a test can opt
// itself into the same check.
func (c *Config) GuardedAddresses() map[string]string {
	return map[string]string{
		"ListenAddr":         c.ListenAddr,
		"DBPath":             c.DBPath,
		"AgentStoreDB":       c.AgentStoreDB,
		"MemoryStoreDB":      c.MemoryStoreDB,
		"HarnessStoreDB":     c.HarnessStoreDB,
		"HookStoreDB":        c.HookStoreDB,
		"ModelStoreDB":       c.ModelStoreDB,
		"BridgePrefsPath":    c.BridgePrefsPath,
		"ConformancePath":    c.ConformancePath,
		"LogStoreURL":        c.LogStoreURL,
		"ToolStoreURL":       c.ToolStoreURL,
		"PermissionStoreURL": c.PermissionStoreURL,
		"SnapshotStoreDB":    c.SnapshotStoreDB,
		"SnapshotStoreGit":   c.SnapshotStoreGit,
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

// purposeFolderDefaults returns the purpose→folder map built from the purpose
// registry in llm-bridge/msg.
//
// This used to be a hardcoded default string listing eight purposes, which is
// how it drifted: `scheduled-task`, `dispatcher` and `herald` were all live
// purposes that never appeared in it, so those sessions silently never filed,
// while its `healthcheck` key mapped a purpose no caller ever sent. Two places
// held the vocabulary and only one of them was maintained.
//
// Reading it off the registry means adding a purpose files it by construction.
func purposeFolderDefaults() map[string]string {
	out := make(map[string]string)
	for _, p := range msg.KnownPurposes() {
		if p.Folder != "" {
			out[p.Name] = p.Folder
		}
	}
	return out
}

// parsePurposeFolders parses "purpose:folder,purpose:folder" into a map,
// overlaid on the registry defaults. Malformed pairs are skipped. Whitespace
// around keys and values is trimmed. An empty spec leaves the defaults alone.
func parsePurposeFolders(spec string) map[string]string {
	out := purposeFolderDefaults()
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
