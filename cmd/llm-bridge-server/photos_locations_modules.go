package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/server"
)

// enablePhotosAndLocations opens photo-store and location-store in Postgres,
// serves them from srv, and starts photo-store's face passes when their
// settings turn any on. It runs after file-store and the connected records
// stores are enabled. The returned function closes the pools.
func enablePhotosAndLocations(ctx context.Context, cfg *config.Config, srv *server.Server) (func(), error) {
	photos, err := openModuleDescribedBy(ctx, cfg.PostgresURL, photos.Module)
	if err != nil {
		return nil, fmt.Errorf("photo-store: %w", err)
	}
	locations, err := openModuleDescribedBy(ctx, cfg.PostgresURL, locations.Module)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("location-store: %w", err), photos.Close())
	}
	closePools := func() {
		if err := errors.Join(photos.Close(), locations.Close()); err != nil {
			log.Printf("postgres: closing photo-store and location-store: %v", err)
		}
	}
	modules := server.PhotosAndLocationsModules{Photos: photos, Locations: locations,
		GeonamesDirectory: cfg.Settings.String(config.SettingLocationStoreGeonamesDirectory)}
	if err := srv.EnablePhotosAndLocations(modules); err != nil {
		closePools()
		return nil, err
	}
	if err := srv.StartPhotoFacePasses(ctx); err != nil {
		closePools()
		return nil, err
	}
	log.Printf("postgres: photo-store and location-store open, served at /photos/ and /locations/")
	return closePools, nil
}

// importPhotoStoreCmd copies a standalone photo-store's SQLite file into the
// photos module, stamping every record with the owner -owner names, and
// carries each photo's thumbnail from the standalone data directory into the
// file-store module under file_store.data_directory.
func importPhotoStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-photo-store", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone photo-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	thumbnails := flags.String("thumbnails", "", "the standalone store's thumbnails directory (its data directory's thumbnails/), which is only read")
	carry := flags.String("carry", string(files.CopyBlobs), "how each thumbnail is put in file-store: copy, or link (a hard link, on one file system, which takes no room)")
	owner := flags.String("owner", "", "the principal-store id of the person who owns every record copied (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *thumbnails == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-photo-store -sqlite <file> -thumbnails <directory> -owner <principal id> [-carry copy|link]")
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
	fileStoreDB, err := openModuleDescribedBy(ctx, url, files.Module)
	if err != nil {
		importFailed(err)
	}
	defer fileStoreDB.Close()
	dataDirectory := cfg.Settings.String(config.SettingFileStoreDataDirectory)
	fileStore, err := files.New(fileStoreDB, dataDirectory)
	if err != nil {
		importFailed(err)
	}
	limits := func() files.Limits {
		return files.Limits{MaximumFileBytes: int64(cfg.Settings.Integer(config.SettingFileStoreMaximumFileBytes))}
	}
	db, err := openModuleDescribedBy(ctx, url, photos.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := photos.ImportStandaloneDatabase(ctx, db, server.PhotoThumbnailFilesCarrying(fileStore, limits, files.BlobCarry(*carry)), *sqlitePath, *thumbnails, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported photo-store into Postgres, owned by %s, thumbnails in %s: %s\n", *owner, dataDirectory, counts.Summary())
}

// importLocationStoreCmd copies a standalone location-store's SQLite file
// into the locations module, stamping every record with the owner -owner
// names.
func importLocationStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-location-store", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone location-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	owner := flags.String("owner", "", "the principal-store id of the person who owns every record copied (principal_000001)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *owner == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-location-store -sqlite <file> -owner <principal id>")
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
	db, err := openModuleDescribedBy(ctx, url, locations.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := locations.ImportStandaloneDatabase(ctx, db, *sqlitePath, *owner)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("imported location-store into Postgres, owned by %s: %s\n", *owner, counts.Summary())
}
