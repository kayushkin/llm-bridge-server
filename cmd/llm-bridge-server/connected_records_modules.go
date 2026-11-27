package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendar"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journal"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/people"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// connectedRecordsModule is one connected_records store: its module, and
// how it copies a standalone SQLite file into it for an owner.
type connectedRecordsModule struct {
	store  string
	module func() (postgresstorage.Module, error)
	run    func(ctx context.Context, db *sql.DB, sqlitePath, ownerPrincipalID string) (string, error)
}

var (
	peopleStoreModule = connectedRecordsModule{"people-store", people.Module,
		func(ctx context.Context, db *sql.DB, path, owner string) (string, error) {
			counts, err := people.ImportStandaloneDatabase(ctx, db, path, owner)
			return counts.Summary(), err
		}}
	calendarStoreModule = connectedRecordsModule{"calendar-store", calendar.Module,
		func(ctx context.Context, db *sql.DB, path, owner string) (string, error) {
			counts, err := calendar.ImportStandaloneDatabase(ctx, db, path, owner)
			return counts.Summary(), err
		}}
	journalStoreModule = connectedRecordsModule{"journal-store", journal.Module,
		func(ctx context.Context, db *sql.DB, path, owner string) (string, error) {
			counts, err := journal.ImportStandaloneDatabase(ctx, db, path, owner)
			return counts.Summary(), err
		}}
)

// connectedRecordsImports are the import commands, by name.
var connectedRecordsImports = map[string]connectedRecordsModule{
	"import-people":   peopleStoreModule,
	"import-calendar": calendarStoreModule,
	"import-journal":  journalStoreModule,
}

func (store connectedRecordsModule) open(ctx context.Context, url string) (*sql.DB, error) {
	module, err := store.module()
	if err != nil {
		return nil, err
	}
	db, err := openPostgresModule(ctx, url, module)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", store.store, err)
	}
	return db, nil
}

// enableConnectedRecords opens people-store, calendar-store and journal-store
// in Postgres and serves them from srv. It runs after access, noteboard and
// reference-store are enabled. The returned function closes the pools.
func enableConnectedRecords(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	var pools []*sql.DB
	closePools := func() {
		var errs []error
		for _, pool := range pools {
			errs = append(errs, pool.Close())
		}
		if err := errors.Join(errs...); err != nil {
			log.Printf("postgres: closing the connected records modules: %v", err)
		}
	}
	var modules server.ConnectedRecordsModules
	for _, opened := range []struct {
		store connectedRecordsModule
		into  **sql.DB
	}{{peopleStoreModule, &modules.People}, {calendarStoreModule, &modules.Calendar}, {journalStoreModule, &modules.Journal}} {
		db, err := opened.store.open(ctx, cfg.PostgresURL)
		if err != nil {
			closePools()
			return nil, err
		}
		pools = append(pools, db)
		*opened.into = db
	}
	if err := srv.EnableConnectedRecords(ctx, modules); err != nil {
		closePools()
		return nil, err
	}
	log.Printf("postgres: people-store, calendar-store and journal-store open, served at /people/, /calendar/ and /journal/")
	return closePools, nil
}

// importConnectedRecordsStoreCmd copies a standalone connected_records store's
// SQLite file into its Postgres module, stamping every record with the owner
// -owner names, and prints how many rows of each table it copied. The owner
// is required, has no default, and must be a person principal-store holds:
// these stores are for several people, and a record nobody owns would be
// either nobody's or everybody's.
func importConnectedRecordsStoreCmd(command string, args []string) {
	importer := connectedRecordsImports[command]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone "+importer.store+" database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	owner := flags.String("owner", "", "the principal-store id of the person who owns every record copied (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: llm-bridge-server %s -sqlite <file> -owner <principal id>\n", command)
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
	db, err := importer.open(ctx, url)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := importer.run(ctx, db, *sqlitePath, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported %s into Postgres, owned by %s: %s\n", importer.store, *owner, counts)
}

// checkOwnerIsAPerson refuses an owner principal-store does not hold, one that
// is disabled, and one that is not a person.
func checkOwnerIsAPerson(ctx context.Context, url, owner string) error {
	module, err := principals.Module()
	if err != nil {
		return err
	}
	db, err := openPostgresModule(ctx, url, module)
	if err != nil {
		return fmt.Errorf("principal-store, to check the owner: %w", err)
	}
	defer db.Close()
	principal, err := principals.New(db).Get(ctx, owner, false)
	if errors.Is(err, principals.ErrNotFound) {
		return fmt.Errorf("principal-store holds no principal %s, so it cannot own the records", owner)
	}
	if err != nil {
		return fmt.Errorf("principal-store, reading the owner %s: %w", owner, err)
	}
	if principal.Kind != principals.KindHuman {
		return fmt.Errorf("%s is a %q principal, and only a person owns connected records", owner, principal.Kind)
	}
	if principal.DisabledAt != 0 {
		return fmt.Errorf("%s is disabled in principal-store", owner)
	}
	return nil
}
