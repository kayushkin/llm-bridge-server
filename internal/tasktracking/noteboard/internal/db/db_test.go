package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

// legacyTimeLayout is SQLite's own timestamp encoding, and the one
// mattn/go-sqlite3 wrote for this table's entire history — every row in the
// live database is stored in it.
//
// modernc.org/sqlite writes Go's time.Time.String() instead
// ("2026-07-12 23:56:23.353827032 +0000 UTC") unless _time_format pins it to
// this. The two are different strings in a TEXT column that ORDER BY compares
// lexicographically, so a mismatch would not error anywhere — it would just
// quietly interleave new rows with old ones in the wrong order.
const legacyTimeLayout = "2006-01-02 15:04:05.999999999-07:00"

var sqliteTimeText = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d{1,9})?\+00:00$`)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// New() runs migrate(), which issues CREATE VIRTUAL TABLE ... USING fts5.
	// That statement is the reason this package used to need CGO_CFLAGS: a
	// driver without the FTS5 module compiled in fails right here. Every test
	// below therefore also asserts, just by getting a store, that a default
	// `go build` produces a binary that can open its own database.
	store, err := New(filepath.Join(t.TempDir(), "noteboard.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustCreate(t *testing.T, s *Store, title string) *model.Item {
	t.Helper()
	item, err := s.CreateItem(&model.CreateItemRequest{Type: "todo", Title: title})
	if err != nil {
		t.Fatalf("CreateItem(%q): %v", title, err)
	}
	return item
}

// rawText reads a column's stored bytes, bypassing the driver's habit of
// decoding a DATETIME column into a time.Time. CAST(...) makes the result an
// expression, which has no declared type, so the driver hands back the text.
func rawText(t *testing.T, s *Store, col, id string) string {
	t.Helper()
	var got string
	q := fmt.Sprintf("SELECT CAST(%s AS TEXT) FROM items WHERE id = ?", col)
	if err := s.db.QueryRow(q, id).Scan(&got); err != nil {
		t.Fatalf("read raw %s: %v", col, err)
	}
	return got
}

// insertLegacyRow writes a row the way the mattn-era binary wrote it: the
// timestamp columns as pre-formatted SQLite-format text, bound as a string so
// nothing in the current driver touches the encoding. This is what all 4100
// rows in the live database look like.
//
// schedule and held_at bind NULL, and hold_reason binds the empty string its
// column defaults to — which is exactly what a row predating those columns holds,
// since ALTER TABLE ADD COLUMN backfills the declared default. So these rows keep
// testing the real legacy shape rather than a shape no row on disk has. A legacy
// row is therefore un-held, which is the right default: the gate did not exist
// when it was written.
func insertLegacyRow(t *testing.T, s *Store, id, title string, created time.Time, dueAt any) {
	t.Helper()
	stamp := created.Format(legacyTimeLayout)
	_, err := s.db.Exec(
		"INSERT INTO items ("+insertCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, "todo", title, "", "[]", 0, 0.0, "open", "", dueAt, nil, "[]", "", stamp, stamp, nil,
		nil, "", nil,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
}

// TestTimestampsAreStoredInSQLiteFormat is the regression guard for the driver
// swap. Drop _time_format=sqlite from the DSN and this fails: the stored text
// becomes Go's time.Time.String().
func TestTimestampsAreStoredInSQLiteFormat(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "format check")

	for _, col := range []string{"created_at", "updated_at"} {
		got := rawText(t, s, col, item.ID)
		if !sqliteTimeText.MatchString(got) {
			t.Errorf("%s stored as %q, which is not SQLite's timestamp format\n"+
				"(a Go time.Time.String() value looks like \"2026-07-12 23:56:23.35 +0000 UTC\")", col, got)
		}
		if _, err := time.Parse(legacyTimeLayout, got); err != nil {
			t.Errorf("%s = %q does not parse as the format the live rows use: %v", col, got, err)
		}
	}
}

// TestReadsRowsWrittenByTheLegacyDriver proves the swap can read the data
// already on disk — the 4100 rows mattn/go-sqlite3 wrote.
func TestReadsRowsWrittenByTheLegacyDriver(t *testing.T) {
	s := newTestStore(t)
	want := time.Date(2026, 7, 12, 23, 56, 23, 353827032, time.UTC)
	insertLegacyRow(t, s, "legacy-1", "written by the old driver", want, nil)

	got, err := s.GetItem("legacy-1")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want)
	}
	if !got.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want)
	}
}

// TestStoredTimestampsAreReadableBySQLiteDateFunctions is the sharpest reason
// the format pin exists. A Go time.Time.String() value is not a timestamp
// SQLite recognizes, and SQLite does not complain about that — date() and
// friends just return NULL. Any date arithmetic in SQL would quietly evaluate
// to nothing, so the loud assertion belongs here.
func TestStoredTimestampsAreReadableBySQLiteDateFunctions(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "date function check")
	today := time.Now().UTC().Format("2006-01-02")

	var day, julian any
	err := s.db.QueryRow(
		"SELECT date(created_at), julianday(created_at) FROM items WHERE id = ?", item.ID,
	).Scan(&day, &julian)
	if err != nil {
		t.Fatalf("date(created_at): %v", err)
	}
	if day == nil || julian == nil {
		t.Fatalf("SQLite cannot parse the stored created_at: date()=%v julianday()=%v\n"+
			"The value was written in an encoding SQLite does not recognize as a timestamp.", day, julian)
	}
	if day != today {
		t.Errorf("date(created_at) = %v, want %v", day, today)
	}
}

// TestListOrdersChronologicallyAcrossDriverEras covers rows written on either
// side of the swap sorting together correctly. ORDER BY on these TEXT columns
// is a lexicographic byte compare, so this is worth an assertion even though
// the encodings do happen to collate consistently with each other.
func TestListOrdersChronologicallyAcrossDriverEras(t *testing.T) {
	s := newTestStore(t)

	// Oldest, written the legacy way.
	insertLegacyRow(t, s, "old", "oldest", time.Now().UTC().Add(-2*time.Hour), nil)
	// Newest, written through CreateItem by the current driver.
	newest := mustCreate(t, s, "newest")

	items, err := s.ListItems(ListParams{Type: "todo", Sort: "created_at"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].ID != newest.ID {
		t.Errorf("created_at DESC put %q first; want the newest row %q", items[0].Title, newest.Title)
	}
}

// insertDatedRow writes a row with a chosen priority and creation time, so a
// test can pin an ordering without racing the clock. Sorting is the one thing
// CreateItem cannot set up: it stamps time.Now(), and rows created inside the
// same test can land on the same timestamp.
func insertDatedRow(t *testing.T, s *Store, id, title string, priority int, created time.Time) {
	t.Helper()
	stamp := created.Format(legacyTimeLayout)
	_, err := s.db.Exec(
		"INSERT INTO items ("+insertCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, "todo", title, "", "[]", priority, 0.0, "open", "", nil, nil, "[]", "", stamp, stamp, nil,
		nil, "", nil,
	)
	if err != nil {
		t.Fatalf("insert row %q: %v", id, err)
	}
}

// TestPrioritySortBreaksTiesByRecency pins the tiebreaker on the priority sort.
//
// Priority is a tiny enumeration over a big table, so nearly every row ties with
// nearly every other: on the night this was written, 42 of the 85 open todos sat
// at priority 0. "ORDER BY priority DESC" alone therefore leaves the order of
// almost the whole table up to SQLite, and a caller passing a LIMIT gets an
// arbitrary slice of the tied rows rather than a stated one. Asserting only that
// two reads agree would not catch it — SQLite happens to scan in rowid order —
// so this asserts the order the store actually promises: newest first within a
// priority.
func TestPrioritySortBreaksTiesByRecency(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().UTC().Add(-24 * time.Hour)
	// Inserted oldest-first, so rowid order is the exact reverse of the answer.
	insertDatedRow(t, s, "tied-old", "tied, created first", 0, base)
	insertDatedRow(t, s, "tied-mid", "tied, created second", 0, base.Add(time.Hour))
	insertDatedRow(t, s, "tied-new", "tied, created last", 0, base.Add(2*time.Hour))
	insertDatedRow(t, s, "urgent", "the only high-priority row", 4, base)

	items, err := s.ListItems(ListParams{Type: "todo", Sort: "priority"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	got := make([]string, len(items))
	for i, it := range items {
		got[i] = it.ID
	}
	want := []string{"urgent", "tied-new", "tied-mid", "tied-old"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("priority sort returned %v, want %v", got, want)
	}
}

// TestLimitedPrioritySortKeepsTheHighestPriorityRows is the reason the
// tiebreaker matters. A limited read is how every unattended consumer sees this
// table, and without a total order the rows it drops are whichever ones SQLite
// felt like dropping — not the least important ones.
func TestLimitedPrioritySortKeepsTheHighestPriorityRows(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().UTC().Add(-48 * time.Hour)
	// The low-priority rows are the newest, so a sort that ignores priority —
	// or one whose ties are arbitrary — buries the urgent pair.
	insertDatedRow(t, s, "urgent-a", "urgent, oldest row in the table", 4, base)
	insertDatedRow(t, s, "urgent-b", "urgent, second oldest", 4, base.Add(time.Minute))
	for i := 0; i < 20; i++ {
		insertDatedRow(t, s, fmt.Sprintf("noise-%02d", i), "recent low-priority churn", 0,
			base.Add(time.Duration(10+i)*time.Minute))
	}

	items, err := s.ListItems(ListParams{Type: "todo", Sort: "priority", Limit: 2})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].ID != "urgent-b" || items[1].ID != "urgent-a" {
		t.Errorf("limited priority sort returned %q,%q; want the two priority-4 rows newest-first",
			items[0].ID, items[1].ID)
	}
}

// TestSearchMatchesViaFTS5 exercises the FTS5 module at runtime. A build
// without it never reaches this — it dies in migrate() — but the assertion also
// covers the index and its triggers actually working.
func TestSearchMatchesViaFTS5(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "xylophone reconciliation")
	mustCreate(t, s, "unrelated")

	found, err := s.Search(SearchParams{Query: "xylophone"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 1 || found[0].ID != item.ID {
		t.Fatalf("Search(xylophone) = %d hits, want just %q", len(found), item.Title)
	}
}

// TestSearchHandlesFTS5Operators covers queries that contain FTS5 operator
// characters. A hyphen is FTS5's NOT prefix, so a raw "hello-world" MATCH is a
// syntax error that used to bubble up as a 500 on the /api/search endpoint.
// After sanitizing, such a query is a literal phrase: it must not error, and it
// must still match an item whose text contains those words.
func TestSearchHandlesFTS5Operators(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "editorial-decision backlog")
	mustCreate(t, s, "unrelated entry")

	// Queries that are pure FTS5 operators or punctuation must return cleanly
	// (no hits, no error) rather than a syntax-error 500.
	for _, q := range []string{"a-b", "---", "*", ":", "AND", `foo"bar`, "   "} {
		if _, err := s.Search(SearchParams{Query: q}); err != nil {
			t.Fatalf("Search(%q) errored, want a clean empty result: %v", q, err)
		}
	}

	// A hyphenated query must still find the item it names.
	found, err := s.Search(SearchParams{Query: "editorial-decision"})
	if err != nil {
		t.Fatalf("Search(editorial-decision): %v", err)
	}
	if len(found) != 1 || found[0].ID != item.ID {
		t.Fatalf("Search(editorial-decision) = %d hits, want just %q", len(found), item.Title)
	}

	// Multi-word queries keep implicit-AND semantics: both terms must match.
	if found, err := s.Search(SearchParams{Query: "editorial backlog"}); err != nil {
		t.Fatalf("Search(editorial backlog): %v", err)
	} else if len(found) != 1 || found[0].ID != item.ID {
		t.Fatalf("Search(editorial backlog) = %d hits, want just %q", len(found), item.Title)
	}
	if found, err := s.Search(SearchParams{Query: "editorial nonexistentword"}); err != nil {
		t.Fatalf("Search(editorial nonexistentword): %v", err)
	} else if len(found) != 0 {
		t.Fatalf("Search(editorial nonexistentword) = %d hits, want 0 (implicit AND)", len(found))
	}
}

