package items

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// standaloneDatabase writes a standalone noteboard's SQLite file holding the
// rows statements insert, in the encodings the standalone store wrote them.
func standaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noteboard.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, statement := range append([]string{standaloneSchema}, statements...) {
		if _, err := source.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

const (
	importedItemID = "6f1c2a4e-0b7d-4c55-9e1a-3d2b8f4a7c10"
	importedChild  = "a8e0d9f3-5b61-4f0e-8c2d-7e9b1a3c5d42"
)

var importedRows = []string{
	`INSERT INTO items VALUES ('` + importedItemID + `', 'todo', 'Water the plants', 'every **third** Tuesday', '["home","personal"]', 2, 1.5, 'open', 'chores',
		'2026-08-01T12:00:00-07:00', NULL, '["https://example.com/plants"]', 'agent', '2026-06-26 05:25:39.96776608+00:00', '2026-09-29 15:44:13.210533898+00:00',
		NULL, '{"dtstart":"2026-07-21T09:00:00-07:00","tzid":"America/Los_Angeles","rrule":"FREQ=MONTHLY;BYDAY=3TU","mode":"instance","last_materialized_at":"2026-07-21T09:00:00-07:00"}',
		'2026-07-18 22:23:20.067418863+00:00', 'waiting on the operator', 0)`,
	`INSERT INTO items VALUES ('` + importedChild + `', 'note', 'Deleted child', '', '[]', 0, 0, 'archived', '',
		'2026-08-02T09:30:00Z', '` + importedItemID + `', '[]', '', '2026-06-28 14:29:25.314249912+00:00', '2026-06-28 14:29:25.314249912+00:00',
		'2026-07-01 08:00:00+00:00', NULL, NULL, '', NULL)`,
	`INSERT INTO item_revisions VALUES (4, '` + importedItemID + `', 'Water plants', 'old body', '["home"]', 'open', 1, '', NULL, '[]', NULL, 'update', '2026-07-01 10:00:00.123456789+00:00')`,
	`INSERT INTO item_revisions VALUES (9, '` + importedChild + `', 'Deleted child', '', '[]', 'archived', 0, '', '` + importedItemID + `', '[]', NULL, 'delete', '2026-07-01 08:00:00+00:00')`,
	// The standalone store had handed out revision 12 before the rows above
	// were all that was left.
	`UPDATE sqlite_sequence SET seq = 12 WHERE name = 'item_revisions'`,
}

func TestImportCopiesEveryItemAndRevisionAsItWas(t *testing.T) {
	path := standaloneDatabase(t, importedRows...)
	store := newTestStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, path)
	if err != nil {
		t.Fatalf("ImportStandaloneDatabase: %v", err)
	}
	want := ImportCounts{Items: 2, DeletedItems: 1, HeldItems: 1, ScheduledItems: 1, Revisions: 2, LastRevisionID: 9}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}

	item, err := store.GetItem(importedItemID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.Title != "Water the plants" || item.Body != "every **third** Tuesday" || item.Priority != 2 || item.Rank != 1.5 ||
		item.ListID != "chores" || item.CreatedBy != "agent" || item.HoldReason != "waiting on the operator" {
		t.Errorf("item fields came across as %+v", item)
	}
	if strings.Join(item.Tags, ",") != "home,personal" || strings.Join(item.Links, ",") != "https://example.com/plants" {
		t.Errorf("tags %v and links %v", item.Tags, item.Links)
	}
	if got := item.DueAt.Format(time.RFC3339); got != "2026-08-01T12:00:00-07:00" {
		t.Errorf("due_at reads back as %s, want the offset it was written with", got)
	}
	if got := item.CreatedAt.Format(time.RFC3339Nano); got != "2026-06-26T05:25:39.967766Z" {
		t.Errorf("created_at reads back as %s, want it cut to the microsecond", got)
	}
	if item.HeldAt == nil || item.AutoHoldAtUSD == nil || *item.AutoHoldAtUSD != 0 {
		t.Errorf("held_at %v and auto_hold_at_usd %v: a zero ceiling is a ceiling", item.HeldAt, item.AutoHoldAtUSD)
	}
	if item.Schedule == nil || item.Schedule.RRule != "FREQ=MONTHLY;BYDAY=3TU" || item.Schedule.TZID != "America/Los_Angeles" {
		t.Errorf("schedule came across as %+v", item.Schedule)
	}

	if _, err := store.GetItem(importedChild); err == nil {
		t.Error("a soft-deleted item came back live")
	}
	child, err := store.GetItemIncludingDeleted(importedChild)
	if err != nil {
		t.Fatalf("GetItemIncludingDeleted: %v", err)
	}
	if child.DeletedAt == nil || child.ParentID == nil || *child.ParentID != importedItemID || child.DueAt.Format(time.RFC3339) != "2026-08-02T09:30:00Z" {
		t.Errorf("deleted child came across as %+v", child)
	}

	revisions, err := store.ListRevisions(importedItemID, 0, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 1 || revisions[0].ID != 4 || revisions[0].Body != "old body" || revisions[0].Reason != "update" {
		t.Errorf("revisions = %+v", revisions)
	}

	// The next revision takes the number after the last one handed out.
	title := "Water the plants weekly"
	if _, err := store.UpdateItem(importedItemID, &model.UpdateItemRequest{Title: &title}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	revisions, err = store.ListRevisions(importedItemID, 1, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if revisions[0].ID != 13 {
		t.Errorf("the first revision after the import is %d, want 13", revisions[0].ID)
	}

	if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil {
		t.Error("a second import into a module that holds rows was taken")
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	cases := map[string][]string{
		"an unknown column": {`ALTER TABLE items ADD COLUMN mood TEXT`},
		"tags that are not a list of strings": {`INSERT INTO items (id, type, title, tags, created_at, updated_at)
			VALUES ('x', 'todo', 'bad tags', '[1, 2]', '2026-06-26 05:25:39+00:00', '2026-06-26 05:25:39+00:00')`},
		"a schedule the store cannot read": {`INSERT INTO items (id, type, title, schedule, created_at, updated_at)
			VALUES ('x', 'todo', 'bad rule', '{"tzid": 5}', '2026-06-26 05:25:39+00:00', '2026-06-26 05:25:39+00:00')`},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			path := standaloneDatabase(t, statements...)
			store := newTestStore(t)
			if _, err := ImportStandaloneDatabase(context.Background(), store.db, path); err == nil {
				t.Fatal("the import took a file it cannot copy whole")
			}
			if count, err := store.ItemCount(); err != nil || count != 0 {
				t.Errorf("a refused import left %d items (%v)", count, err)
			}
		})
	}
}

func TestImportNamesTextThatHoldsANulCharacter(t *testing.T) {
	path := standaloneDatabase(t, `INSERT INTO items (id, type, title, body, created_at, updated_at)
		VALUES ('x', 'note', 'NUL test', 'before' || char(0) || 'after', '2026-06-26 05:25:39+00:00', '2026-06-26 05:25:39+00:00')`)
	store := newTestStore(t)
	_, err := ImportStandaloneDatabase(context.Background(), store.db, path)
	if !errors.Is(err, ErrTextHoldsNulCharacter) || !strings.Contains(err.Error(), "item x") || !strings.Contains(err.Error(), "body") {
		t.Fatalf("import of a body holding NUL = %v, want ErrTextHoldsNulCharacter naming item x and body", err)
	}
}
