package noteboardsplit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issueitems"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The ids of the install the move test builds. They are made up.
const (
	noteID               = "10000000-0000-4000-8000-000000000001"
	rankEntryID          = "10000000-0000-4000-8000-000000000002"
	workspaceID          = "10000000-0000-4000-8000-000000000003"
	noteUnderTodoID      = "10000000-0000-4000-8000-000000000004"
	todoID               = "20000000-0000-4000-8000-000000000001"
	heldTodoID           = "20000000-0000-4000-8000-000000000002"
	childTodoID          = "20000000-0000-4000-8000-000000000003"
	deletedTodoID        = "20000000-0000-4000-8000-000000000004"
	scheduledTodoID      = "20000000-0000-4000-8000-000000000005"
	purgedCardID         = "30000000-0000-4000-8000-000000000001"
	purgedUnknownItemID  = "30000000-0000-4000-8000-000000000002"
	revisionOfNote       = 41
	revisionOfTodo       = 42
	revisionOfHeldTodo   = 43
	revisionOfPurgedCard = 44
	revisionOfPurgedItem = 45
	revisionOfWorkspace  = 46
)

// installBeforeTheSplit is an install made before issues owned todos: a
// noteboard module holding notes, todos and their revisions, and a kanban
// module at its first migration only, holding a board with a card. Both are
// under their names of before, with roles of the test's own, because a role
// belongs to the whole Postgres server and the shared test server holds the
// real names already.
func installBeforeTheSplit(t *testing.T) (*sql.DB, Modules, *postgrestest.Database) {
	t.Helper()
	ctx := context.Background()
	notesModule, err := notes.Module()
	if err != nil {
		t.Fatal(err)
	}
	issuesModule, err := issuesdb.Module()
	if err != nil {
		t.Fatal(err)
	}
	suffix := randomSuffix(t)
	noteboardBefore := postgresstorage.Module{Schema: "noteboard", Role: "noteboard_" + suffix, Migrations: notesModule.Migrations}
	kanbanBefore := postgresstorage.Module{Schema: "kanban", Role: "kanban_" + suffix, Migrations: issuesModule.Migrations[:1]}
	notesAfter := notesModule
	notesAfter.Role = "notes_" + suffix
	notesAfter.FormerNames = []postgresstorage.FormerName{{Schema: "noteboard", Role: noteboardBefore.Role}}
	issuesAfter := issuesModule
	issuesAfter.Role = "issues_" + suffix
	issuesAfter.FormerNames = []postgresstorage.FormerName{{Schema: "kanban", Role: kanbanBefore.Role}}
	dropRolesAtEnd(t, noteboardBefore.Role, kanbanBefore.Role, notesAfter.Role, issuesAfter.Role)

	database := postgrestest.NewDatabase(t)
	administrator := database.OpenAsAdministrator()
	for _, module := range []postgresstorage.Module{noteboardBefore, kanbanBefore} {
		if _, err := postgresstorage.Provision(ctx, administrator, module); err != nil {
			t.Fatal(err)
		}
		pool := database.Open(module)
		if _, err := postgresstorage.Migrate(ctx, pool, module); err != nil {
			t.Fatal(err)
		}
		pool.Close()
	}
	statements := []string{
		`INSERT INTO noteboard.items (id, type, title, body, tags, priority, rank, status, list_id, due_at, due_at_offset_seconds, parent_id, links, created_by, created_at, updated_at, deleted_at, schedule, held_at, hold_reason, auto_hold_at_usd) VALUES
			('` + noteID + `', 'note', 'Deploy playbook', 'steps', '["ops"]', 0, 0, 'open', '', NULL, NULL, NULL, '[]', 'agent', '2026-06-01 10:00:00.123456+00', '2026-06-02 10:00:00+00', NULL, NULL, NULL, '', NULL),
			('` + rankEntryID + `', 'rank', 'A song', '', '[]', 0, 2.5, 'open', 'songs', NULL, NULL, NULL, '["` + todoID + `"]', '', '2026-06-01 11:00:00+00', '2026-06-01 11:00:00+00', NULL, NULL, NULL, '', NULL),
			('` + workspaceID + `', 'workspace', 'Digest memory', 'remembered', '[]', 0, 0, 'open', '', NULL, NULL, NULL, '[]', 'scheduler', '2026-06-01 12:00:00+00', '2026-06-03 12:00:00+00', NULL, NULL, NULL, '', NULL),
			('` + noteUnderTodoID + `', 'note', 'Findings', '', '[]', 0, 0, 'open', '', NULL, NULL, '` + todoID + `', '[]', '', '2026-06-04 12:00:00+00', '2026-06-04 12:00:00+00', NULL, NULL, NULL, '', NULL),
			('` + todoID + `', 'todo', 'Call the plumber', 'about the sink', '["home"]', 2, 0, 'open', '', '2026-08-01 19:00:00+00', -25200, NULL, '["https://example.com"]', 'agent', '2026-06-05 09:00:00+00', '2026-06-06 09:00:00.654321+00', NULL, NULL, NULL, '', 5),
			('` + heldTodoID + `', 'todo', 'Parked work', '', '[]', 1, 0, 'open', '', NULL, NULL, NULL, '[]', '', '2026-06-05 10:00:00+00', '2026-06-05 10:00:00+00', NULL, NULL, '2026-06-05 10:30:00+00', 'waiting', 0),
			('` + childTodoID + `', 'todo', 'Under the parked work', '', '[]', 0, 0, 'open', '', NULL, NULL, '` + heldTodoID + `', '[]', '', '2026-06-05 11:00:00+00', '2026-06-05 11:00:00+00', NULL, NULL, NULL, '', NULL),
			('` + deletedTodoID + `', 'todo', 'Taken away', '', '[]', 0, 0, 'done', '', NULL, NULL, NULL, '[]', '', '2026-06-05 12:00:00+00', '2026-06-07 12:00:00+00', '2026-06-07 12:00:00+00', NULL, NULL, '', NULL),
			('` + scheduledTodoID + `', 'todo', 'Water the plants', '', '["personal"]', 0, 0, 'open', '', '2026-07-21 16:00:00+00', -25200, NULL, '[]', '', '2026-06-05 13:00:00+00', '2026-06-05 13:00:00+00', NULL,
				'{"dtstart":"2026-07-21T09:00:00-07:00","tzid":"America/Los_Angeles","rrule":"FREQ=MONTHLY;BYDAY=3TU","mode":"instance"}', NULL, '', NULL)`,
		`INSERT INTO noteboard.item_revisions (id, item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at) OVERRIDING SYSTEM VALUE VALUES
			(` + itoa(revisionOfNote) + `, '` + noteID + `', 'Deploy', 'old steps', '[]', 'open', 0, '', NULL, '[]', NULL, 'update', '2026-06-02 10:00:00+00'),
			(` + itoa(revisionOfTodo) + `, '` + todoID + `', 'Call plumber', 'old', '["home"]', 'open', 1, '', NULL, '[]', NULL, 'update', '2026-06-06 09:00:00+00'),
			(` + itoa(revisionOfHeldTodo) + `, '` + heldTodoID + `', 'Parked work', '', '[]', 'open', 1, '', NULL, '[]', NULL, 'hold', '2026-06-05 10:30:00+00'),
			(` + itoa(revisionOfPurgedCard) + `, '` + purgedCardID + `', 'A purged card', 'gone', '[]', 'open', 0, '', NULL, '[]', NULL, 'purge', '2026-06-08 10:00:00+00'),
			(` + itoa(revisionOfPurgedItem) + `, '` + purgedUnknownItemID + `', 'A purged note, perhaps', 'gone', '[]', 'open', 0, '', NULL, '[]', NULL, 'purge', '2026-06-08 11:00:00+00'),
			(` + itoa(revisionOfWorkspace) + `, '` + workspaceID + `', 'Digest memory', 'older memory', '[]', 'open', 0, '', NULL, '[]', NULL, 'update', '2026-06-03 12:00:00+00')`,
		// The standalone store had handed out revision 60 before these were all that was left.
		`SELECT setval('noteboard.item_revisions_id_seq', 60, true)`,
		`INSERT INTO kanban.boards (id, name, created_at, updated_at) VALUES ('board-1', 'Work', now(), now())`,
		`INSERT INTO kanban.columns (id, board_id, name, position, created_at, updated_at) VALUES ('column-1', 'board-1', 'Doing', 1, now(), now())`,
		`INSERT INTO kanban.placements (card_id, board_id, column_id, position, created_at, updated_at) VALUES ('` + todoID + `', 'board-1', 'column-1', 1, now(), now())`,
		`INSERT INTO kanban.card_events (id, card_id, kind, clock_state, occurred_at, recorded_at) VALUES
			('event-1', '` + todoID + `', 'card_created', 'running', now(), now()),
			('event-2', '` + purgedCardID + `', 'card_created', 'running', now(), now())`,
	}
	for _, statement := range statements {
		if _, err := administrator.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	// The start of a binary that knows the split: both renamed, both
	// migrated (issues gains its items tables), in the order the server
	// takes them.
	for _, module := range []postgresstorage.Module{notesAfter, issuesAfter} {
		report, err := postgresstorage.Provision(ctx, administrator, module)
		if err != nil {
			t.Fatal(err)
		}
		if report.RenamedFrom == nil {
			t.Fatalf("module %s was not renamed from its name before", module.Schema)
		}
		pool := database.Open(module)
		if _, err := postgresstorage.Migrate(ctx, pool, module); err != nil {
			t.Fatal(err)
		}
		pool.Close()
	}
	return administrator, Modules{Notes: notesAfter, Issues: issuesAfter}, database
}

func TestTheFirstStartMovesEveryTodoIntoIssuesWithNothingLostOrDuplicated(t *testing.T) {
	ctx := context.Background()
	administrator, modules, database := installBeforeTheSplit(t)

	before := rowsBySchema(t, administrator, "notes")
	moved, err := MoveTodosOutOfNotes(ctx, administrator, modules)
	if err != nil {
		t.Fatalf("MoveTodosOutOfNotes: %v", err)
	}
	if want := (MoveCounts{Issues: 5, Revisions: 3, RevisionsOfPurgedCards: 1}); moved != want {
		t.Errorf("moved %+v, want %+v", moved, want)
	}
	notesAfter := rowsBySchema(t, administrator, "notes")
	issuesAfter := rowsBySchema(t, administrator, "issues")

	// Every row is in exactly one module, unchanged.
	for _, table := range []string{"items", "item_revisions"} {
		for key, row := range before[table] {
			inNotes, notesHas := notesAfter[table][key]
			inIssues, issuesHas := issuesAfter[table][key]
			switch {
			case notesHas && issuesHas:
				t.Errorf("%s %s is in both modules", table, key)
			case !notesHas && !issuesHas:
				t.Errorf("%s %s was lost", table, key)
			case notesHas && inNotes != row, issuesHas && inIssues != row:
				t.Errorf("%s %s changed on the way:\n before %s\n notes  %s\n issues %s", table, key, row, inNotes, inIssues)
			}
		}
		if total := len(notesAfter[table]) + len(issuesAfter[table]); total != len(before[table]) {
			t.Errorf("%s: %d rows before, %d after", table, len(before[table]), total)
		}
	}
	wantInIssues := map[string][]string{
		"items":          {todoID, heldTodoID, childTodoID, deletedTodoID, scheduledTodoID},
		"item_revisions": {itoa(revisionOfTodo), itoa(revisionOfHeldTodo), itoa(revisionOfPurgedCard)},
	}
	for table, keys := range wantInIssues {
		if got := sortedKeys(issuesAfter[table]); !equalStrings(got, sorted(keys)) {
			t.Errorf("issues.%s holds %v, want %v", table, got, sorted(keys))
		}
	}
	if got, want := sortedKeys(notesAfter["item_revisions"]), sorted([]string{itoa(revisionOfNote), itoa(revisionOfPurgedItem), itoa(revisionOfWorkspace)}); !equalStrings(got, want) {
		t.Errorf("notes.item_revisions holds %v, want %v", got, want)
	}

	// The stores read what moved as it was, and the gates still hold.
	issueStore := issueitems.New(database.Open(modules.Issues))
	noteStore := notes.New(database.Open(modules.Notes))
	todo, err := issueStore.GetItem(todoID)
	if err != nil || todo.Title != "Call the plumber" || todo.DueAt.Format("2006-01-02T15:04:05-07:00") != "2026-08-01T12:00:00-07:00" || todo.AutoHoldAtUSD == nil || *todo.AutoHoldAtUSD != 5 {
		t.Fatalf("the moved todo reads %+v (%v)", todo, err)
	}
	revisions, err := issueStore.ListRevisions(todoID, 0, 0)
	if err != nil || len(revisions) != 1 || revisions[0].ID != revisionOfTodo {
		t.Errorf("the moved todo's revisions are %+v (%v)", revisions, err)
	}
	listed, err := issueStore.ListItems(items.ListParams{Type: model.TypeTodo, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(listed); !equalStrings(got, sorted([]string{todoID, scheduledTodoID})) {
		t.Errorf("open issues listed %v: the held one and the one under it stay hidden", got)
	}
	scheduled, err := issueStore.GetItem(scheduledTodoID)
	if err != nil || scheduled.Schedule == nil {
		t.Fatalf("the moved scheduled todo reads %+v (%v)", scheduled, err)
	}
	from, _ := time.Parse(time.RFC3339, "2026-07-01T00:00:00-07:00")
	to, _ := time.Parse(time.RFC3339, "2026-10-01T00:00:00-07:00")
	if occurrences, err := scheduled.Schedule.DueOccurrences(scheduled.DueAt, from, to); err != nil || len(occurrences) != 3 {
		t.Errorf("the moved rule falls due %d times in three months (%v), want 3", len(occurrences), err)
	}
	if _, err := noteStore.GetItem(todoID); !errors.Is(err, items.ErrItemNotFound) {
		t.Errorf("notes still answers the moved todo: %v", err)
	}
	if _, err := noteStore.CreateItem(&model.CreateItemRequest{Type: model.TypeTodo, Title: "A new todo"}); !errors.Is(err, items.ErrTypeNotKept) {
		t.Errorf("notes took a new todo: %v", err)
	}

	// Revision numbers go on after the highest either module handed out.
	title := "Call the plumber today"
	if _, err := issueStore.UpdateItem(todoID, &model.UpdateItemRequest{Title: &title}); err != nil {
		t.Fatal(err)
	}
	body := "new steps"
	if _, err := noteStore.UpdateItem(noteID, &model.UpdateItemRequest{Body: &body}); err != nil {
		t.Fatal(err)
	}
	newest, err := issueStore.ListRevisions(todoID, 1, 0)
	if err != nil || newest[0].ID != 61 {
		t.Errorf("the next issue revision is %+v (%v), want 61", newest, err)
	}
	newestNote, err := noteStore.ListRevisions(noteID, 1, 0)
	if err != nil || newestNote[0].ID != 61 {
		t.Errorf("the next note revision is %+v (%v), want 61", newestNote, err)
	}

	// A second start moves nothing.
	again, err := MoveTodosOutOfNotes(ctx, administrator, modules)
	if err != nil || again != (MoveCounts{}) {
		t.Errorf("the second start moved %+v (%v)", again, err)
	}
}

func TestTheMoveRefusesATodoIssuesAlreadyHoldsAndMovesNothing(t *testing.T) {
	ctx := context.Background()
	administrator, modules, _ := installBeforeTheSplit(t)
	if _, err := administrator.ExecContext(ctx, `INSERT INTO issues.items (id, type, title, created_at, updated_at) VALUES ($1, 'todo', 'Another copy', now(), now())`, todoID); err != nil {
		t.Fatal(err)
	}
	before := rowsBySchema(t, administrator, "notes")
	if _, err := MoveTodosOutOfNotes(ctx, administrator, modules); err == nil {
		t.Fatal("the move took a todo issues already holds")
	}
	after := rowsBySchema(t, administrator, "notes")
	for _, table := range []string{"items", "item_revisions"} {
		if len(after[table]) != len(before[table]) {
			t.Errorf("a refused move changed notes.%s from %d rows to %d", table, len(before[table]), len(after[table]))
		}
	}
}

// rowsBySchema is every row of a schema's items and item_revisions tables as
// JSON, by id, the generated search document aside.
func rowsBySchema(t *testing.T, db *sql.DB, schema string) map[string]map[string]string {
	t.Helper()
	tables := map[string]map[string]string{}
	for _, table := range []string{"items", "item_revisions"} {
		rows, err := db.Query(`SELECT r.id::text, (to_jsonb(r) - 'search_document')::text FROM ` + schema + `.` + table + ` r`)
		if err != nil {
			t.Fatal(err)
		}
		tables[table] = map[string]string{}
		for rows.Next() {
			var id, row string
			if err := rows.Scan(&id, &row); err != nil {
				t.Fatal(err)
			}
			tables[table][id] = row
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return tables
}

func sortedKeys(rows map[string]string) []string {
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func idsOf(listed []*model.Item) []string {
	ids := make([]string, 0, len(listed))
	for _, item := range listed {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func itoa(number int) string {
	digits := []byte{}
	for number > 0 {
		digits = append([]byte{byte('0' + number%10)}, digits...)
		number /= 10
	}
	return string(digits)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

// dropRolesAtEnd is registered before the test's database, so it runs after
// the database is gone: a role cannot be dropped while it owns a schema there.
func dropRolesAtEnd(t *testing.T, roles ...string) {
	t.Cleanup(func() {
		server, err := sql.Open("pgx", os.Getenv(postgrestest.AdministratorURLVariable))
		if err != nil {
			t.Errorf("cleaning up roles: %v", err)
			return
		}
		defer server.Close()
		for _, role := range roles {
			if _, err := server.Exec("DROP ROLE IF EXISTS " + role); err != nil {
				t.Errorf("dropping role %s: %v", role, err)
			}
		}
	})
}
