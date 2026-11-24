package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// The query is only fast if SQLite walks the sent ids once and finds each item
// by its primary key. Written as a plain JOIN it chose the other order and ran
// for minutes on this host's data; see QueryItems. A plan is what failed, so a
// plan is what is pinned: a timing test would pass on a small database and on a
// fast machine whichever order was chosen.
func TestItemsQueryWalksTheIdsOnce(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*)`+itemsQueryFrom+`items.deleted_at IS NULL`, "[]")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	steps := []string{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	if len(steps) != 2 || !strings.HasPrefix(steps[0], "SCAN sent") || !strings.Contains(steps[1], "SEARCH items USING INDEX sqlite_autoindex_items_1 (id=?)") {
		t.Fatalf("plan = %q\nwant the sent ids scanned first, then each item searched by its primary key", steps)
	}
}
