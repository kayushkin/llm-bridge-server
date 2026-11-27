package journal

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"

	_ "modernc.org/sqlite"
)

// newStandaloneDatabase writes a made-up standalone journal-store file: the
// live standalone schema, then rows. Each statement runs in order.
func newStandaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "journal-store.db")
	database, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(string(schema)); err != nil {
		t.Fatalf("applying the standalone schema: %v", err)
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("writing the standalone rows: %v\n%s", err, statement)
		}
	}
	return path
}

// standaloneRows is a small journal: two entries (the second deleted), a
// revision of each, mentions, a timeline with one item, its revision and its
// mentions. The numbers have gaps, as a long-lived store's revisions may.
var standaloneRows = []string{
	`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
	 VALUES ('entry_000001', 1, 'journal', '', 'Tea with [Carol](ref:person_000012) in Lisbon', 1700000000, 'private', 0, '', '["travel"]', '', '', '', '', 1700000000, 1700000100, 0)`,
	`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
	 VALUES ('entry_000003', 3, 'note', 'Groceries', 'bread', 1700000200, 'private', 0, '', '[]', '', 'samsung-notes', 'a.txt', 'h1', 1700000200, 1700000300, 1700000300)`,
	`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
	 VALUES ('entry_000004', 4, 'timeline', 'A trip', '', 1700000400, 'published', 1700000400, 'a-trip', '[]', '', '', '', '', 1700000400, 1700000400, 0)`,
	`INSERT INTO entry_revisions (revision_id, entry_id, reason, replaced_at, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
	 VALUES (5, 'entry_000001', 'update', 1700000100, 'journal', '', 'Tea', 1700000000, 'private', 0, '', '[]', '', '', '', '', 1700000000, 1700000000, 0)`,
	`INSERT INTO entry_revisions (revision_id, entry_id, reason, replaced_at, kind, title, body, written_at, visibility, published_at, slug, tags, about_ref, source, source_ref, source_hash, created_at, updated_at, deleted_at)
	 VALUES (9, 'entry_000003', 'delete', 1700000300, 'note', 'Groceries', 'bread', 1700000200, 'private', 0, '', '[]', '', 'samsung-notes', 'a.txt', 'h1', 1700000200, 1700000200, 0)`,
	`INSERT INTO mentions (entry_id, position, line, entity_ref, entity_type, label) VALUES ('entry_000001', 0, 1, 'person_000012', 'person', 'Carol')`,
	`INSERT INTO timeline_items (id, seq, timeline_id, occurred_at, ends_at, precision, title, body, sources, created_at, updated_at, deleted_at)
	 VALUES ('timeline_item_000002', 2, 'entry_000004', 1700000500, 0, 'day', 'Landed', 'met [Bob](ref:person_000013)', '["entry_000001"]', 1700000500, 1700000600, 0)`,
	`INSERT INTO timeline_item_revisions (revision_id, item_id, reason, replaced_at, timeline_id, occurred_at, ends_at, precision, title, body, sources, created_at, updated_at, deleted_at)
	 VALUES (7, 'timeline_item_000002', 'update', 1700000600, 'entry_000004', 1700000500, 0, 'day', 'Left', '', '[]', 1700000500, 1700000500, 0)`,
	`INSERT INTO timeline_item_mentions (item_id, position, entity_ref, entity_type, label) VALUES ('timeline_item_000002', 0, 'person_000013', '', 'Bob')`,
	`INSERT INTO timeline_item_mentions (item_id, position, entity_ref, entity_type, label) VALUES ('timeline_item_000002', 1, 'entry_000001', 'entry', '')`,
}

