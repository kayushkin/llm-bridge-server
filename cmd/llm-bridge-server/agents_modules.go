package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundlestore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/skillstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore/tools"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// openPostgresModule makes module's role and schema on the Postgres at url if
// they are missing, brings its schema up to this binary's migrations, and
// opens its pool. url names a login that may create roles and schemas; each
// step that fails is the error, and nothing carries on without it.
func openPostgresModule(ctx context.Context, url string, module postgresstorage.Module) (*sql.DB, error) {
	administrator, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("the Postgres URL: %w", err)
	}
	defer administrator.Close()
	if err := administrator.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("reaching Postgres: %w", err)
	}
	if err := postgresstorage.Provision(ctx, administrator, module); err != nil {
		return nil, err
	}
	db, err := postgresstorage.Open(ctx, url, module)
	if err != nil {
		return nil, err
	}
	report, err := postgresstorage.Migrate(ctx, db, module)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, migration := range report.Applied {
		log.Printf("postgres: module %s: applied migration %d (%s)", module.Schema, migration.Version, migration.Name)
	}
	if len(report.NewerThanThisBinary) > 0 {
		log.Printf("postgres: module %s: the database has migrations %v that this binary does not know; a newer server migrated it", module.Schema, report.NewerThanThisBinary)
	}
	return db, nil
}

// openAgentsModule opens the module a store's Module function describes.
func openAgentsModule(ctx context.Context, url string, describe func() (postgresstorage.Module, error)) (*sql.DB, error) {
	module, err := describe()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableAgentsStoreModules opens tool-store, skill-store and bundle-store in
// Postgres and mounts them on srv. With no Postgres URL they stay off: their
// routes answer 503, a session gets no tools from the registry, and a session
// naming a bundle is refused. With one, a module that cannot be provisioned,
// migrated, opened or seeded stops the server. The returned function closes
// the pools.
func enableAgentsStoreModules(ctx context.Context, srv *server.Server, cfg *config.Config) func() {
	if cfg.PostgresURL == "" {
		log.Printf("postgres: %s is not set, so tool-store, skill-store and bundle-store are off and /tool-store/, /skill-store/ and /bundle-store/ answer 503", config.PostgresURLEnvironmentVariable)
		return func() {}
	}
	var pools []*sql.DB
	closeAll := func() {
		for _, pool := range pools {
			pool.Close()
		}
	}
	open := func(name string, describe func() (postgresstorage.Module, error)) *sql.DB {
		db, err := openAgentsModule(ctx, cfg.PostgresURL, describe)
		if err != nil {
			closeAll()
			log.Fatalf("%s: %v", name, err)
		}
		pools = append(pools, db)
		return db
	}

	// Before the registry of in-process tools is seeded: browser, web_search
	// and scheduler are in it only once they are given where PinchTab, Brave
	// and the scheduler are.
	tools.RegisterToolsThatReachOutsideServices(tools.OutsideServiceConnections{
		Pinchtab:    tools.PinchtabConnection{BaseURL: cfg.LocalToolsPinchtabURL, Token: cfg.LocalToolsPinchtabToken},
		BraveAPIKey: cfg.LocalToolsBraveAPIKey,
		Scheduler:   tools.SchedulerConnection{BaseURL: cfg.SchedulerURL, Token: cfg.LocalToolsSchedulerToken},
	})
	toolStore := toolstore.New(open("tool-store", toolstore.Module))
	if err := toolstore.SeedRegistry(ctx, toolStore); err != nil {
		closeAll()
		log.Fatalf("tool-store: seeding the registry: %v", err)
	}
	srv.EnableToolStore(toolStore, srv.ToolStoreCredentialResolver())

	skillStore, err := skillstore.New(open("skill-store", skillstore.Module), cfg.SkillStoreDataDirectory, cfg.SkillStoreInstallDirectory)
	if err != nil {
		closeAll()
		log.Fatalf("skill-store: %v", err)
	}
	srv.EnableSkillStore(skillStore)

	srv.EnableBundleStore(bundlestore.New(open("bundle-store", bundlestore.Module)))

	log.Printf("postgres: tool-store, skill-store and bundle-store open, served at /tool-store/, /skill-store/ and /bundle-store/ (skill files in %s)", cfg.SkillStoreDataDirectory)
	return closeAll
}

// importAgentsStoreCmd copies a standalone store's SQLite file into its
// Postgres module, keeping every id, and prints what it copied. It refuses a
// module that already holds any row. It reads the Postgres URL from the same
// setting the server does.
func importAgentsStoreCmd(command string, args []string, describe func() (postgresstorage.Module, error), importFile func(context.Context, *sql.DB, string) (any, error)) {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone store's database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
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
	db, err := openAgentsModule(ctx, cfg.PostgresURL, describe)
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

func importToolStoreCmd(args []string) {
	importAgentsStoreCmd("import-tool-store", args, toolstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return toolstore.ImportStandaloneDatabase(ctx, db, path)
	})
}

// importSkillStoreCmd copies the rows only: the skill files stay in the
// standalone store's data directory until they are copied into
// skill_store.data_directory.
func importSkillStoreCmd(args []string) {
	importAgentsStoreCmd("import-skill-store", args, skillstore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return skillstore.ImportStandaloneDatabase(ctx, db, path)
	})
}

func importBundleStoreCmd(args []string) {
	importAgentsStoreCmd("import-bundle-store", args, bundlestore.Module, func(ctx context.Context, db *sql.DB, path string) (any, error) {
		return bundlestore.ImportStandaloneDatabase(ctx, db, path)
	})
}
