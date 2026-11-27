package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/server"
	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
)

func openNoteboardModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := items.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

func openKanbanStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := issuesdb.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

func openProjectStoreModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := projects.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// enableTaskTrackingModules opens noteboard, kanban-store and project-store in
// Postgres, serves them from srv, and when projects.session_filer.enabled
// is on starts the filer's passes. kanban-store calls noteboard and the access
// stores in this process; project-store calls noteboard, kanban-store,
// repo-store, work-graph-store, the scheduler and this server's sessions,
// briefs and one-shot router through their Go methods, in this process. Only
// healthcheck, a service of its own, is asked over HTTP. The returned
// function closes the pools.
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
	noteboardStore := items.New(noteboardDB)
	srv.EnableNoteboard(noteboardStore)

	kanbanMessageTriggersEnabled := cfg.Settings.Boolean(config.SettingIssuesMessageTriggersEnabled)
	if err := server.CheckKanbanStoreConfiguration(cfg, kanbanMessageTriggersEnabled); err != nil {
		closeModules()
		return nil, fmt.Errorf("kanban-store: %w", err)
	}
	srv.EnableKanbanStore(issuesdb.New(kanbanStoreDB), server.KanbanStoreOwners{Noteboard: noteboardStore, Access: accessStores, MessageTriggersEnabled: kanbanMessageTriggersEnabled})

	settings := cfg.Settings
	projectStore := projects.New(projectStoreDB)
	owners := &projects.Owners{
		HTTP:                &http.Client{Timeout: 10 * time.Second},
		Repositories:        srv.ProjectStoreRepositories(),
		Kanban:              srv.ProjectStoreKanban(),
		Scheduler:           srv.ProjectStoreScheduler(),
		HealthcheckURL:      cfg.HealthcheckURL,
		Principals:          projects.PrincipalStoreModule{Store: principals},
		Noteboard:           projects.NoteboardModule{Store: noteboardStore},
		Bridge:              srv.ProjectStoreBridge(),
		SuggestionModelRole: settings.String(config.SettingProjectsSuggestionModelRole),
	}
	filer := &projects.SessionFiler{
		Store:                          projectStore,
		Owners:                         owners,
		ModelRole:                      settings.String(config.SettingProjectsSessionFilerModelRole),
		MinimumConfidence:              settings.Decimal(config.SettingProjectsSessionFilerMinimumConfidence),
		QuietBeforeFilingWithoutABrief: settings.Duration(config.SettingProjectsSessionFilerQuietBeforeFilingWithoutABrief),
		Window:                         settings.Duration(config.SettingProjectsSessionFilerWindow),
		SessionsPerPass:                settings.Integer(config.SettingProjectsSessionFilerSessionsPerPass),
	}
	if err := filer.Validate(); err != nil {
		closeModules()
		return nil, fmt.Errorf("project-store: %w", err)
	}
	srv.EnableProjectStore(&projects.Handlers{Store: projectStore, Owners: owners, SessionFiler: filer})
	log.Printf("postgres: noteboard open, served at /noteboard/; kanban-store open, served at /issues/ (%s); project-store open, served at /projects/", srv.KanbanStoreDescription())

	if settings.Boolean(config.SettingProjectsSessionFilerEnabled) {
		interval := settings.Duration(config.SettingProjectsSessionFilerInterval)
		if interval <= 0 {
			closeModules()
			return nil, fmt.Errorf("project-store: %s is %s; it must be longer than nothing", config.SettingProjectsSessionFilerInterval, interval)
		}
		go srv.RunProjectStoreSessionFiler(ctx, interval)
		log.Printf("project-store: the session filer runs a pass every %s", interval)
	} else {
		log.Printf("project-store: the session filer runs no passes here (%s is off); POST /projects/session-filings/run runs one", config.SettingProjectsSessionFilerEnabled)
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
	sqlitePath := importArguments("import-projects", "project-store", args)
	ctx := context.Background()
	db, err := openProjectStoreModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := projects.ImportStandaloneDatabase(ctx, db, sqlitePath)
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
	sqlitePath := importArguments("import-issues", "kanban-store", args)
	ctx := context.Background()
	db, err := openKanbanStoreModule(ctx, importPostgresURL())
	if err != nil {
		importFailed(err)
	}
	defer db.Close()
	counts, err := issuesdb.ImportStandaloneDatabase(ctx, db, sqlitePath)
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
