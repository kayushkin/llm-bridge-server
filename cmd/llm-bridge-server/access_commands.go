package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	permstore "github.com/kayushkin/llm-bridge-server/internal/access/permissionstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/signinstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// accessImport is one access store's import: its module, and how it copies a
// standalone SQLite file into it and says what it copied.
type accessImport struct {
	module func() (postgresstorage.Module, error)
	run    func(ctx context.Context, db *sql.DB, sqlitePath string) (string, error)
}

var accessImports = map[string]accessImport{
	"import-principal-store": {principalstore.Module, func(ctx context.Context, db *sql.DB, path string) (string, error) {
		counts, err := principalstore.ImportStandaloneDatabase(ctx, db, path)
		return fmt.Sprintf("%+v", counts), err
	}},
	"import-grant-store": {grantstore.Module, func(ctx context.Context, db *sql.DB, path string) (string, error) {
		counts, err := grantstore.ImportStandaloneDatabase(ctx, db, path)
		return fmt.Sprintf("%+v", counts), err
	}},
	"import-sign-in-store": {signinstore.Module, func(ctx context.Context, db *sql.DB, path string) (string, error) {
		counts, err := signinstore.ImportStandaloneDatabase(ctx, db, path)
		return fmt.Sprintf("%+v", counts), err
	}},
	"import-permission-store": {permstore.Module, func(ctx context.Context, db *sql.DB, path string) (string, error) {
		counts, err := permstore.ImportStandaloneDatabase(ctx, db, path)
		return fmt.Sprintf("%+v", counts), err
	}},
}

// importAccessStoreCmd copies a standalone access store's SQLite file into its
// Postgres module, keeping every id, number and time, and prints what it
// copied. Each store's import refuses a module that already holds its records.
// It reads the Postgres URL from the same setting the server does.
func importAccessStoreCmd(command string, args []string) {
	importer := accessImports[command]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
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
	module, err := importer.module()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	db, err := access.OpenModule(ctx, cfg.PostgresURL, module)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	counts, err := importer.run(ctx, db, *sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: nothing was imported: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(counts)
}

// makeSignInLinkCmd prints a one-time sign-in link for a principal: the way
// back in from a shell when nobody can sign in any other way. Having a shell
// on this host, with this server's Postgres URL, is the credential, so it
// needs no running server and no token. Only an enabled person gets a link.
func makeSignInLinkCmd(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server make-sign-in-link <principal id>")
		os.Exit(2)
	}
	cfg := config.Load()
	if cfg.PostgresURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set\n", config.PostgresURLEnvironmentVariable)
		os.Exit(1)
	}
	if cfg.SignInPageURL == "" {
		fmt.Fprintf(os.Stderr, "error: %s is not set, so a link would open no page\n", config.SignInPageURLEnvironmentVariable)
		os.Exit(1)
	}
	ctx := context.Background()
	stores, err := access.OpenStores(ctx, cfg.PostgresURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer stores.Close()
	directory := signinstore.NewDirectory(access.SignInPrincipalDirectory(stores.Principals), signinstore.DirectoryConfig{})
	link, url, err := signinstore.MakeSignInLinkFromHostShell(ctx, stores.SignIn, directory, args[0], cfg.SignInPageURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(signinstore.HostShellLinkMessage(link, url))
}
