package gitactivity

import (
	"context"
	"strings"
	"testing"
)

// A repo's graph asks for the ref updates of one repo. Most rows belong to
// other repos or to none, so the query must find its rows by index: a scan of
// the whole table took 144 ms per graph on a table of 590,000 rows. A test
// table is too small for the planner to prefer the index by itself, so the
// test forbids sequential scans and asks whether the index can serve the
// query, in its order, without a sort.
func TestBranchSessionsQueryFindsARepoByIndex(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(`SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := transaction.Query(`EXPLAIN `+branchSessionsQuery, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "ref_updates_by_repo_store_id") {
		t.Fatalf("branch sessions query does not use the repo index: %s", joined)
	}
	if strings.Contains(joined, "Sort") {
		t.Fatalf("branch sessions query sorts its rows instead of reading them in index order: %s", joined)
	}
}
