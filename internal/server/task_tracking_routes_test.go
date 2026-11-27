package server

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issueitems"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboardsplit"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// taskTrackingStores are the stores enableTaskTrackingOnTestDatabase serves.
type taskTrackingStores struct {
	notes    *items.Store
	issues   *items.Store
	projects *projects.Store
}

// enableTaskTrackingOnTestDatabase opens notes, the issues module's items and
// project-store on a Postgres database of the test's own and serves them from
// server, with project-store calling notes and issues in process, as main
// wires them. The boards and cards of issues are not served.
func enableTaskTrackingOnTestDatabase(t *testing.T, server *Server) taskTrackingStores {
	t.Helper()
	notesModule, err := notes.Module()
	if err != nil {
		t.Fatal(err)
	}
	issuesModule, err := issuesdb.Module()
	if err != nil {
		t.Fatal(err)
	}
	projectsModule, err := projects.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, notesModule, issuesModule, projectsModule)
	stores := taskTrackingStores{
		notes:    notes.New(database.Open(notesModule)),
		issues:   issueitems.New(database.Open(issuesModule)),
		projects: projects.New(database.Open(projectsModule)),
	}
	server.EnableNotes(stores.notes)
	server.EnableIssueItems(stores.issues)
	server.EnableProjects(&projects.Handlers{Store: stores.projects, Owners: &projects.Owners{NotesAndIssues: projects.NotesAndIssuesModules{Notes: stores.notes, Issues: stores.issues}}})
	return stores
}

func TestTaskTrackingRoutesAnswer503WithNoPostgres(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for path, code := range map[string]string{"/notes/items": "notes_off", "/issues/items": "issues_off", "/projects/projects": "project_store_off"} {
		response := gated.requestAsService(t, "GET", path)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), code) {
			t.Errorf("GET %s with no module = %d %s, want 503 %s", path, response.Code, response.Body, code)
		}
	}
}

