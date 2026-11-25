package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/activitystore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/messagestore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

var (
	messageStoreModule = connectedRecordsModule{"message-store", messagestore.Module,
		func(ctx context.Context, db *sql.DB, path, owner string) (string, error) {
			counts, err := messagestore.ImportStandaloneDatabase(ctx, db, path, owner)
			return counts.Summary(), err
		}}
	timeLedgerStoreModule = connectedRecordsModule{"time-ledger-store", timeledgerstore.Module,
		func(ctx context.Context, db *sql.DB, path, owner string) (string, error) {
			counts, err := timeledgerstore.ImportStandaloneDatabase(ctx, db, path, owner)
			return counts.Summary(), err
		}}
	// activity-store's import also carries attachment bytes into file-store,
	// so it has a command of its own (importActivityStoreCmd); run is unused.
	activityStoreModule = connectedRecordsModule{"activity-store", activitystore.Module, nil}
)

// message-store and time-ledger-store import as the other connected_records
// stores do, through importConnectedRecordsStoreCmd.
func init() {
	for command, module := range map[string]connectedRecordsModule{
		"import-message-store":     messageStoreModule,
		"import-time-ledger-store": timeLedgerStoreModule,
	} {
		if _, taken := connectedRecordsImports[command]; taken {
			panic("two connected records imports are named " + command)
		}
		connectedRecordsImports[command] = module
	}
}

// enableMessageActivityAndTimeLedgerStores opens message-store, activity-store
// and time-ledger-store in Postgres and serves them from srv. It runs after
// enableConnectedRecords and after file-store is enabled. The returned
// function closes the pools.
func enableMessageActivityAndTimeLedgerStores(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	var pools []*sql.DB
	closePools := func() {
		var errs []error
		for _, pool := range pools {
			errs = append(errs, pool.Close())
		}
		if err := errors.Join(errs...); err != nil {
			log.Printf("postgres: closing message-store, activity-store and time-ledger-store: %v", err)
		}
	}
	var modules server.MessageActivityAndTimeLedgerModules
	for _, opened := range []struct {
		store connectedRecordsModule
		into  **sql.DB
	}{{messageStoreModule, &modules.Messages}, {activityStoreModule, &modules.Activity}, {timeLedgerStoreModule, &modules.TimeLedger}} {
		db, err := opened.store.open(ctx, cfg.PostgresURL)
		if err != nil {
			closePools()
			return nil, err
		}
		pools = append(pools, db)
		*opened.into = db
	}
	if err := srv.EnableMessageActivityAndTimeLedgerStores(modules); err != nil {
		closePools()
		return nil, err
	}
	log.Printf("postgres: message-store, activity-store and time-ledger-store open, served at /message-store/, /activity-store/ and /time-ledger-store/")
	return closePools, nil
}

// importActivityStoreCmd copies a standalone activity-store's SQLite file
// into its Postgres module, owned by -owner, and carries every attachment's
// bytes from -attachments into file-store (file_store.data_directory),
// checking each one's SHA-256. It prints how many rows of each table it
// copied, and nothing of the records.
func importActivityStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-activity-store", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone activity-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	attachments := flags.String("attachments", "", "the standalone store's attachments directory (its data directory's attachments/), which is only read")
	owner := flags.String("owner", "", "the principal-store id of the person who owns every record copied (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *attachments == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-activity-store -sqlite <file> -attachments <directory> -owner <principal id>")
		os.Exit(2)
	}
	if err := ownership.CheckPrincipalID("-owner", *owner); err != nil {
		importFailed(err)
	}
	url := importPostgresURL()
	cfg := config.Load()
	ctx := context.Background()
	if err := checkOwnerIsAPerson(ctx, url, *owner); err != nil {
		importFailed(err)
	}
	fileStoreDB, err := openModuleDescribedBy(ctx, url, filestore.Module)
	if err != nil {
		importFailed(err)
	}
	defer fileStoreDB.Close()
	files, err := filestore.New(fileStoreDB, cfg.Settings.String(config.SettingFileStoreDataDirectory))
	if err != nil {
		importFailed(err)
	}
	fileStoreHandler := filestore.NewServer(files, func() filestore.Limits {
		return filestore.Limits{
			MaximumFileBytes:   int64(cfg.Settings.Integer(config.SettingFileStoreMaximumFileBytes)),
			InlineContentTypes: cfg.Settings.StringList(config.SettingFileStoreInlineContentTypes),
		}
	}).Handler()
	db, err := activityStoreModule.open(ctx, url)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := activitystore.ImportStandaloneDatabase(ctx, db, server.ActivityAttachmentFilesIn(fileStoreHandler), *sqlitePath, *attachments, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported activity-store into Postgres, owned by %s: %s\n", *owner, counts.Summary())
}
