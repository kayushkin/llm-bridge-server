package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projectstore"
)

func openNoteboardModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := items.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

func openProjectStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := projectstore.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// ownAddress is where this process answers on its own machine, from the
// address it listens on: a host left empty, or the unspecified address,
// listens on loopback too, so loopback is where it answers itself.
func ownAddress(listenAddress string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", fmt.Errorf("the listen address %q: %w", listenAddress, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// enableTaskTrackingModules opens noteboard and project-store in Postgres,
// serves them from srv, and when project_store.session_filer.enabled is on
// starts the filer's passes. project-store calls noteboard in this process,
// and calls this server's own routes (sessions, briefs, the one-shot router)
// over HTTP with the service token, as it did when it was a service of its
// own: the passes move to bridge-worker, another process, in the end. It
// reads repo-store and work-graph-store in this process. The returned
// function closes the pools.
func enableTaskTrackingModules(ctx context.Context, cfg *config.Config, srv *server.Server, principals *principalstore.Store) (func(), error) {
	noteboardDB, err := openNoteboardModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("noteboard: %w", err)
	}
	projectStoreDB, err := openProjectStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		noteboardDB.Close()
		return nil, fmt.Errorf("project-store: %w", err)
	}
	closeModules := func() {
		if err := errors.Join(projectStoreDB.Close(), noteboardDB.Close()); err != nil {
			log.Printf("postgres: closing the task tracking modules: %v", err)
		}
	}
	bridgeURL, err := ownAddress(cfg.ListenAddr)
	if err != nil {
		closeModules()
		return nil, fmt.Errorf("project-store: %w", err)
	}

	noteboardStore := items.New(noteboardDB)
	srv.EnableNoteboard(noteboardStore)

	settings := cfg.Settings
	projects := projectstore.New(projectStoreDB)
	owners := &projectstore.Owners{
		HTTP:                    &http.Client{Timeout: 10 * time.Second},
		Repositories:            srv.RepositoriesHandler(),
		KanbanStoreURL:          cfg.KanbanStoreURL,
		KanbanStoreServiceToken: settings.String("kanban_store.service_token"),
		SchedulerURL:            cfg.SchedulerURL,
		HealthcheckURL:          cfg.HealthcheckURL,
		Principals:              projectstore.PrincipalStoreModule{Store: principals},
		Noteboard:               projectstore.NoteboardModule{Store: noteboardStore},
		BridgeURL:               bridgeURL,
		BridgeServiceToken:      cfg.ServiceToken,
		SuggestionModelRole:     settings.String(config.SettingProjectStoreSuggestionModelRole),
		ModelHTTP:               &http.Client{Timeout: 5 * time.Minute},
	}
	filer := &projectstore.SessionFiler{
		Store:                          projects,
		Owners:                         owners,
		ModelRole:                      settings.String(config.SettingProjectStoreSessionFilerModelRole),
		MinimumConfidence:              settings.Decimal(config.SettingProjectStoreSessionFilerMinimumConfidence),
		QuietBeforeFilingWithoutABrief: settings.Duration(config.SettingProjectStoreSessionFilerQuietBeforeFilingWithoutABrief),
		Window:                         settings.Duration(config.SettingProjectStoreSessionFilerWindow),
		SessionsPerPass:                settings.Integer(config.SettingProjectStoreSessionFilerSessionsPerPass),
	}
	if err := filer.Validate(); err != nil {
		closeModules()
		return nil, fmt.Errorf("project-store: %w", err)
	}
	srv.EnableProjectStore(&projectstore.Handlers{Store: projects, Owners: owners, SessionFiler: filer})
	log.Printf("postgres: noteboard open, served at /noteboard/; project-store open, served at /project-store/")

	if settings.Boolean(config.SettingProjectStoreSessionFilerEnabled) {
		interval := settings.Duration(config.SettingProjectStoreSessionFilerInterval)
		if interval <= 0 {
			closeModules()
			return nil, fmt.Errorf("project-store: %s is %s; it must be longer than nothing", config.SettingProjectStoreSessionFilerInterval, interval)
		}
		go srv.RunProjectStoreSessionFiler(ctx, interval)
		log.Printf("project-store: the session filer runs a pass every %s", interval)
	} else {
		log.Printf("project-store: the session filer runs no passes here (%s is off); POST /project-store/session-filings/run runs one", config.SettingProjectStoreSessionFilerEnabled)
	}
	return closeModules, nil
}

// importNoteboardCmd copies a standalone noteboard's SQLite file into the
// Postgres module, keeping every id and time, and prints what it copied. It
// refuses a module that already holds any row. It reads the Postgres URL from
// the same setting the server does.
func importNoteboardCmd(args []string) {
	sqlitePath := importArguments("import-noteboard", "noteboard", args)
	ctx := context.Background()
	db, err := openNoteboardModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := items.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("items:     %d (%d soft-deleted, %d held, %d with a schedule)\n", counts.Items, counts.DeletedItems, counts.HeldItems, counts.ScheduledItems)
	fmt.Printf("revisions: %d, last id %d\n", counts.Revisions, counts.LastRevisionID)
}

// importProjectStoreCmd copies a standalone project-store's SQLite file into
// the Postgres module, keeping every id, number and time.
func importProjectStoreCmd(args []string) {
	sqlitePath := importArguments("import-project-store", "project-store", args)
	ctx := context.Background()
	db, err := openProjectStoreModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := projectstore.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	fmt.Printf("projects:        %d, numbering on after %d\n", counts.Projects, counts.LastProjectNumber)
	fmt.Printf("links:           %d, last id %d\n", counts.Links, counts.LastLinkID)
	fmt.Printf("changes:         %d, last id %d\n", counts.Changes, counts.LastChangeID)
	fmt.Printf("suggestions:     %d, numbering on after %d\n", counts.Suggestions, counts.LastSuggestionNumber)
	fmt.Printf("session filings: %d, numbering on after %d\n", counts.SessionFilings, counts.LastSessionFilingNumber)
}
