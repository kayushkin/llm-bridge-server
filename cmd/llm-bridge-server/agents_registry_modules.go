package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/agents/agentstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/memorystore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/modelstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// agentsRegistryStores are agent-store, memory-store and model-store, opened
// in Postgres before the server is built, since server.New takes them.
type agentsRegistryStores struct {
	agents   *agentstore.Store
	memories *memorystore.Store
	models   *modelstore.Store
	pools    []*sql.DB
}

func (stores *agentsRegistryStores) close() {
	for _, pool := range stores.pools {
		pool.Close()
	}
}

// openAgentsRegistryModules provisions, migrates and opens the modules agents,
// memories and models, and stops the server if any step fails: every session's
// model is resolved through model-store and every session's prompt through
// agent-store, so a server without them cannot start a session. agent-store
// scans and renders prompt files under this process's home directory, as the
// embedded copy did.
func openAgentsRegistryModules(ctx context.Context, cfg *config.Config) *agentsRegistryStores {
	stores := &agentsRegistryStores{}
	open := func(name string, describe func() (postgresstorage.Module, error)) *sql.DB {
		module, err := describe()
		if err != nil {
			stores.close()
			log.Fatalf("%s: %v", name, err)
		}
		db, err := openPostgresModule(ctx, cfg.PostgresURL, module)
		if err != nil {
			stores.close()
			log.Fatalf("%s: %v", name, err)
		}
		stores.pools = append(stores.pools, db)
		return db
	}

	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("agent-store: the home directory its scan walks: %v", err)
	}
	stores.agents, err = agentstore.New(open("agent-store", agentstore.Module), homeDirectory)
	if err != nil {
		stores.close()
		log.Fatalf("agent-store: %v", err)
	}
	stores.memories = memorystore.New(open("memory-store", memorystore.Module))
	stores.models = modelstore.New(open("model-store", modelstore.Module))
	log.Printf("postgres: agent-store (prompt files under %s), memory-store and model-store open; model-store's own routes at /model-store/", homeDirectory)
	return stores
}

// importAgentStoreCmd copies a standalone agent-store's SQLite file into its
// module, keeping every id. It writes no prompt file: the rows only.
func importAgentStoreCmd(args []string) {
	importAgentsStoreCmd("import-agent-store", args, agentstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return agentstore.ImportStandaloneDatabase(ctx, db, path)
	})
}

func importMemoryStoreCmd(args []string) {
	importAgentsStoreCmd("import-memory-store", args, memorystore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return memorystore.ImportStandaloneDatabase(ctx, db, path)
	})
}

// importModelStoreCmd copies a standalone model-store's registry into its
// module. Its credential tables stay in the file: auth-store holds credentials.
func importModelStoreCmd(args []string) {
	importAgentsStoreCmd("import-model-store", args, modelstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return modelstore.ImportStandaloneDatabase(ctx, db, path)
	})
}