// TestDueAtPreservesStoredOffset covers the one non-null due_at in the live
// database, which is RFC3339 with a -07:00 offset. The offset is part of the
// value the caller supplied, so it has to survive the round trip rather than
// being flattened to UTC.
func TestDueAtPreservesStoredOffset(t *testing.T) {
	s := newTestStore(t)
	insertLegacyRow(t, s, "due-1", "has a due date", time.Now().UTC(), "2026-06-30T09:00:00-07:00")

	got, err := s.GetItem("due-1")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.DueAt == nil {
		t.Fatal("DueAt is nil, want 2026-06-30T09:00:00-07:00")
	}
	if want := time.Date(2026, 6, 30, 16, 0, 0, 0, time.UTC); !got.DueAt.Equal(want) {
		t.Errorf("DueAt = %v, want the same instant as %v", got.DueAt, want)
	}
	if got, want := got.DueAt.Format(time.RFC3339), "2026-06-30T09:00:00-07:00"; got != want {
		t.Errorf("DueAt renders as %q, want %q — the stored zone was lost", got, want)
	}
}

// TestDueAtRoundTripsThroughCreate covers the write side of the same column.
func TestDueAtRoundTripsThroughCreate(t *testing.T) {
	s := newTestStore(t)
	due := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)
	created, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "with a due date", DueAt: &due,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := s.GetItem(created.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.DueAt == nil || !got.DueAt.Equal(due) {
		t.Errorf("DueAt = %v, want %v", got.DueAt, due)
	}
}

