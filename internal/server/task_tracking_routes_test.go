package server

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enableTaskTrackingOnTestDatabase opens noteboard and project-store on a
// Postgres database of the test's own and serves them from server, with
// project-store calling noteboard in process, as main wires them.
func enableTaskTrackingOnTestDatabase(t *testing.T, server *Server) (*items.Store, *projects.Store) {
	t.Helper()
	noteboardModule, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	projectsModule, err := projects.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, noteboardModule, projectsModule)
	noteboard := items.New(database.Open(noteboardModule))
	projectStore := projects.New(database.Open(projectsModule))
	server.EnableNoteboard(noteboard)
	server.EnableProjects(&projects.Handlers{Store: projectStore, Owners: &projects.Owners{Noteboard: projects.NoteboardModule{Store: noteboard}}})
	return noteboard, projectStore
}

func TestTaskTrackingRoutesAnswer503WithNoPostgres(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for path, code := range map[string]string{"/noteboard/api/items": "noteboard_off", "/projects/projects": "project_store_off"} {
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
	noteboard, _ := enableTaskTrackingOnTestDatabase(t, gated.server)

	created := gated.requestAsServiceWithJSON(t, "POST", "/noteboard/api/items", map[string]any{"type": "todo", "title": "Run deploy.sh", "tags": []string{"ops"}})
	var item struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s (%v)", created.Code, created.Body, err)
	}
	if read := gated.requestAsService(t, "GET", "/noteboard/api/items/"+item.ID); read.Code != http.StatusOK || read.Header().Get("ETag") != `"`+item.UpdatedAt+`"` {
		t.Fatalf("read = %d, ETag %q, want the create's updated_at %q", read.Code, read.Header().Get("ETag"), item.UpdatedAt)
	}
	if found := gated.requestAsService(t, "GET", "/noteboard/api/search?q=deploy"); !strings.Contains(found.Body.String(), item.ID) {
		t.Fatalf("search = %d %s", found.Code, found.Body)
	}
	if held := gated.requestAsServiceWithJSON(t, "POST", "/noteboard/api/items/"+item.ID+"/hold", map[string]any{"reason": "parked"}); held.Code != http.StatusOK {
		t.Fatalf("hold = %d %s", held.Code, held.Body)
	}
	if listed := gated.requestAsService(t, "GET", "/noteboard/api/items?tag=ops"); strings.Contains(listed.Body.String(), item.ID) {
		t.Fatalf("a held item was listed: %s", listed.Body)
	}
	if listed := gated.requestAsService(t, "GET", "/noteboard/api/items?tag=ops&include_held=true"); !strings.Contains(listed.Body.String(), item.ID) {
		t.Fatalf("include_held=true did not list the held item: %s", listed.Body)
	}

	project := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects", map[string]any{"name": "Inbox", "goal": "One inbox."})
	if project.Code != http.StatusCreated || !strings.Contains(project.Body.String(), `"id":"project_000001"`) {
		t.Fatalf("create project = %d %s", project.Code, project.Body)
	}
	step := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects/project_000001/next-steps", map[string]any{"title": "Merge the branch"})
	var note struct {
		NoteID string `json:"note_id"`
	}
	if err := json.Unmarshal(step.Body.Bytes(), &note); err != nil || step.Code != http.StatusCreated {
		t.Fatalf("next step = %d %s (%v)", step.Code, step.Body, err)
	}
	if todo, err := noteboard.GetItem(note.NoteID); err != nil || todo.Title != "Merge the branch" {
		t.Fatalf("the next step in noteboard is %+v (%v)", todo, err)
	}
	if links := gated.requestAsService(t, "GET", "/projects/links?entity_type=note&entity_ref="+note.NoteID); !strings.Contains(links.Body.String(), "project_000001") {
		t.Fatalf("reverse lookup = %d %s", links.Code, links.Body)
	}
	if goal := gated.requestAsServiceWithJSON(t, "POST", "/projects/projects/project_000001/goals", map[string]any{"statement": "Every chat in one list"}); goal.Code != http.StatusCreated || !strings.Contains(goal.Body.String(), `"id":"goal_000001"`) {
		t.Fatalf("create goal = %d %s", goal.Code, goal.Body)
	}
	if linked := gated.requestAsServiceWithJSON(t, "POST", "/projects/goals/goal_000001/links", map[string]any{"entity_type": "note", "entity_ref": note.NoteID}); linked.Code != http.StatusCreated {
		t.Fatalf("link a goal to the next step = %d %s", linked.Code, linked.Body)
	}
	if missing := gated.requestAsService(t, "GET", "/projects/projects/project_0000001"); missing.Code != http.StatusNotFound {
		t.Fatalf("an id of the wrong form = %d %s, want 404", missing.Code, missing.Body)
	}
}

