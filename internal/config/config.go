package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	ListenAddr     string
	DBPath         string
	AgentStoreDB   string
	MemoryStoreDB  string
	HarnessStoreDB string
	ModelStoreURL  string
	AgentStoreURL  string
	NatsURL        string
	ImagesDir      string
}

func Load() *Config {
	return &Config{
		ListenAddr:     envOr("LLMBRIDGE_LISTEN_ADDR", ":8160"),
		DBPath:         envOr("LLMBRIDGE_DB_PATH", filepath.Join(os.Getenv("HOME"), ".llm-bridge", "bridge.db")),
		AgentStoreDB:   envOr("LLMBRIDGE_AGENT_DB", filepath.Join(os.Getenv("HOME"), ".config", "agent-store", "agents.db")),
		MemoryStoreDB:  envOr("LLMBRIDGE_MEMORY_DB", filepath.Join(os.Getenv("HOME"), ".config", "memory-store", "memory.db")),
		HarnessStoreDB: envOr("LLMBRIDGE_HARNESS_DB", filepath.Join(os.Getenv("HOME"), ".config", "harness-store", "harness.db")),
		ModelStoreURL:  os.Getenv("LLMBRIDGE_MODEL_STORE_URL"),
		AgentStoreURL:  os.Getenv("LLMBRIDGE_AGENT_STORE_URL"),
		NatsURL:        os.Getenv("NATS_URL"),
		ImagesDir:      envOr("LLMBRIDGE_IMAGES_DIR", "images"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
