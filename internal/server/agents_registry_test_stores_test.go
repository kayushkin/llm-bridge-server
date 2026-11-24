package server

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/agents/agentstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/memorystore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// openTestAgentStore gives the test an empty agent-store on a Postgres
// database of its own, whose home directory is a temporary folder: nothing a
// test scans, renders or writes can reach this machine's real home, repos or
// prompt files.
func openTestAgentStore(t *testing.T) *agentstore.Store {
	t.Helper()
	module, err := agentstore.Module()
	if err != nil {
		t.Fatalf("agent-store module: %v", err)
	}
	store, err := agentstore.New(postgrestest.NewDatabase(t, module).Open(module), t.TempDir())
	if err != nil {
		t.Fatalf("agent-store: %v", err)
	}
	return store
}

// openTestMemoryStore gives the test an empty memory-store on a Postgres
// database of its own.
func openTestMemoryStore(t *testing.T) *memorystore.Store {
	t.Helper()
	module, err := memorystore.Module()
	if err != nil {
		t.Fatalf("memory-store module: %v", err)
	}
	return memorystore.New(postgrestest.NewDatabase(t, module).Open(module))
}
