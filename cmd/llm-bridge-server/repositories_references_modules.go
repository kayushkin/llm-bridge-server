package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/gitactivity"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// openModuleDescribedBy opens the module a store's Module function describes.
func openModuleDescribedBy(ctx context.Context, url string, describe func() (postgresstorage.Module, error)) (*sql.DB, error) {
	module, err := describe()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableRepositoriesAndReferences opens repo-store, work-graph-store and
// file-store in Postgres and builds reference-store, and serves all four from
// srv. reference-store looks records up in the services this server runs
// through srv itself, and in every other service over HTTP. The pull-request
// sync and the hook spool's ingest run here only when their settings say so.
// Any step that fails is the error, and the server does not start. The
// returned function closes the pools.
func enableRepositoriesAndReferences(ctx context.Context, cfg *config.Config, srv *server.Server, accessStores *access.Stores) (func(), error) {
	settings := cfg.Settings
	var pools []*sql.DB
	closeAll := func() {
		var errs []error
		for _, pool := range pools {
			errs = append(errs, pool.Close())
		}
		if err := errors.Join(errs...); err != nil {
			log.Printf("postgres: closing the repositories and file_storage modules: %v", err)
		}
	}
	open := func(name string, describe func() (postgresstorage.Module, error)) (*sql.DB, error) {
		db, err := openModuleDescribedBy(ctx, cfg.PostgresURL, describe)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		pools = append(pools, db)
		return db, nil
	}
	fail := func(err error) (func(), error) {
		closeAll()
		return nil, err
	}

	repoStoreDB, err := open("repo-store", repos.Module)
	if err != nil {
		return fail(err)
	}
	workGraphStoreDB, err := open("work-graph-store", gitactivity.Module)
	if err != nil {
		return fail(err)
	}
	fileStoreDB, err := open("file-store", files.Module)
	if err != nil {
		return fail(err)
	}

	// repo-store and work-graph-store.
	repoStore := repos.New(repoStoreDB)
	pullRequestSyncEnabled := settings.Boolean(config.SettingReposPullRequestSyncEnabled)
	var syncer *repos.PullRequestSyncer
	syncerOffReason := ""
	github, err := repos.NewGitHubCommandLine(settings.String(config.SettingReposGitHubCommandLine))
	switch {
	case err == nil:
		syncer = repos.NewPullRequestSyncer(repoStore, github)
	case pullRequestSyncEnabled:
		return fail(fmt.Errorf("repo-store: %s is on, and %s: %w", config.SettingReposPullRequestSyncEnabled, config.SettingReposGitHubCommandLine, err))
	default:
		syncerOffReason = fmt.Sprintf("this server cannot run gh (%s): %v", config.SettingReposGitHubCommandLine, err)
		log.Printf("repo-store: %s; stored pull requests stay readable, and no repo can be synced", syncerOffReason)
	}
	workGraph := gitactivity.New(workGraphStoreDB)
	srv.EnableRepositories(repoStore, syncer, syncerOffReason, workGraph)
	if pullRequestSyncEnabled {
		interval := settings.Duration(config.SettingReposPullRequestSyncInterval)
		if interval <= 0 {
			return fail(fmt.Errorf("repo-store: %s is %s; it must be longer than nothing", config.SettingReposPullRequestSyncInterval, interval))
		}
		go syncer.RunEvery(ctx, interval)
		log.Printf("repo-store: the pull-request sync runs every %s", interval)
	} else {
		log.Printf("repo-store: the pull-request sync runs no passes here (%s is off)", config.SettingReposPullRequestSyncEnabled)
	}
	if settings.Boolean(config.SettingGitActivityIngestEnabled) {
		ingester := &gitactivity.Ingester{
			Store:       workGraph,
			SpoolPath:   settings.String(config.SettingGitActivitySpoolPath),
			RepoStore:   gitactivity.RepoStoreModule{Store: repoStore},
			Interval:    2 * time.Second,
			SettleDelay: 200 * time.Millisecond,
		}
		go ingester.Run(ctx)
		log.Printf("work-graph-store: ingesting the git hook's spool %s", ingester.SpoolPath)
	} else {
		log.Printf("work-graph-store: not reading the git hook's spool (%s is off)", config.SettingGitActivityIngestEnabled)
	}

	// file-store.
	dataDirectory := settings.String(config.SettingFilesDataDirectory)
	files, err := files.New(fileStoreDB, dataDirectory)
	if err != nil {
		return fail(fmt.Errorf("file-store: %w", err))
	}
	if err := srv.EnableFileStore(files); err != nil {
		return fail(fmt.Errorf("file-store: %w", err))
	}

	// reference-store.
	serviceURLs := settings.StringMap(config.SettingReferencesServiceURLs)
	for service := range serviceURLs {
		if slices.Contains(server.InProcessReferenceServices, service) {
			return fail(fmt.Errorf("reference-store: %s names %s, which runs in this server and is asked here; take it out", config.SettingReferencesServiceURLs, service))
		}
	}
	siteURLs := settings.StringMap(config.SettingReferencesSiteURLs)
	if err := references.ValidateReferenceConfiguration(references.Types, serviceURLs, siteURLs); err != nil {
		return fail(fmt.Errorf("reference-store: %w", err))
	}
	seconds := func(key string) time.Duration { return time.Duration(settings.Integer(key)) * time.Second }
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := srv.EnableReferenceStore(&references.Resolver{
		Types:       references.Types,
		ServiceURLs: serviceURLs,
		SiteURLs:    siteURLs,
		Credentials: &references.Credentials{
			AuthStoreURL:       cfg.AuthStoreURL,
			AuthStoreToken:     cfg.AuthStoreToken,
			AuthStoreProviders: settings.StringMap(config.SettingReferencesAuthStoreProviders),
			HTTPClient:         client,
			TTL:                5 * time.Minute,
		},
		Access: &references.AccessChecker{
			Directory: server.ReferenceAccessDirectory(accessStores.Principals, accessStores.Grants, access.GrantsPrincipalDirectory(accessStores.Principals)),
			TTL:       seconds(config.SettingReferencesAccessCacheSeconds),
		},
		HTTPClient:    client,
		InProcess:     srv.InProcessReferenceReaders(),
		RecordTTL:     seconds(config.SettingReferencesRecordCacheSeconds),
		MissTTL:       seconds(config.SettingReferencesMissCacheSeconds),
		LookupTimeout: seconds(config.SettingReferencesLookupTimeout),
		Concurrency:   settings.Integer(config.SettingReferencesLookupConcurrency),
	}); err != nil {
		return fail(err)
	}
	inProcess := slices.Sorted(slices.Values(server.InProcessReferenceServices))
	log.Printf("postgres: repo-store, work-graph-store and file-store open, served at /repos/, /git-activity/ and /file-storage/ (file bytes in %s); reference-store served at /references/, reading %v in this process by Go calls and asking %d services over HTTP",
		dataDirectory, inProcess, len(serviceURLs))
	return closeAll, nil
}

// importRepoStoreCmd copies a standalone repo-store's SQLite file into the
// Postgres module, keeping every id and time.
func importRepoStoreCmd(args []string) {
	sqlitePath := importArguments("import-repos", "repo-store", args)
	ctx := context.Background()
	db, err := openModuleDescribedBy(ctx, importPostgresURL(), repos.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := repos.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("repos:              %d, numbering on after %d\n", counts.Repos, counts.LastRepoID)
	fmt.Printf("deployments:        %d, numbering on after %d\n", counts.Deployments, counts.LastDeploymentID)
	fmt.Printf("pull requests:      %d\n", counts.PullRequests)
	fmt.Printf("pull-request syncs: %d\n", counts.PullRequestSyncs)
}

// importWorkGraphStoreCmd copies a standalone work-graph-store's SQLite file
// into the Postgres module, keeping every id and time. The hook's spool is
// not touched.
func importWorkGraphStoreCmd(args []string) {
	sqlitePath := importArguments("import-git-activity", "work-graph-store", args)
	ctx := context.Background()
	db, err := openModuleDescribedBy(ctx, importPostgresURL(), gitactivity.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := gitactivity.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("ref updates: %d (%d with a repo-store id), numbering on after %d; newest recorded at %d\n",
		counts.RefUpdates, counts.WithARepoStoreID, counts.LastRefUpdateID, counts.NewestRecordedUnix)
}

// importFileStoreCmd copies a standalone file-store's records into the
// Postgres module and carries every blob a record names into this server's
// files.data_directory, checking each one's SHA-256.
func importFileStoreCmd(args []string) {
	flags := flag.NewFlagSet("import-files", flag.ExitOnError)
	sqlitePath := flags.String("sqlite", "", "the standalone file-store database to copy (a copy of it, made with sqlite3 .backup, while the store runs)")
	blobs := flags.String("blobs", "", "the standalone store's blobs directory (its data directory's blobs/), which is only read")
	carry := flags.String("carry", string(files.CopyBlobs), "how each blob is put in this server's data directory: copy, or link (a hard link, on one file system)")
	if err := flags.Parse(args); err != nil {
		os.Exit(2)
	}
	if *sqlitePath == "" || *blobs == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: llm-bridge-server import-files -sqlite <file> -blobs <directory> [-carry copy|link]")
		os.Exit(2)
	}
	postgresURL := importPostgresURL()
	cfg := config.Load()
	ctx := context.Background()
	db, err := openModuleDescribedBy(ctx, postgresURL, files.Module)
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	dataDirectory := cfg.Settings.String(config.SettingFilesDataDirectory)
	fileStore, err := files.New(db, dataDirectory)
	if err != nil {
		importFailed(err)
	}
	counts, err := files.ImportStandaloneDatabase(ctx, db, fileStore, *sqlitePath, *blobs, files.BlobCarry(*carry))
	if err != nil {
		importFailed(fmt.Errorf("no record was imported: %w", err))
	}
	fmt.Printf("files: %d (%d deleted), numbering on after %d\n", counts.Files, counts.DeletedFiles, counts.LastFileNumber)
	fmt.Printf("blobs: %d, %d bytes, in %s (%d were there already)\n", counts.Blobs, counts.BlobBytes, dataDirectory, counts.BlobsAlreadyHere)
	settingOf := map[string]string{"maximum_file_bytes": config.SettingFilesMaximumFileBytes, "inline_content_types": config.SettingFilesInlineContentTypes}
	keys := make([]string, 0, len(counts.StoredSettings))
	for key := range counts.StoredSettings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setting, known := settingOf[key]
		if !known {
			fmt.Printf("the standalone store had a setting this server does not know: %s=%q\n", key, counts.StoredSettings[key])
			continue
		}
		fmt.Printf("the standalone store had %s=%q; to keep it: PUT /settings/%s {\"value\":%q}\n", key, counts.StoredSettings[key], setting, counts.StoredSettings[key])
	}
}