// Both hold records with no owner on a row, so only the operator reaches them.
func TestTaskTrackingRoutesAreTheOperators(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableTaskTrackingOnTestDatabase(t, gated.server)
	person := gated.loginAs(t, firstTestPrincipalID)
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	for _, path := range []string{"/noteboard/api/items", "/noteboard/api/search?q=x", "/projects/projects", "/projects/session-filings"} {
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
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, administratorsSession), "GET", "/noteboard/api/tags"); response.Code != http.StatusOK {
		t.Errorf("an administrator's session agent token = %d %s, want 200", response.Code, response.Body)
	}
	personsSession := gated.createSessionAs(t, person, firstTestPrincipalID)
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, personsSession), "GET", "/noteboard/api/tags"); response.Code != http.StatusUnauthorized {
		t.Errorf("another person's session agent token = %d %s, want 401", response.Code, response.Body)
	}
}

// Copies of standalone databases, made with sqlite3 .backup, to check an
// import against the live stores:
//
//	go test ./internal/server -run TestImportedStandaloneTaskTrackingDatabases -args \
//	  -standalone-noteboard-copy=/tmp/noteboard.db -standalone-project-store-copy=/tmp/project-store.db \
//	  -task-tracking-paths=/tmp/paths.txt -task-tracking-output=/tmp/answers.json
//
// The test imports each copy into a fresh test database and writes what each
// GET path in the paths file (one a line) answers, for a comparison with the
// standalone stores' own answers at the same paths.
var (
	standaloneNoteboardCopy    = flag.String("standalone-noteboard-copy", "", "a copy of a standalone noteboard database to import")
	standaloneProjectStoreCopy = flag.String("standalone-project-store-copy", "", "a copy of a standalone project-store database to import")
	taskTrackingPaths          = flag.String("task-tracking-paths", "", "a file of GET paths, one a line, to ask the imported stores")
	taskTrackingOutput         = flag.String("task-tracking-output", "", "where to write the answers")
)

func TestImportedStandaloneTaskTrackingDatabases(t *testing.T) {
	if *standaloneNoteboardCopy == "" || *standaloneProjectStoreCopy == "" || *taskTrackingPaths == "" || *taskTrackingOutput == "" {
		t.Skip("no -standalone-noteboard-copy, -standalone-project-store-copy, -task-tracking-paths and -task-tracking-output given")
	}
	gated := newGatedTestServer(t, nil)
	noteboardModule, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	projectsModule, err := projects.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, noteboardModule, projectsModule)
	noteboardDB, projectsDB := database.Open(noteboardModule), database.Open(projectsModule)
	noteboardCounts, err := items.ImportStandaloneDatabase(context.Background(), noteboardDB, *standaloneNoteboardCopy)
	if err != nil {
		t.Fatalf("noteboard import: %v", err)
	}
	projectCounts, err := projects.ImportStandaloneDatabase(context.Background(), projectsDB, *standaloneProjectStoreCopy)
	if err != nil {
		t.Fatalf("project-store import: %v", err)
	}
	t.Logf("imported noteboard %+v and project-store %+v", noteboardCounts, projectCounts)
	noteboard := items.New(noteboardDB)
	gated.server.EnableNoteboard(noteboard)
	gated.server.EnableProjects(&projects.Handlers{Store: projects.New(projectsDB), Owners: &projects.Owners{Noteboard: projects.NoteboardModule{Store: noteboard}}})

	paths, err := os.ReadFile(*taskTrackingPaths)
	if err != nil {
		t.Fatal(err)
	}
	answers := map[string]json.RawMessage{}
	for _, path := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
		response := gated.requestAsService(t, "GET", path)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, response.Code, response.Body)
		}
		answers[path] = json.RawMessage(response.Body.Bytes())
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*taskTrackingOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
