package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/agents/hookstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/harnessstore"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshotstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// bridgeOwnStores are the stores the server kept in SQLite files of its own
// until they moved into Postgres: harness-store (module harnesses), hook-store
// (hooks), snapshot-store (snapshots, with its blobs still in the git folder
// snapshot_store.git_path names) and the operations queue (operations).
type bridgeOwnStores struct {
	harnesses  *harnessstore.Store
	hooks      *hookstore.Store
	snapshots  *snapshotstore.Store
	operations *operationstore.Store
	pools      []*sql.DB
}

func (stores *bridgeOwnStores) close() {
	for _, pool := range stores.pools {
		pool.Close()
	}
}

// openBridgeOwnStoreModules provisions, migrates and opens the four modules
// and stops the server if any step fails: every session starts on an
// instance harness-store holds, every spawn reads its hooks, every tool call
// may be snapshotted, and every operation is queued in the operations module,
// so a server without any of them would do its work wrongly rather than not
// at all.
func openBridgeOwnStoreModules(ctx context.Context, cfg *config.Config) *bridgeOwnStores {
	stores := &bridgeOwnStores{}
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
	stores.harnesses = harnessstore.New(open("harness-store", harnessstore.Module))
	stores.hooks = hookstore.New(open("hook-store", hookstore.Module))
	var err error
	stores.snapshots, err = snapshotstore.New(open("snapshot-store", snapshotstore.Module), snapshotstore.Config{GitDir: cfg.SnapshotStoreGit})
	if err != nil {
		stores.close()
		log.Fatalf("snapshot-store: %v", err)
	}
	stores.operations = operationstore.New(open("operations", operationstore.Module))
	log.Printf("postgres: harness-store, hook-store, snapshot-store (blobs in %s) and operations open", cfg.SnapshotStoreGit)
	return stores
}

// importBridgeOwnStoreCmd copies one of the server's former SQLite files into
// its module, keeping every id, and prints the counts it copied. It refuses a
// module that already holds any row.
func importBridgeOwnStoreCmd(command string, args []string, describe func() (postgresstorage.Module, error), importFile func(context.Context, *sql.DB, string) (any, error)) {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the SQLite file to copy (a copy of it, made with sqlite3 .backup, while the server runs)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: llm-bridge-server %s -sqlite <file>\n", command)
		os.Exit(2)
	}
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set, so there is no module to import into\n", config.PostgresURLEnvironmentVariable)
		os.Exit(1)
	}
	ctx := context.Background()
	module, err := describe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	db, err := openPostgresModule(ctx, cfg.PostgresURL, module)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	counts, err := importFile(ctx, db, *sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: nothing was imported: %v\n", err)
		os.Exit(1)
	}
	report, err := json.MarshalIndent(counts, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: the import finished, but its counts could not be written: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(report))
}

// runBridgeOwnStoreImportCommand runs the import subcommand named command and
// answers true, or answers false when command is not one of them.
func runBridgeOwnStoreImportCommand(command string, args []string) bool {
	switch command {
	case "import-harness-store":
		importBridgeOwnStoreCmd(command, args, harnessstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
			return harnessstore.ImportStandaloneDatabase(ctx, db, path)
		})
	case "import-hook-store":
		importBridgeOwnStoreCmd(command, args, hookstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
			return hookstore.ImportStandaloneDatabase(ctx, db, path)
		})
	case "import-snapshot-store":
		importBridgeOwnStoreCmd(command, args, snapshotstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
			return snapshotstore.ImportStandaloneDatabase(ctx, db, path)
		})
	case "import-operations":
		importBridgeOwnStoreCmd(command, args, operationstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
			return operationstore.ImportStandaloneDatabase(ctx, db, path)
		})
	default:
		return false
	}
	return true
}
