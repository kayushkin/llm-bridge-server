package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	kanbandb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/db"
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

func openKanbanStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := kanbandb.Module()
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

// enableTaskTrackingModules opens noteboard, kanban-store and project-store in
// Postgres, serves them from srv, and when project_store.session_filer.enabled
// is on starts the filer's passes. kanban-store calls noteboard and the access
// stores in this process; project-store calls noteboard and kanban-store in
// this process, and calls this server's own routes (sessions, briefs, the
// one-shot router) over HTTP with the service token, as it did when it was a
// service of its own: the passes move to bridge-worker, another process, in
// the end. It reads repo-store and work-graph-store in this process. The
// returned function closes the pools.
func enableTaskTrackingModules(ctx context.Context, cfg *config.Config, srv *server.Server, accessStores *access.Stores) (func(), error) {
	principals := accessStores.Principals
	noteboardDB, err := openNoteboardModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("noteboard: %w", err)
	}
	kanbanStoreDB, err := openKanbanStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		noteboardDB.Close()
		return nil, fmt.Errorf("kanban-store: %w", err)
	}
	projectStoreDB, err := openProjectStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		kanbanStoreDB.Close()
		noteboardDB.Close()
		return nil, fmt.Errorf("project-store: %w", err)
	}
	closeModules := func() {
		if err := errors.Join(projectStoreDB.Close(), kanbanStoreDB.Close(), noteboardDB.Close()); err != nil {
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

	kanbanMessageTriggersEnabled := cfg.Settings.Boolean(config.SettingKanbanStoreMessageTriggersEnabled)
	if err := server.CheckKanbanStoreConfiguration(cfg, kanbanMessageTriggersEnabled); err != nil {
		closeModules()
		return nil, fmt.Errorf("kanban-store: %w", err)
	}
	srv.EnableKanbanStore(kanbandb.New(kanbanStoreDB), server.KanbanStoreOwners{Noteboard: noteboardStore, Access: accessStores, MessageTriggersEnabled: kanbanMessageTriggersEnabled})
	kanbanForProjects, err := srv.KanbanStoreHTTPClientAsInternalService()
	if err != nil {
		closeModules()
		return nil, fmt.Errorf("project-store: %w", err)
	}

	settings := cfg.Settings
	projects := projectstore.New(projectStoreDB)
	owners := &projectstore.Owners{
		HTTP:                &http.Client{Timeout: 10 * time.Second},
		Repositories:        srv.RepositoriesHandler(),
		Kanban:              kanbanForProjects,
		KanbanStoreURL:      server.KanbanStoreInProcessBaseURL,
		Scheduler:           srv.SchedulerHTTPClient(),
		SchedulerURL:        server.SchedulerInProcessBaseURL,
		HealthcheckURL:      cfg.HealthcheckURL,
		Principals:          projectstore.PrincipalStoreModule{Store: principals},
		Noteboard:           projectstore.NoteboardModule{Store: noteboardStore},
		BridgeURL:           bridgeURL,
		BridgeServiceToken:  cfg.ServiceToken,
		SuggestionModelRole: settings.String(config.SettingProjectStoreSuggestionModelRole),
		ModelHTTP:           &http.Client{Timeout: 5 * time.Minute},
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
	log.Printf("postgres: noteboard open, served at /noteboard/; kanban-store open, served at /kanban/ and /kanban-store/ (%s); project-store open, served at /project-store/", srv.KanbanStoreDescription())

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

// importKanbanStoreCmd copies a standalone kanban-store's SQLite file into the
// Postgres module, keeping every id and time (to the microsecond), and prints
// what it copied, one line per table. It refuses a module that already holds
// any row.
func importKanbanStoreCmd(args []string) {
	sqlitePath := importArguments("import-kanban-store", "kanban-store", args)
	ctx := context.Background()
	db, err := openKanbanStoreModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := kanbandb.ImportStandaloneDatabase(ctx, db, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	encoded, err := json.Marshal(counts)
	if err != nil {
		importFailed(err)
	}
	var byTable map[string]int64
	if err := json.Unmarshal(encoded, &byTable); err != nil {
		importFailed(err)
	}
	names := make([]string, 0, len(byTable))
	for name := range byTable {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-40s %d\n", name+":", byTable[name])
	}
}