// The stores' own routes, behind their prefixes, answer as the standalone
// stores did.
func TestTaskTrackingRoutesServeTheStoresUnderTheirPrefixes(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	stores := enableTaskTrackingOnTestDatabase(t, gated.server)

	if refused := gated.requestAsServiceWithJSON(t, "POST", "/notes/items", map[string]any{"type": "todo", "title": "Run deploy.sh"}); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "/issues/items") {
		t.Fatalf("a todo written to notes = %d %s, want 400 naming /issues/items", refused.Code, refused.Body)
	}
	note := gated.requestAsServiceWithJSON(t, "POST", "/notes/items", map[string]any{"type": "note", "title": "Deploy playbook", "body": "run deploy.sh"})
	if note.Code != http.StatusCreated {
		t.Fatalf("create note = %d %s", note.Code, note.Body)
	}
	if found := gated.requestAsService(t, "GET", "/notes/search?q=deploy"); !strings.Contains(found.Body.String(), "Deploy playbook") {
		t.Fatalf("notes search = %d %s", found.Code, found.Body)
	}

	created := gated.requestAsServiceWithJSON(t, "POST", "/issues/items", map[string]any{"title": "Run deploy.sh", "tags": []string{"ops"}})
	var item struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s (%v)", created.Code, created.Body, err)
	}
	if !strings.Contains(created.Body.String(), `"type":"todo"`) {
		t.Fatalf("an issue made with no type reads %s, want type todo", created.Body)
	}
	if read := gated.requestAsService(t, "GET", "/issues/items/"+item.ID); read.Code != http.StatusOK || read.Header().Get("ETag") != `"`+item.UpdatedAt+`"` {
		t.Fatalf("read = %d, ETag %q, want the create's updated_at %q", read.Code, read.Header().Get("ETag"), item.UpdatedAt)
	}
	if found := gated.requestAsService(t, "GET", "/issues/items/search?q=deploy"); !strings.Contains(found.Body.String(), item.ID) || strings.Contains(found.Body.String(), "Deploy playbook") {
		t.Fatalf("issues search = %d %s, want the issue and not the note", found.Code, found.Body)
	}
	if notesFound := gated.requestAsService(t, "GET", "/notes/search?q=deploy"); strings.Contains(notesFound.Body.String(), item.ID) {
		t.Fatalf("notes search found the issue: %s", notesFound.Body)
	}
	if held := gated.requestAsServiceWithJSON(t, "POST", "/issues/items/"+item.ID+"/hold", map[string]any{"reason": "parked"}); held.Code != http.StatusOK {
		t.Fatalf("hold = %d %s", held.Code, held.Body)
	}
	if listed := gated.requestAsService(t, "GET", "/issues/items?tag=ops"); strings.Contains(listed.Body.String(), item.ID) {
		t.Fatalf("a held item was listed: %s", listed.Body)
	}
	if listed := gated.requestAsService(t, "GET", "/issues/items?tag=ops&include_held=true"); !strings.Contains(listed.Body.String(), item.ID) {
		t.Fatalf("include_held=true did not list the held item: %s", listed.Body)
	}
	if tags := gated.requestAsService(t, "GET", "/issues/items/tags"); !strings.Contains(tags.Body.String(), `"ops"`) {
		t.Fatalf("issue tags = %d %s", tags.Code, tags.Body)
	}

	project := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects", map[string]any{"name": "Inbox", "goal": "One inbox."})
	if project.Code != http.StatusCreated || !strings.Contains(project.Body.String(), `"id":"project_000001"`) {
		t.Fatalf("create project = %d %s", project.Code, project.Body)
	}
	step := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects/project_000001/next-steps", map[string]any{"title": "Merge the branch"})
	var stepNote struct {
		NoteID string `json:"note_id"`
	}
	if err := json.Unmarshal(step.Body.Bytes(), &stepNote); err != nil || step.Code != http.StatusCreated {
		t.Fatalf("next step = %d %s (%v)", step.Code, step.Body, err)
	}
	if todo, err := stores.issues.GetItem(stepNote.NoteID); err != nil || todo.Title != "Merge the branch" {
		t.Fatalf("the next step in issues is %+v (%v)", todo, err)
	}
	if links := gated.requestAsService(t, "GET", "/projects/links?entity_type=note&entity_ref="+stepNote.NoteID); !strings.Contains(links.Body.String(), "project_000001") {
		t.Fatalf("reverse lookup = %d %s", links.Code, links.Body)
	}
	if goal := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects/project_000001/goals", map[string]any{"statement": "Every chat in one list"}); goal.Code != http.StatusCreated || !strings.Contains(goal.Body.String(), `"id":"goal_000001"`) {
		t.Fatalf("create goal = %d %s", goal.Code, goal.Body)
	}
	if linked := gated.requestAsServiceWithJSON(t, "POST", "/projects/goals/goal_000001/links", map[string]any{"entity_type": "note", "entity_ref": stepNote.NoteID}); linked.Code != http.StatusCreated {
		t.Fatalf("link a goal to the next step = %d %s", linked.Code, linked.Body)
	}
	if missing := gated.requestAsService(t, "GET", "/projects/projects/project_0000001"); missing.Code != http.StatusNotFound {
		t.Fatalf("an id of the wrong form = %d %s, want 404", missing.Code, missing.Body)
	}
}

// All three hold records with no owner on a row, so only the operator reaches them.
func TestTaskTrackingRoutesAreTheOperators(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableTaskTrackingOnTestDatabase(t, gated.server)
	person := gated.loginAs(t, firstTestPrincipalID)
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	for _, path := range []string{"/notes/items", "/notes/search?q=x", "/issues/items", "/issues/items/search?q=x", "/projects/projects", "/projects/session-filings"} {
		if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401", path, response.Code)
		}
		if response := gated.requestAs(t, person, "GET", path, nil); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator_route") {
			t.Errorf("GET %s as a person who is not an administrator = %d %s, want 403 operator_route", path, response.Code, response.Body)
		}
		if response := gated.requestAs(t, administrator, "GET", path, nil); response.Code != http.StatusOK {
			t.Errorf("GET %s as an administrator = %d %s, want 200", path, response.Code, response.Body)
		}
	}
	administratorsSession := gated.createSessionAs(t, administrator, administratorTestPrincipalID)
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, administratorsSession), "GET", "/notes/tags"); response.Code != http.StatusOK {
		t.Errorf("an administrator's session agent token = %d %s, want 200", response.Code, response.Body)
	}
	personsSession := gated.createSessionAs(t, person, firstTestPrincipalID)
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, personsSession), "GET", "/notes/tags"); response.Code != http.StatusUnauthorized {
		t.Errorf("another person's session agent token = %d %s, want 401", response.Code, response.Body)
	}
}

