package workgraphstore

import (
	"path/filepath"
	"strings"
	"testing"
)

// A repo's graph asks for the ref updates of one repo. Most rows belong to
// other repos or to none, so the query must find its rows by index: a scan of
// the whole table took 144 ms per graph on a table of 590,000 rows.
func TestBranchSessionsQueryFindsARepoByIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.database.Query(`EXPLAIN QUERY PLAN `+branchSessionsQuery, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "USING INDEX ref_updates_by_repo_store_id") {
		t.Fatalf("branch sessions query does not use the repo index: %s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("branch sessions query sorts its rows instead of reading them in index order: %s", joined)
	}
}
