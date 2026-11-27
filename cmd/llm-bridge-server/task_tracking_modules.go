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
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issueitems"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboardsplit"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
)

func openNotesModule(ctx context.Context, url string) (*sql.DB, error) {
	module, err := notes.Module()
	if err != nil {
		return nil, err
	}
	return openPostgresModule(ctx, url, module)
}

// noteboardSplitModules are the two modules noteboard's records are split
// between: notes, and issues.
func noteboardSplitModules() (noteboardsplit.Modules, error) {
	notesModule, err := notes.Module()
	if err != nil {
		return noteboardsplit.Modules{}, err
	}
	issuesModule, err := issuesdb.Module()
	if err != nil {
		return noteboardsplit.Modules{}, err
	}
	return noteboardsplit.Modules{Notes: notesModule, Issues: issuesModule}, nil
}

// moveTodosOutOfNotes moves the todos an install's noteboard module held in
// Postgres, now its notes module, into issues, once: after the first start of
// a binary that knows issues own them, notes holds no todo and nothing moves.
// Both modules must be migrated first.
func moveTodosOutOfNotes(ctx context.Context, url string) error {
	modules, err := noteboardSplitModules()
	if err != nil {
		return err
	}
	administrator, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("the Postgres URL: %w", err)
	}
	defer administrator.Close()
	moved, err := noteboardsplit.MoveTodosOutOfNotes(ctx, administrator, modules)
	if err != nil {
		return err
	}
	if moved.Issues > 0 || moved.Revisions > 0 {
		log.Printf("postgres: moved %d todos and %d of their revisions (%d of purged cards) from module %s into module %s, ids kept",
			moved.Issues, moved.Revisions, moved.RevisionsOfPurgedCards, modules.Notes.Schema, modules.Issues.Schema)
	}
	return nil
}

func openIssuesModule(ctx context.Context, url string) (*sql.DB, error) {
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

// enableTaskTrackingModules opens notes, issues (boards, cards and the
// content of each issue) and project-store in Postgres, moves the todos an
// older install kept in noteboard into issues, serves them from srv, and when
// projects.session_filer.enabled is on starts the filer's passes. issues
// calls the access stores in this process; project-store calls notes,
// issues,
// repo-store, work-graph-store, the scheduler and this server's sessions,
// briefs and one-shot router through their Go methods, in this process. Only
// healthcheck, a service of its own, is asked over HTTP. The returned
// function closes the pools.
func enableTaskTrackingModules(ctx context.Context, cfg *config.Config, srv *server.Server, accessStores *access.Stores) (func(), error) {
	principals := accessStores.Principals
	notesDB, err := openNotesModule(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("notes: %w", err)
	}
	issuesDB, err := openIssuesModule(ctx, cfg.PostgresURL)
	if err != nil {
		notesDB.Close()
		return nil, fmt.Errorf("issues: %w", err)
	}
	projectStoreDB, err := openProjectStoreModule(ctx, cfg.PostgresURL)
	if err != nil {
		issuesDB.Close()
		notesDB.Close()
		return nil, fmt.Errorf("project-store: %w", err)
	}
	closeModules := func() {
		if err := errors.Join(projectStoreDB.Close(), issuesDB.Close(), notesDB.Close()); err != nil {
			log.Printf("postgres: closing the task tracking modules: %v", err)
		}
	}
	if err := moveTodosOutOfNotes(ctx, cfg.PostgresURL); err != nil {
		closeModules()
		return nil, err
	}
	notesStore := notes.New(notesDB)
	srv.EnableNotes(notesStore)
	issueItemsStore := issueitems.New(issuesDB)

	kanbanMessageTriggersEnabled := cfg.Settings.Boolean(config.SettingIssuesMessageTriggersEnabled)
	if err := server.CheckKanbanStoreConfiguration(cfg, kanbanMessageTriggersEnabled); err != nil {
		closeModules()
		return nil, fmt.Errorf("kanban-store: %w", err)
	}
	srv.EnableIssues(issuesdb.New(issuesDB), server.KanbanStoreOwners{IssueItems: issueItemsStore, Access: accessStores, MessageTriggersEnabled: kanbanMessageTriggersEnabled})

	settings := cfg.Settings
	projectStore := projects.New(projectStoreDB)
	owners := &projects.Owners{
		HTTP:                &http.Client{Timeout: 10 * time.Second},
		Repositories:        srv.ProjectStoreRepositories(),
		Kanban:              srv.ProjectStoreKanban(),
		Scheduler:           srv.ProjectStoreScheduler(),
		HealthcheckURL:      cfg.HealthcheckURL,
		Principals:          projects.PrincipalStoreModule{Store: principals},
		NotesAndIssues:      projects.NotesAndIssuesModules{Notes: notesStore, Issues: issueItemsStore},
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
	srv.EnableProjects(&projects.Handlers{Store: projectStore, Owners: owners, SessionFiler: filer})
	log.Printf("postgres: notes open, served at /notes/; issues open, served at /issues/, the content of each at /issues/items (%s); project-store open, served at /projects/", srv.IssuesDescription())

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
// two modules that own its records now, keeping every id, time and revision:
// todos and their revisions into issues, everything else into notes. It
// refuses modules that already hold any item or revision, and prints what it
// copied into each. Run import-issues first, so a revision of a purged card
// goes with the card's history. It reads the Postgres URL from the same
// setting the server does.
func importNoteboardCmd(args []string) {
	sqlitePath := importArguments("import-noteboard", "noteboard", args)
	ctx := context.Background()
	url := importPostgresURL()
	for _, open := range []func(context.Context, string) (*sql.DB, error){openNotesModule, openIssuesModule} {
		db, err := open(ctx, url)
		if err != nil {
			importFailed(err)
		}
		db.Close()
	}
	modules, err := noteboardSplitModules()
	if err != nil {
		importFailed(err)
	}
	administrator, err := sql.Open("pgx", url)
	if err != nil {
		importFailed(fmt.Errorf("the Postgres URL: %w", err))
	}
	defer administrator.Close()
	counts, err := noteboardsplit.ImportStandaloneDatabase(ctx, administrator, modules, sqlitePath)
	if err != nil {
		importFailed(fmt.Errorf("nothing was imported: %w", err))
	}
	for _, module := range []struct {
		name   string
		counts items.StandaloneCopyCounts
	}{{"notes", counts.Notes}, {"issues", counts.Issues}} {
		fmt.Printf("%-7s items:     %d (%d soft-deleted, %d held, %d with a schedule)\n", module.name, module.counts.Items, module.counts.DeletedItems, module.counts.HeldItems, module.counts.ScheduledItems)
		fmt.Printf("%-7s revisions: %d\n", module.name, module.counts.Revisions)
	}
	fmt.Printf("revisions of purged items: %d to issues (cards), %d to notes\n", counts.RevisionsOfPurgedItemsInIssues, counts.RevisionsOfPurgedItemsInNotes)
	fmt.Printf("revision numbers continue after %d in both\n", counts.LastRevisionNumberHandedOut)
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
	db, err := openIssuesModule(ctx, importPostgresURL())
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