func TestImportCopiesAStandaloneJournalWithEveryIDAndOneOwner(t *testing.T) {
	ctx := context.Background()
	service := newTestService(t)
	path := newStandaloneDatabase(t, standaloneRows...)

	counts, err := ImportStandaloneDatabase(ctx, service.store.db, path, alicePrincipalID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Entries: 3, EntryRevisions: 2, Mentions: 1, TimelineItems: 1, TimelineItemRevisions: 1, TimelineItemMentions: 2}
	if counts != want {
		t.Fatalf("counts = %s, want %s", counts.Summary(), want.Summary())
	}
	if counts.Summary() != "entries=3 entry_revisions=2 mentions=1 timeline_items=1 timeline_item_revisions=1 timeline_item_mentions=2" {
		t.Errorf("summary = %q", counts.Summary())
	}

	// Every row is Alice's, kept under its own id.
	asAlice := ownership.OwnedBy(alicePrincipalID)
	entry, err := service.store.Get(ctx, asAlice, "entry_000001", false)
	if err != nil {
		t.Fatalf("entry_000001 after the import: %v", err)
	}
	if entry.OwnerPrincipalID != alicePrincipalID || entry.WrittenAt != 1700000000 || entry.UpdatedAt != 1700000100 || entry.Tags[0] != "travel" {
		t.Errorf("entry_000001 = owner %q written %d updated %d tags %v", entry.OwnerPrincipalID, entry.WrittenAt, entry.UpdatedAt, entry.Tags)
	}
	if len(entry.Mentions) != 1 || entry.Mentions[0].EntityRef != "person_000012" {
		t.Errorf("entry_000001's mentions = %+v", entry.Mentions)
	}
	if _, err := service.store.Get(ctx, asAlice, "entry_000003", false); err == nil {
		t.Errorf("the deleted entry_000003 came back live")
	}
	if deleted, err := service.store.Get(ctx, asAlice, "entry_000003", true); err != nil || deleted.DeletedAt != 1700000300 || deleted.Source != "samsung-notes" {
		t.Errorf("entry_000003 with deleted = %+v, %v", deleted, err)
	}
	if _, err := service.store.Get(ctx, ownership.OwnedBy(bobPrincipalID), "entry_000001", false); err == nil {
		t.Errorf("Bob reaches an entry imported for Alice")
	}
	revisions, err := service.store.ListRevisions(ctx, asAlice, "entry_000001", 0, 0)
	if err != nil || len(revisions) != 1 || revisions[0].RevisionID != 5 || revisions[0].OwnerPrincipalID != alicePrincipalID {
		t.Errorf("entry_000001's revisions = %+v, %v", revisions, err)
	}
	item, err := service.store.GetTimelineItem(ctx, asAlice, "timeline_item_000002", false)
	if err != nil || item.TimelineID != "entry_000004" || item.OwnerPrincipalID != alicePrincipalID || item.Sources[0] != "entry_000001" {
		t.Errorf("timeline_item_000002 = %+v, %v", item, err)
	}
	itemRevisions, err := service.store.ListTimelineItemRevisions(ctx, asAlice, "timeline_item_000002", 0, 0)
	if err != nil || len(itemRevisions) != 1 || itemRevisions[0].RevisionID != 7 || itemRevisions[0].Item.TimelineID != "entry_000004" {
		t.Errorf("timeline_item_000002's revisions = %+v, %v", itemRevisions, err)
	}
	backlinks, err := service.store.Backlinks(ctx, asAlice, "person_000013", 0, 0)
	if err != nil || len(backlinks) != 1 || backlinks[0].Entry.ID != "entry_000004" {
		t.Errorf("backlinks of person_000013 through the item = %+v, %v", backlinks, err)
	}
	published, err := service.store.GetPublished(ctx, asAlice, "a-trip")
	if err != nil || published.ID != "entry_000004" {
		t.Errorf("/published/a-trip = %+v, %v", published, err)
	}

	// The search column was built from the copied rows.
	found, err := service.store.List(ctx, asAlice, Filter{Query: "lisb"})
	if err != nil || len(found) != 1 || found[0].ID != "entry_000001" {
		t.Errorf("a search for lisb = %d entries, %v; want entry_000001", len(found), err)
	}
	found, err = service.store.List(ctx, asAlice, Filter{Query: "trav"})
	if err != nil || len(found) != 1 || found[0].ID != "entry_000001" {
		t.Errorf("a search for a tag's prefix = %d entries, %v; want entry_000001", len(found), err)
	}

	// The next of each takes the number after the highest copied.
	created := service.mustCreate(EntryInput{Kind: "note", Title: "after the import"})
	if created.ID != "entry_000005" {
		t.Errorf("the first entry after the import is %s, want entry_000005", created.ID)
	}
	items := service.mustAddItems("entry_000004", TimelineItemInput{OccurredAt: 1700000700, Precision: "day", Title: "home"})
	if items[0].ID != "timeline_item_000003" {
		t.Errorf("the first item after the import is %s, want timeline_item_000003", items[0].ID)
	}
	var patched Entry
	service.call("PATCH", "/entries/entry_000001", map[string]string{"body": "edited"}, &patched)
	revisions, _ = service.store.ListRevisions(ctx, asAlice, "entry_000001", 0, 0)
	if len(revisions) != 2 || revisions[0].RevisionID != 10 {
		t.Errorf("the first revision after the import is %+v, want revision 10", revisions[0])
	}

	// A module that holds rows is refused, so an import cannot run twice.
	if _, err := ImportStandaloneDatabase(ctx, service.store.db, path, alicePrincipalID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import = %v, want a refusal of a module that holds rows", err)
	}
}

func TestImportRefusesToRunWithoutAnOwner(t *testing.T) {
	service := newTestService(t)
	path := newStandaloneDatabase(t, standaloneRows...)
	for _, owner := range []string{"", "alice", "principal_1", " principal_000001"} {
		if _, err := ImportStandaloneDatabase(context.Background(), service.store.db, path, owner); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Errorf("an import owned by %q = %v, want a refusal naming the owner", owner, err)
		}
	}
	var held int
	if err := service.store.db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&held); err != nil || held != 0 {
		t.Errorf("a refused import left %d entries (%v)", held, err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	for name, statements := range map[string][]string{
		"an unknown column": {`ALTER TABLE entries ADD COLUMN mood TEXT NOT NULL DEFAULT ''`},
		"an id that is not its number": {
			`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, created_at, updated_at) VALUES ('entry_000002', 1, 'note', 't', '', 1, 'private', 1, 1)`},
		"tags that are not a list": {
			`INSERT INTO entries (id, seq, kind, title, body, written_at, visibility, tags, created_at, updated_at) VALUES ('entry_000001', 1, 'note', 't', '', 1, 'private', '"travel"', 1, 1)`},
		"a mention of no entry": {
			`PRAGMA foreign_keys = OFF`,
			`INSERT INTO mentions (entry_id, position, line, entity_ref) VALUES ('entry_000009', 0, 1, 'person_000012')`},
	} {
		t.Run(name, func(t *testing.T) {
			service := newTestService(t)
			path := newStandaloneDatabase(t, statements...)
			if _, err := ImportStandaloneDatabase(context.Background(), service.store.db, path, alicePrincipalID); err == nil {
				t.Fatalf("the import took a file with %s", name)
			}
			var held int
			if err := service.store.db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&held); err != nil || held != 0 {
				t.Errorf("a refused import left %d entries (%v)", held, err)
			}
		})
	}
}
