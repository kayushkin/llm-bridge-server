package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// openPossessionStore provisions and migrates the possessions module on the
// Postgres at url and answers the store over it, which opens one pool per
// scope it is narrowed to.
func openPossessionStore(ctx context.Context, url string) (possessions.PoolOpener, error) {
	module, err := possessions.Module()
	if err != nil {
		return nil, err
	}
	migrated, err := openPostgresModule(ctx, url, module)
	if err != nil {
		return nil, err
	}
	// Only provisioning and migrating need this pool; the store opens its own,
	// each naming the owner its connections act for.
	if err := migrated.Close(); err != nil {
		return nil, err
	}
	return func(ctx context.Context, settings map[string]string) (*sql.DB, error) {
		return postgresstorage.OpenWithSessionSettings(ctx, url, module, settings)
	}, nil
}

// enablePossessions opens possession-store in Postgres, serves it from srv,
// and starts its review worker when its settings say so. It runs after
// file-store, photo-store and mailstack are enabled and model-store is open.
// The returned function closes the store's pools.
func enablePossessions(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	settings := cfg.Settings
	open, err := openPossessionStore(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("possession-store: %w", err)
	}
	location, err := time.LoadLocation(settings.String(config.SettingPossessionStoreOwnerTimeZone))
	if err != nil {
		return nil, fmt.Errorf("possession-store: %s: %w", config.SettingPossessionStoreOwnerTimeZone, err)
	}
	var reviewOwners []string
	if settings.Boolean(config.SettingPossessionStoreReviewWorkerEnabled) {
		reviewOwners = settings.StringList(config.SettingPossessionStoreReviewWorkerOwnerPrincipalIDs)
	}
	module := server.PossessionsModule{
		Open:                   open,
		FirstReaderRole:        settings.ModelRole(config.SettingPossessionStoreFirstReaderRole),
		SecondReaderRole:       settings.ModelRole(config.SettingPossessionStoreSecondReaderRole),
		OwnerLocation:          location,
		ReviewImageLongestSide: settings.Integer(config.SettingPossessionStoreReviewImageLongestSide),
		CoverCacheDirectory:    settings.String(config.SettingPossessionStoreCoverCacheDirectory),
		ArticleStoreURL:        settings.String(config.SettingPossessionStoreArticleStoreURL),
		ReviewOwners:           reviewOwners,
	}
	if err := srv.EnablePossessions(module); err != nil {
		return nil, err
	}
	if err := srv.StartPossessionReviewWorkers(ctx); err != nil {
		srv.ClosePossessions()
		return nil, err
	}
	log.Printf("postgres: possession-store open, served at /possession-store/")
	return srv.ClosePossessions, nil
}

// importPossessionStoreCmd copies a standalone possession-store's SQLite file
// into the possessions module, stamping every record with the owner -owner
// names, and carries each original from the standalone data directory's
// originals/ into the file-store module under file_store.data_directory.
func importPossessionStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-possession-store", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone possession-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	originals := flags.String("originals", "", "the standalone store's originals directory (its data directory's originals/), which is only read")
	carry := flags.String("carry", string(filestore.CopyBlobs), "how each original is put in file-store: copy, or link (a hard link, on one file system, which takes no room)")
	owner := flags.String("owner", "", "the principal-store id of the person who owns every record copied (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *originals == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-possession-store -sqlite <file> -originals <directory> -owner <principal id> [-carry copy|link]")
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
	dataDirectory := cfg.Settings.String(config.SettingFileStoreDataDirectory)
	files, err := filestore.New(fileStoreDB, dataDirectory)
	if err != nil {
		importFailed(err)
	}
	limits := func() filestore.Limits {
		return filestore.Limits{MaximumFileBytes: int64(cfg.Settings.Integer(config.SettingFileStoreMaximumFileBytes))}
	}
	open, err := openPossessionStore(ctx, url)
	if err != nil {
		importFailed(err)
	}
	// The import checks no reference: every id it copies was checked when
	// the standalone store wrote it.
	root, err := possessions.New(open, possessions.ServiceReferences{}, server.PossessionOriginalFilesCarrying(files, limits, filestore.BlobCarry(*carry)))
	if err != nil {
		importFailed(err)
	}
	defer root.Close()
	counts, err := possessions.ImportStandaloneDatabase(ctx, root, *sqlitePath, *originals, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported possession-store into Postgres, owned by %s, originals in %s: %s\n", *owner, dataDirectory, counts.Summary())
}
