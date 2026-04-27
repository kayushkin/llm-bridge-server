package config

import (
	"os"
	"path/filepath"
	"strings"
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
	NatsURL          string
	ImagesDir        string
	BridgePrefsPath  string
	ConformancePath  string
	LogStoreURL      string
	ToolStoreURL     string
	SnapshotStoreDB  string
	SnapshotStoreGit string
	// SourceFolders maps CreateSessionRequest.Source values to the folder a
	// newly created session should be auto-filed into. Configured via
	// LLMBRIDGE_SOURCE_FOLDERS (format: "source:folder,source:folder"). Any
	// source not in the map results in no auto-filing.
	SourceFolders map[string]string
}

func Load() *Config {
	return &Config{
		ListenAddr:     envOr("LLMBRIDGE_LISTEN_ADDR", ":8160"),
		DBPath:         envOr("LLMBRIDGE_DB_PATH", filepath.Join(os.Getenv("HOME"), ".llm-bridge", "bridge.db")),
		AgentStoreDB:   envOr("LLMBRIDGE_AGENT_DB", filepath.Join(os.Getenv("HOME"), ".config", "agent-store", "agents.db")),
		MemoryStoreDB:  envOr("LLMBRIDGE_MEMORY_DB", filepath.Join(os.Getenv("HOME"), ".config", "memory-store", "memory.db")),
		HarnessStoreDB: envOr("LLMBRIDGE_HARNESS_DB", filepath.Join(os.Getenv("HOME"), ".config", "harness-store", "harness.db")),
		HookStoreDB:    envOr("LLMBRIDGE_HOOK_DB", filepath.Join(os.Getenv("HOME"), ".config", "hook-store", "hooks.db")),
		ModelStoreDB:    envOr("LLMBRIDGE_MODEL_STORE_DB", filepath.Join(os.Getenv("HOME"), ".config", "model-store", "store.db")),
		ModelStoreURL:   os.Getenv("LLMBRIDGE_MODEL_STORE_URL"),
		AgentStoreURL:   os.Getenv("LLMBRIDGE_AGENT_STORE_URL"),
		NatsURL:        os.Getenv("NATS_URL"),
		ImagesDir:       envOr("LLMBRIDGE_IMAGES_DIR", "images"),
		BridgePrefsPath: envOr("LLMBRIDGE_BRIDGE_PREFS", filepath.Join(os.Getenv("HOME"), ".config", "llm-bridge", "bridge-prefs.json")),
		ConformancePath: envOr("LLMBRIDGE_CONFORMANCE_PATH", filepath.Join(os.Getenv("HOME"), ".config", "llm-bridge", "conformance.json")),
		LogStoreURL:     envOr("LLMBRIDGE_LOG_STORE_URL", "http://localhost:8175"),
		ToolStoreURL:    envOr("LLMBRIDGE_TOOL_STORE_URL", "http://localhost:8302"),
		SnapshotStoreDB:  envOr("LLMBRIDGE_SNAPSHOT_DB", filepath.Join(os.Getenv("HOME"), ".config", "snapshot-store", "snapshots.db")),
		SnapshotStoreGit: envOr("LLMBRIDGE_SNAPSHOT_GIT", filepath.Join(os.Getenv("HOME"), ".config", "snapshot-store", "snapshots.git")),
		SourceFolders:   parseSourceFolders(envOr("LLMBRIDGE_SOURCE_FOLDERS", "scheduler:Scheduled,autoworker:Scheduled,healthcheck:Scheduled,renamer:Auto-rename,conformance:Conformance")),
	}
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
