package db

import (
	"path/filepath"
	"testing"
	"time"
)

// TestListCardsByEntityIsOldestLinkFirst pins the order of the session→cards
// reverse lookup.
//
// The order is part of the answer. One session picks up a dispatch card and
// then has its work classified onto more, and a caller that must name a
// single card — "which todo is this session for?" — reads the first one. The
// honest answer is the link that was made before the session did anything.
//
// The GROUP BY in the query is the SELECT DISTINCT it replaced, kept so the
// dedupe survives if the UNIQUE (card_id, entity_type, entity_ref) index ever
// relaxes. It has nothing to dedupe today, which is why there is no test for
// it: that state is unreachable through both the API and the schema.
//
// This has to insert rows directly, because through the public API a link's
// created_at is stamped at insert time and therefore always agrees with the
// order rows were written in. That agreement is what hides the defect: an
// unordered SELECT returns rows in whatever order SQLite finds convenient,
// which today happens to match and stops matching the moment the planner
// picks an index. Writing created_at out of step with insertion order is the
// only way to ask which of the two the query is actually promising.
func TestListCardsByEntityIsOldestLinkFirst(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	// Written newest-first, so insertion order is the reverse of link age.
	rows := []struct {
		linkID, cardID string
		age            time.Duration
	}{
		{"link-c", "card-newest", 2 * time.Hour},
		{"link-b", "card-middle", 1 * time.Hour},
		{"link-a", "card-oldest", 0},
	}
	for _, row := range rows {
		if _, err := store.db.Exec(
			`INSERT INTO card_links (id, card_id, entity_type, entity_ref, label, created_at)
			 VALUES (?,?,?,?,?,?)`,
			row.linkID, row.cardID, "session", "br_1", "", base.Add(row.age),
		); err != nil {
			t.Fatalf("insert %s: %v", row.linkID, err)
		}
	}

	got, err := store.ListCardsByEntity("session", "br_1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"card-oldest", "card-middle", "card-newest"}
	if len(got) != len(want) {
		t.Fatalf("got %d cards, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].CardID != want[i] {
			t.Fatalf("order = %+v, want %v", got, want)
		}
	}
}
