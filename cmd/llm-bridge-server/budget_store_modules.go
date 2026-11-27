package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budget"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

// enableBudgetStore opens budget-store in Postgres, serves it from srv, and
// starts its passes when their settings turn any on. It runs after
// location-store is enabled, since trip matching reads its journeys. The
// returned function closes the pool.
func enableBudgetStore(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	db, err := openModuleDescribedBy(ctx, cfg.PostgresURL, budget.Module)
	if err != nil {
		return nil, fmt.Errorf("budget-store: %w", err)
	}
	closePool := func() {
		if err := db.Close(); err != nil {
			log.Printf("postgres: closing budget-store: %v", err)
		}
	}
	store := budget.New(db)
	if environment := cfg.Settings.String(config.SettingBudgetStorePlaidEnvironment); environment != "" {
		if cfg.AuthStoreURL == "" || cfg.AuthStoreToken == "" {
			closePool()
			return nil, fmt.Errorf("budget-store: %s is %q, and auth_store.url or auth_store.token is empty: Plaid's keys and every bank's access token live in auth-store",
				config.SettingBudgetStorePlaidEnvironment, environment)
		}
		store.SetPlaidVault(budget.AuthStoreVault{BaseURL: cfg.AuthStoreURL, Token: cfg.AuthStoreToken, PlaidEnvironment: environment,
			Client: &http.Client{Timeout: 30 * time.Second}})
		log.Printf("budget-store: Plaid on, environment %s", environment)
	}
	if err := srv.EnableBudgetStore(store); err != nil {
		closePool()
		return nil, err
	}
	if err := srv.StartBudgetStorePasses(ctx); err != nil {
		closePool()
		return nil, err
	}
	log.Printf("postgres: budget-store open, served at /budget/")
	return closePool, nil
}

// importBudgetStoreCmd copies a standalone budget-store's SQLite file into the
// budget module, every row owned by the principal -owner names.
func importBudgetStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-budget-store", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone budget-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	owner := flags.String("owner", "", "the principal-store id of the person whose books every row copied is in (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-budget-store -sqlite <file> -owner <principal id>")
		os.Exit(2)
	}
	if err := ownership.CheckPrincipalID("-owner", *owner); err != nil {
		importFailed(err)
	}
	url := importPostgresURL()
	ctx := context.Background()
	if err := checkOwnerIsAPerson(ctx, url, *owner); err != nil {
		importFailed(err)
	}
	db, err := openModuleDescribedBy(ctx, url, budget.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := budget.ImportStandaloneDatabase(ctx, db, *sqlitePath, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported budget-store into Postgres, owned by %s: %s\n", *owner, counts.Summary())
}