// mustCreateTyped makes a live item of a given type/body for the reversibility
// tests below. (mustCreate above is todo-only and title-only.)
func mustCreateTyped(t *testing.T, s *Store, typ, title, body string) *model.Item {
	t.Helper()
	item, err := s.CreateItem(&model.CreateItemRequest{Type: typ, Title: title, Body: &body})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return item
}

// A delete must be undoable. Nothing in this store destroys data, so a deleted
// item leaves the row in place, disappears from every read path, and comes back
// intact.
func TestDeleteIsReversible(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeNote, "findable", "body")

	if err := s.DeleteItem(item.ID, false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	if _, err := s.GetItem(item.ID); err == nil {
		t.Fatal("GetItem returned a deleted item; a soft delete any read still returns is not a delete")
	}
	items, err := s.ListItems(ListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, got := range items {
		if got.ID == item.ID {
			t.Fatal("ListItems returned a deleted item")
		}
	}
	found, err := s.Search(SearchParams{Query: "findable"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, got := range found {
		if got.ID == item.ID {
			t.Fatal("Search returned a deleted item — FTS is a read path too")
		}
	}

	restored, err := s.RestoreItem(item.ID)
	if err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatal("restored item still carries a tombstone")
	}
	if restored.Body != "body" {
		t.Fatalf("restore lost the body: %q", restored.Body)
	}
}

// The bug this replaced: DELETE used to set status='archived', so a restore
// could not tell "the user archived this" from "this was deleted". Deletion
// must leave the item's own state untouched.
func TestDeleteDoesNotClobberStatus(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeTodo, "archived on purpose", "")
	archived := "archived"
	if _, err := s.UpdateItem(item.ID, &model.UpdateItemRequest{Status: &archived}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	if err := s.DeleteItem(item.ID, false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	restored, err := s.RestoreItem(item.ID)
	if err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if restored.Status != "archived" {
		t.Fatalf("restore did not preserve the status the user chose: got %q, want archived", restored.Status)
	}
}

// A workspace is rewritten on every run of its job, so an agent that corrupts
// its own working memory must not be able to destroy what came before.
func TestUpdateSnapshotsPriorBody(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeWorkspace, "deploy-guard memory", "## 2026-07-13\nfleet is stale")

	wiped := ""
	if _, err := s.UpdateItem(item.ID, &model.UpdateItemRequest{Body: &wiped}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	revisions, err := s.ListRevisions(item.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 1 {
		t.Fatalf("want 1 revision after one update, got %d", len(revisions))
	}
	if revisions[0].Body != "## 2026-07-13\nfleet is stale" {
		t.Fatalf("prior body not recoverable: %q", revisions[0].Body)
	}
	if revisions[0].Reason != "update" {
		t.Fatalf("revision reason = %q, want update", revisions[0].Reason)
	}
}

// TestHeldItemsAreWithheldFromDiscoveryByDefault is the guard on the agent gate.
// The whole point of the design is that it fails SAFE: a caller that passes no
// hold-related parameter at all — i.e. every consumer written before the gate
// existed, and every one written after by someone who never heard of it — must
// not see parked work. If this test ever needs an explicit "exclude held" flag
// added to make it pass, the gate has been inverted into an opt-in convention
// and no longer gates anything.
func TestHeldItemsAreWithheldFromDiscoveryByDefault(t *testing.T) {
	s := newTestStore(t)
	open := mustCreate(t, s, "free to work")
	parked := mustCreate(t, s, "parked, do not touch")

	if _, err := s.HoldItem(parked.ID, "sends email"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}

	// List: the autoworker's discovery path.
	items, err := s.ListItems(ListParams{Type: "todo"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, it := range items {
		if it.ID == parked.ID {
			t.Fatal("ListItems returned a held item with no include_held — the gate is open")
		}
	}
	if len(items) != 1 || items[0].ID != open.ID {
		t.Fatalf("want only the un-held item, got %d items", len(items))
	}

	// Search: the other way an agent finds work it was not handed.
	found, err := s.Search(SearchParams{Query: "parked"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("Search routed around the gate and returned %d held item(s)", len(found))
	}
}

// TestHoldIsVisibleToCallersThatAskForIt covers the surfaces that manage the
// gate — the kanban board and the notes UI cannot offer a "resume" button for a
// card they are not allowed to see.
func TestHoldIsVisibleToCallersThatAskForIt(t *testing.T) {
	s := newTestStore(t)
	parked := mustCreate(t, s, "parked")
	if _, err := s.HoldItem(parked.ID, "sends email"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}

	items, err := s.ListItems(ListParams{Type: "todo", IncludeHeld: true})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || !items[0].Held() || items[0].HoldReason != "sends email" {
		t.Fatalf("include_held must return the item with its reason intact, got %+v", items)
	}

	// Fetching by id is not discovery: a caller holding the id was handed the
	// item, so the gate does not apply.
	got, err := s.GetItem(parked.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if !got.Held() {
		t.Fatal("GetItem lost the hold")
	}
}

// TestHoldDoesNotTouchStatus is the reason hold is its own field. A parked todo
// is still open work the user wants to see in their own list; if hold were a
// status, parking it would delete it from that list.
func TestHoldDoesNotTouchStatus(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "parked but still open")
	held, err := s.HoldItem(item.ID, "")
	if err != nil {
		t.Fatalf("HoldItem: %v", err)
	}
	if held.Status != "open" {
		t.Fatalf("hold rewrote status to %q; hold is permission, status is lifecycle", held.Status)
	}

	cleared, err := s.UnholdItem(item.ID)
	if err != nil {
		t.Fatalf("UnholdItem: %v", err)
	}
	if cleared.Held() || cleared.HoldReason != "" || cleared.Status != "open" {
		t.Fatalf("unhold must clear held_at and the reason and leave status alone, got %+v", cleared)
	}
}

// TestCreateHeldLeavesNoWindow: an item born held must never have existed in a
// listable state, or an agent ticking every 5 minutes can win the race between
// "create" and "then hold it".
func TestCreateHeldLeavesNoWindow(t *testing.T) {
	s := newTestStore(t)
	item, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "email blast", Hold: true, HoldReason: "sends email",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if !item.Held() || item.HoldReason != "sends email" {
		t.Fatalf("create with hold must return a held item, got %+v", item)
	}
	items, err := s.ListItems(ListParams{Type: "todo"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatal("an item created held was listable")
	}
}

// TestReHoldingDoesNotMoveTheTimestamp — the hold dates from when the work was
// parked, not from the last time a UI re-sent the button press.
func TestReHoldingDoesNotMoveTheTimestamp(t *testing.T) {
	s := newTestStore(t)
	item := mustCreate(t, s, "parked")
	first, err := s.HoldItem(item.ID, "first reason")
	if err != nil {
		t.Fatalf("HoldItem: %v", err)
	}
	again, err := s.HoldItem(item.ID, "second reason")
	if err != nil {
		t.Fatalf("HoldItem (re-hold): %v", err)
	}
	if !again.HeldAt.Equal(*first.HeldAt) || again.HoldReason != "first reason" {
		t.Fatalf("re-hold overwrote the original hold: %+v", again)
	}
}

// TestSpendCeilingDistinguishesZeroFromUnset is the trap this field is built to
// avoid. NULL means "no ceiling"; 0.0 means "hold before spending a cent". A
// plain float64 column with DEFAULT 0 would collapse the two and silently arm a
// ceiling on every one of the ~4100 rows that predate the feature.
func TestSpendCeilingDistinguishesZeroFromUnset(t *testing.T) {
	s := newTestStore(t)

	none := mustCreate(t, s, "no ceiling")
	if none.AutoHoldAtUSD != nil {
		t.Fatalf("a plain item must have NO ceiling, got %v", *none.AutoHoldAtUSD)
	}

	zero := 0.0
	armed, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "zero ceiling", AutoHoldAtUSD: &zero,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if armed.AutoHoldAtUSD == nil {
		t.Fatal("a ceiling of 0 was stored as 'no ceiling' — zero and unset have collapsed")
	}
	if *armed.AutoHoldAtUSD != 0 {
		t.Fatalf("ceiling = %v, want 0", *armed.AutoHoldAtUSD)
	}
}

// TestSpendCeilingCanBeClearedWithNull — a ceiling you cannot take off is one
// you can only escape by deleting the item.
func TestSpendCeilingCanBeClearedWithNull(t *testing.T) {
	s := newTestStore(t)
	ten := 10.0
	item, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "capped", AutoHoldAtUSD: &ten,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// An unrelated PATCH must not disturb the ceiling.
	var req model.UpdateItemRequest
	if err := json.Unmarshal([]byte(`{"title":"renamed"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	touched, err := s.UpdateItem(item.ID, &req)
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if touched.AutoHoldAtUSD == nil || *touched.AutoHoldAtUSD != 10 {
		t.Fatalf("an unrelated PATCH dropped the ceiling: %v", touched.AutoHoldAtUSD)
	}

	// An explicit null clears it.
	var clear model.UpdateItemRequest
	if err := json.Unmarshal([]byte(`{"auto_hold_at_usd":null}`), &clear); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !clear.HasAutoHoldAtUSD {
		t.Fatal("explicit null did not register as present — it reads as 'leave alone'")
	}
	cleared, err := s.UpdateItem(item.ID, &clear)
	if err != nil {
		t.Fatalf("UpdateItem(null): %v", err)
	}
	if cleared.AutoHoldAtUSD != nil {
		t.Fatalf("null did not clear the ceiling: %v", *cleared.AutoHoldAtUSD)
	}
}

// TestNegativeSpendCeilingRejected — a negative ceiling is breached the instant
// it is set, arming a gate that can never open.
func TestNegativeSpendCeilingRejected(t *testing.T) {
	neg := -1.0
	req := &model.CreateItemRequest{Type: "todo", Title: "bad", AutoHoldAtUSD: &neg}
	if err := req.Validate(); err == nil {
		t.Fatal("a negative spend ceiling was accepted")
	}
}

// TestHoldIsInheritedByChildren is the escape hatch this closes. The autoworker's
// own prompt instructs a worker that a too-large todo must be SPLIT into child
// todos — so if a hold stopped the parent but not its children, "park this task"
// would be escapable by exactly the decomposition we asked for, and the spend
// ceiling built on top of the hold would be escapable with it.
func TestHoldIsInheritedByChildren(t *testing.T) {
	s := newTestStore(t)
	parent := mustCreate(t, s, "parent")
	child, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "child", ParentID: &parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(child): %v", err)
	}
	grandchild, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "grandchild", ParentID: &child.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(grandchild): %v", err)
	}

	// Before the hold, everything is workable.
	if items, _ := s.ListItems(ListParams{Type: "todo"}); len(items) != 3 {
		t.Fatalf("expected 3 workable todos before the hold, got %d", len(items))
	}

	if _, err := s.HoldItem(parent.ID, "spend ceiling reached"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}

	items, err := s.ListItems(ListParams{Type: "todo"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		var leaked []string
		for _, i := range items {
			leaked = append(leaked, i.Title)
		}
		t.Fatalf("holding the parent left %v dispatchable — a held task is escapable via its children", leaked)
	}

	// Search is a discovery path too.
	if found, _ := s.Search(SearchParams{Query: "grandchild"}); len(found) != 0 {
		t.Fatal("search returned the grandchild of a held parent")
	}

	// The hold lives on ONE row. The children are withheld by inheritance, not by
	// a cascaded write — nothing to go stale, and unhold needs no bookkeeping to
	// know which descendants were only ever held on the parent's account.
	got, err := s.GetItem(grandchild.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Held() {
		t.Error("the hold was cascaded onto the grandchild's own row; it should be inherited at read time")
	}

	// Releasing the parent releases the tree.
	if _, err := s.UnholdItem(parent.ID); err != nil {
		t.Fatalf("UnholdItem: %v", err)
	}
	if items, _ := s.ListItems(ListParams{Type: "todo"}); len(items) != 3 {
		t.Fatalf("unholding the parent should free the whole tree, got %d workable", len(items))
	}
}

// A parent_id cycle must not hang the query. UNION dedupes, so the recursion
// terminates; UNION ALL would spin forever and take every read path with it.
func TestHoldInheritanceSurvivesAParentCycle(t *testing.T) {
	s := newTestStore(t)
	a := mustCreate(t, s, "a")
	b, err := s.CreateItem(&model.CreateItemRequest{Type: "todo", Title: "b", ParentID: &a.ID})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	// Close the loop behind the API's back: a -> b -> a.
	if _, err := s.db.Exec("UPDATE items SET parent_id = ? WHERE id = ?", b.ID, a.ID); err != nil {
		t.Fatalf("create cycle: %v", err)
	}
	if _, err := s.HoldItem(a.ID, "cycle"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}
	items, err := s.ListItems(ListParams{Type: "todo"})
	if err != nil {
		t.Fatalf("ListItems over a cycle: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("both items in the cycle should be withheld, got %d", len(items))
	}
}

// The revision history is the one read on this store whose cost grows without
// bound as an item is used normally: every snapshot carries the WHOLE body it
// replaced, so an item that is rewritten on a schedule accumulates the sum of
// every version of itself. Measured 2026-08-21 on the nightly signpost todo —
// 308 revisions, 246 MB from a single GET — which is more than any caller can
// hold and far more than one asking "what did this look like last night" wants.
//
// The three tests below pin the page. They matter together: a limit that is
// silently ignored and a limit that works look identical on a short history,
// and the histories that need paging are exactly the ones no test creates.
func TestRevisionsCanBeAskedForOnePageAtATime(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeWorkspace, "harness-watch memory", "version 0")
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf("version %d", i)
		if _, err := s.UpdateItem(item.ID, &model.UpdateItemRequest{Body: &body}); err != nil {
			t.Fatalf("UpdateItem %d: %v", i, err)
		}
	}

	// Newest first, so the newest snapshot holds the body the LAST update
	// replaced — "version 4", not "version 5", which is the body still live on
	// the item and was never replaced by anything.
	page, err := s.ListRevisions(item.ID, 2, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("want a page of 2, got %d", len(page))
	}
	if page[0].Body != "version 4" || page[1].Body != "version 3" {
		t.Fatalf("first page = %q, %q; want version 4, version 3", page[0].Body, page[1].Body)
	}
}

func TestAnOffsetPagesThroughTheRestOfTheHistory(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeWorkspace, "harness-watch memory", "version 0")
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf("version %d", i)
		if _, err := s.UpdateItem(item.ID, &model.UpdateItemRequest{Body: &body}); err != nil {
			t.Fatalf("UpdateItem %d: %v", i, err)
		}
	}

	// Walk the whole history two at a time and require it to arrive exactly
	// once, in order. This is the assertion that a page boundary neither
	// repeats nor skips a revision — the failure an ordering full of ties
	// produces, and the reason this one orders by the unique revision id.
	// The walk is bounded and the bound FAILS rather than breaks. An offset that
	// is dropped hands back page one every time, so an unbounded loop over a
	// paged read does not report the defect — it hangs, which reads as a slow
	// test rather than a broken one. Found by sabotage: deleting the OFFSET
	// clause wedged this test for two minutes instead of failing it.
	var walked []string
	for offset := 0; offset <= 5+2; offset += 2 {
		page, err := s.ListRevisions(item.ID, 2, offset)
		if err != nil {
			t.Fatalf("ListRevisions offset %d: %v", offset, err)
		}
		if len(page) == 0 {
			break
		}
		for _, rev := range page {
			walked = append(walked, rev.Body)
		}
	}
	if len(walked) > 5 {
		t.Fatalf("paging returned %d revisions for a 5-revision history — the offset is not being applied: %v", len(walked), walked)
	}

	want := []string{"version 4", "version 3", "version 2", "version 1", "version 0"}
	if len(walked) != len(want) {
		t.Fatalf("paging walked %d revisions, want %d: %v", len(walked), len(want), walked)
	}
	for i := range want {
		if walked[i] != want[i] {
			t.Fatalf("paged history[%d] = %q, want %q (full walk: %v)", i, walked[i], want[i], walked)
		}
	}

	// An offset asked for on its own must still skip. SQLite refuses OFFSET
	// without LIMIT, so the store spells the missing limit as LIMIT -1; if that
	// is ever dropped, this call quietly returns page one again and a caller
	// paging without a limit loops forever on the same rows.
	rest, err := s.ListRevisions(item.ID, 0, 3)
	if err != nil {
		t.Fatalf("ListRevisions offset-only: %v", err)
	}
	if len(rest) != 2 || rest[0].Body != "version 1" {
		t.Fatalf("offset without limit did not skip: got %d rows starting %q", len(rest), rest[0].Body)
	}
}

// The default has to stay "every revision". This call could not be paged until
// 2026-08-21, so every caller written before then passes nothing and must keep
// getting the whole history — including the one that matters most, the claim in
// the workspace contract that rewriting a workspace is safe because the prior
// versions can be read back.
func TestNoLimitStillMeansTheWholeHistory(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeWorkspace, "harness-watch memory", "version 0")
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf("version %d", i)
		if _, err := s.UpdateItem(item.ID, &model.UpdateItemRequest{Body: &body}); err != nil {
			t.Fatalf("UpdateItem %d: %v", i, err)
		}
	}

	all, err := s.ListRevisions(item.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("unpaged read returned %d revisions, want all 5", len(all))
	}
}

// TestAMissingRowIsReportedAsErrItemNotFound. Every single-item read goes
// through scanItem, so the condition is born in one place and every caller can
// recognize it with errors.Is. Before this, the only signal was the driver's
// sql.ErrNoRows, which a handler can match on only by importing database/sql to
// ask a question about its own store.
func TestAMissingRowIsReportedAsErrItemNotFound(t *testing.T) {
	s := newTestStore(t)
	missing := "a88bca06-3c94-4b7a-9a2d-59c2d1a3e9d1"

	reads := map[string]func(string) (*model.Item, error){
		"GetItem":                 s.GetItem,
		"GetItemIncludingDeleted": s.GetItemIncludingDeleted,
	}
	for name, read := range reads {
		_, err := read(missing)
		if !errors.Is(err, ErrItemNotFound) {
			t.Errorf("%s on a missing id returned %v, want ErrItemNotFound", name, err)
		}
	}

	if err := s.DeleteItem(missing, false); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("DeleteItem on a missing id returned %v, want ErrItemNotFound", err)
	}
}

// TestASoftDeletedRowIsStillFound is the boundary the sentinel must not cross.
// GetItem hides a tombstone and so reports it missing; GetItemIncludingDeleted
// is the read that still has to find it, and DeleteItem is built on that one.
// If both reads reported ErrItemNotFound, a second delete would answer 404 for
// an item whose row is right there.
func TestASoftDeletedRowIsStillFound(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeNote, "tombstoned", "body")
	if err := s.DeleteItem(item.ID, false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	if _, err := s.GetItem(item.ID); !errors.Is(err, ErrItemNotFound) {
		t.Errorf("GetItem on a tombstone returned %v, want ErrItemNotFound", err)
	}
	if _, err := s.GetItemIncludingDeleted(item.ID); err != nil {
		t.Errorf("GetItemIncludingDeleted lost a tombstoned row: %v", err)
	}
	if err := s.DeleteItem(item.ID, false); err != nil {
		t.Errorf("a second soft delete reported %v; it is a no-op, not a missing item", err)
	}
}
