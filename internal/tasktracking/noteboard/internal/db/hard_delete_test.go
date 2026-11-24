package db

import (
	"testing"

	"github.com/kayushkin/noteboard/model"
)

// DeleteItem's `hard` parameter selects between the two deletes this store
// offers, and until these tests it had only ever been called with false.
// Three call sites in this package passed false; none passed true. So the
// branch that runs `DELETE FROM items` — the only statement in the store that
// destroys a row instead of tombstoning it — had never executed under test,
// and neither had the snapshot that is supposed to precede it.
//
// The untested value being the destructive one is not bad luck. A test author
// reaching for a default reaches for the harmless argument, so a boolean's
// unexercised branch is systematically the one that can lose data.

// TestAHardDeleteDestroysTheRowAndLeavesThePurgeSnapshotBehind pins both
// halves of the documented guarantee: the row really is gone, and a revision
// carrying the item is still readable afterwards.
//
// Asserting the content rather than the existence of that revision is what
// makes it worth writing. A snapshot taken from an empty struct still produces
// a row, with the right reason, at the right moment — so a test that counted
// revisions, or compared their timestamps, would pass while the item it was
// meant to preserve had been lost. Measured: writing the snapshot from
// `&model.Item{ID: existing.ID}` reddens five assertions here and nothing
// anywhere else in the package.
//
// ⚠️ What this test does NOT prove is the ordering, and the obvious reading of
// it says otherwise. "The content cannot be read off a row that no longer
// exists" is false here: GetItemIncludingDeleted has already loaded the item
// into `existing`, so a snapshot written after the DELETE still carries every
// field and every assertion below still passes. Moving the snapshot after the
// delete was measured — it reddens only
// TestAHardDeleteWhoseSnapshotFailsKeepsTheRow, further down. That test, not
// this one, is what holds "snapshots first".
func TestAHardDeleteDestroysTheRowAndLeavesThePurgeSnapshotBehind(t *testing.T) {
	s := newTestStore(t)

	body := "the body that has to survive the row"
	priority := 3
	status := "done"
	item, err := s.CreateItem(&model.CreateItemRequest{
		Type:     model.TypeNote,
		Title:    "purged but not forgotten",
		Body:     &body,
		Tags:     []string{"ops", "irreversible"},
		Priority: &priority,
		Status:   &status,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	if err := s.DeleteItem(item.ID, true); err != nil {
		t.Fatalf("DeleteItem(hard): %v", err)
	}

	// A soft delete would leave the row with deleted_at set. This one must
	// leave nothing, so count the raw table rather than a read path that
	// filters tombstones out and cannot tell the two apart.
	var rows int
	if err := s.db.QueryRow("SELECT count(*) FROM items WHERE id = ?", item.ID).Scan(&rows); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if rows != 0 {
		t.Errorf("hard delete left %d row(s) in items; it must destroy the row, not tombstone it", rows)
	}

	revisions, err := s.ListRevisions(item.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	var purge *model.Revision
	for _, rev := range revisions {
		if rev.Reason == "purge" {
			purge = rev
			break
		}
	}
	if purge == nil {
		t.Fatalf("no revision with reason %q after a hard delete; got %d revision(s), so the purge was unrecoverable", "purge", len(revisions))
	}

	if purge.Title != item.Title {
		t.Errorf("purge snapshot title = %q, want %q", purge.Title, item.Title)
	}
	if purge.Body != body {
		t.Errorf("purge snapshot body = %q, want %q", purge.Body, body)
	}
	if purge.Status != status {
		t.Errorf("purge snapshot status = %q, want %q", purge.Status, status)
	}
	if purge.Priority != priority {
		t.Errorf("purge snapshot priority = %d, want %d", purge.Priority, priority)
	}
	if len(purge.Tags) != 2 || purge.Tags[0] != "ops" || purge.Tags[1] != "irreversible" {
		t.Errorf("purge snapshot tags = %v, want [ops irreversible]", purge.Tags)
	}

	// The revision outliving its item is the whole point, and nothing in the
	// schema enforces it — item_revisions.item_id carries no foreign key, so
	// an ON DELETE CASCADE added later would wipe the snapshot in the same
	// statement that purges the row and every assertion above would still
	// pass except this one.
	var kept int
	if err := s.db.QueryRow("SELECT count(*) FROM item_revisions WHERE item_id = ?", item.ID).Scan(&kept); err != nil {
		t.Fatalf("count item_revisions: %v", err)
	}
	if kept != len(revisions) {
		t.Errorf("item_revisions holds %d row(s) for a purged item but ListRevisions returned %d", kept, len(revisions))
	}
}

// TestTheTwoDeletesAreDistinguishableInTheRevisionLog. `delete` and `purge`
// are different events — one is undoable, one is not — and the reason column
// is the only thing that records which happened. If both wrote the same
// reason, a reader of the log could not tell a tombstone from a destroyed row.
func TestTheTwoDeletesAreDistinguishableInTheRevisionLog(t *testing.T) {
	s := newTestStore(t)

	soft := mustCreateTyped(t, s, model.TypeNote, "tombstoned", "body")
	if err := s.DeleteItem(soft.ID, false); err != nil {
		t.Fatalf("DeleteItem(soft): %v", err)
	}
	hard := mustCreateTyped(t, s, model.TypeNote, "destroyed", "body")
	if err := s.DeleteItem(hard.ID, true); err != nil {
		t.Fatalf("DeleteItem(hard): %v", err)
	}

	for _, tc := range []struct {
		name       string
		id         string
		wantReason string
	}{
		{"soft delete", soft.ID, "delete"},
		{"hard delete", hard.ID, "purge"},
	} {
		revisions, err := s.ListRevisions(tc.id, 0, 0)
		if err != nil {
			t.Fatalf("%s: ListRevisions: %v", tc.name, err)
		}
		if len(revisions) != 1 {
			t.Fatalf("%s: got %d revisions, want exactly 1", tc.name, len(revisions))
		}
		if revisions[0].Reason != tc.wantReason {
			t.Errorf("%s recorded reason %q, want %q", tc.name, revisions[0].Reason, tc.wantReason)
		}
	}
}

// TestAHardDeleteWhoseSnapshotFailsKeepsTheRow. snapshot's own comment says
// the mutation must not proceed if the snapshot fails, "because a change
// nobody can undo is exactly what this store no longer does". For a soft
// delete that ordering is a nicety — the row survives either way. For a hard
// delete it is the entire guarantee, and this is the only test that puts it
// under a failing snapshot.
//
// Dropping item_revisions is the cheapest way to make the INSERT fail without
// reaching into the store's internals.
//
// This is also the only test in the package that detects the snapshot being
// moved to after the DELETE, because that reordering is invisible to every
// assertion that runs against a snapshot which succeeded.
func TestAHardDeleteWhoseSnapshotFailsKeepsTheRow(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeNote, "must outlive a failed snapshot", "body")

	if _, err := s.db.Exec("DROP TABLE item_revisions"); err != nil {
		t.Fatalf("DROP TABLE item_revisions: %v", err)
	}

	if err := s.DeleteItem(item.ID, true); err == nil {
		t.Error("DeleteItem(hard) reported success with no revisions table; a purge that cannot be snapshotted must fail loudly")
	}

	var rows int
	if err := s.db.QueryRow("SELECT count(*) FROM items WHERE id = ?", item.ID).Scan(&rows); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if rows != 1 {
		t.Errorf("the row is gone after a failed snapshot; the purge ran anyway and the item is unrecoverable")
	}
}

// TestAHardDeleteTakesTheRowOutOfSearch. The items_ad trigger fires on
// DELETE and nothing else, so a hard delete is the only thing that runs it
// and it too had never been exercised. A stale FTS row does not just show a
// deleted item: items_fts is an external-content index, and a row in it whose
// item is gone points at a rowid that no longer resolves.
func TestAHardDeleteTakesTheRowOutOfSearch(t *testing.T) {
	s := newTestStore(t)
	item := mustCreateTyped(t, s, model.TypeNote, "xenophile", "body")

	found, err := s.Search(SearchParams{Query: "xenophile"})
	if err != nil {
		t.Fatalf("Search before delete: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("search found %d items before the delete, want 1 — the rest of this test proves nothing", len(found))
	}

	if err := s.DeleteItem(item.ID, true); err != nil {
		t.Fatalf("DeleteItem(hard): %v", err)
	}

	if found, err = s.Search(SearchParams{Query: "xenophile"}); err != nil {
		t.Fatalf("Search after purge: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("search still returns %d item(s) for a purged row", len(found))
	}

	var indexed int
	if err := s.db.QueryRow("SELECT count(*) FROM items_fts WHERE items_fts MATCH 'xenophile'").Scan(&indexed); err != nil {
		t.Fatalf("count items_fts: %v", err)
	}
	if indexed != 0 {
		t.Errorf("items_fts still holds %d row(s) for a purged item, so the index points at a rowid that no longer exists", indexed)
	}
}
