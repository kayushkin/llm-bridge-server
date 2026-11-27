package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/agents/memories"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// agentsRegistryStores are agent-store, memory-store and model-store, opened
// in Postgres before the server is built, since server.New takes them.
type agentsRegistryStores struct {
	agents   *agents.Store
	memories *memories.Store
	models   *models.Store
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
	stores.agents, err = agents.New(open("agent-store", agents.Module), homeDirectory)
	if err != nil {
		stores.close()
		log.Fatalf("agent-store: %v", err)
	}
	stores.memories = memories.New(open("memory-store", memories.Module))
	stores.models = models.New(open("model-store", models.Module))
	log.Printf("postgres: agent-store (prompt files under %s), memory-store and model-store open; model-store's own routes at /model-store/", homeDirectory)
	return stores
}

// importAgentStoreCmd copies a standalone agent-store's SQLite file into its
// module, keeping every id. It writes no prompt file: the rows only.
func importAgentStoreCmd(args []string) {
	importAgentsStoreCmd("import-agent-store", args, agents.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return agents.ImportStandaloneDatabase(ctx, db, path)
	})
}

func importMemoryStoreCmd(args []string) {
	importAgentsStoreCmd("import-memory-store", args, memories.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return memories.ImportStandaloneDatabase(ctx, db, path)
	})
}

// importModelStoreCmd copies a standalone model-store's registry into its
// module. Its credential tables stay in the file: auth-store holds credentials.
func importModelStoreCmd(args []string) {
	importAgentsStoreCmd("import-model-store", args, models.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return models.ImportStandaloneDatabase(ctx, db, path)
	})
}