// Copies of standalone databases, made with sqlite3 .backup, to check an
// import against the live stores:
//
//	go test ./internal/server -run TestImportedStandaloneTaskTrackingDatabases -args \
//	  -standalone-noteboard-copy=/tmp/noteboard.db [-standalone-project-store-copy=/tmp/project-store.db] \
//	  [-standalone-kanban-store-copy-for-routes=/tmp/kanban-store.db] \
//	  -task-tracking-paths=/tmp/paths.txt -task-tracking-output=/tmp/answers.json
//
// The test imports each copy into a fresh test database, the noteboard copy
// split between notes and issues as import-noteboard splits it, and writes
// what each GET path in the paths file (one a line) answers, for a
// comparison with the standalone stores' own answers at the same paths.
var (
	standaloneNoteboardCopy    = flag.String("standalone-noteboard-copy", "", "a copy of a standalone noteboard database to import")
	standaloneProjectStoreCopy = flag.String("standalone-project-store-copy", "", "a copy of a standalone project-store database to import")
	taskTrackingPaths          = flag.String("task-tracking-paths", "", "a file of GET paths, one a line, to ask the imported stores")
	taskTrackingOutput         = flag.String("task-tracking-output", "", "where to write the answers")
)

func TestImportedStandaloneTaskTrackingDatabases(t *testing.T) {
	if *standaloneNoteboardCopy == "" || *taskTrackingPaths == "" || *taskTrackingOutput == "" {
		t.Skip("no -standalone-noteboard-copy, -task-tracking-paths and -task-tracking-output given")
	}
	gated := newGatedTestServer(t, nil)
	notesModule, err := notes.Module()
	if err != nil {
		t.Fatal(err)
	}
	issuesModule, err := issuesdb.Module()
	if err != nil {
		t.Fatal(err)
	}
	projectsModule, err := projects.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, notesModule, issuesModule, projectsModule)
	if *standaloneKanbanStoreCopy != "" {
		// kanban-store first, as on landing day, so a revision of a purged
		// card follows the card's history into issues.
		kanbanCounts, err := issuesdb.ImportStandaloneDatabase(context.Background(), database.Open(issuesModule), *standaloneKanbanStoreCopy)
		if err != nil {
			t.Fatalf("kanban-store import: %v", err)
		}
		t.Logf("imported kanban-store %+v", kanbanCounts)
	}
	counts, err := noteboardsplit.ImportStandaloneDatabase(context.Background(), database.OpenAsAdministrator(),
		noteboardsplit.Modules{Notes: notesModule, Issues: issuesModule}, *standaloneNoteboardCopy)
	if err != nil {
		t.Fatalf("noteboard import: %v", err)
	}
	t.Logf("imported noteboard %+v", counts)
	projectsDB := database.Open(projectsModule)
	if *standaloneProjectStoreCopy != "" {
		projectCounts, err := projects.ImportStandaloneDatabase(context.Background(), projectsDB, *standaloneProjectStoreCopy)
		if err != nil {
			t.Fatalf("project-store import: %v", err)
		}
		t.Logf("imported project-store %+v", projectCounts)
	}
	notesStore, issuesStore := notes.New(database.Open(notesModule)), issueitems.New(database.Open(issuesModule))
	gated.server.EnableNotes(notesStore)
	gated.server.EnableIssueItems(issuesStore)
	gated.server.EnableProjects(&projects.Handlers{Store: projects.New(projectsDB), Owners: &projects.Owners{NotesAndIssues: projects.NotesAndIssuesModules{Notes: notesStore, Issues: issuesStore}}})

	paths, err := os.ReadFile(*taskTrackingPaths)
	if err != nil {
		t.Fatal(err)
	}
	answers := map[string]json.RawMessage{}
	for _, path := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
		response := gated.requestAsService(t, "GET", path)
		answers[path] = json.RawMessage(fmt.Sprintf(`{"status":%d,"body":%s}`, response.Code, jsonOrString(response.Body.Bytes())))
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*taskTrackingOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// jsonOrString is body when it is JSON, and body as a JSON string otherwise.
func jsonOrString(body []byte) []byte {
	if json.Valid(body) {
		return body
	}
	encoded, _ := json.Marshal(string(body))
	return encoded
}
