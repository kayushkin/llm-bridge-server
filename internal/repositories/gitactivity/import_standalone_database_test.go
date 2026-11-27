package gitactivity

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"

	_ "modernc.org/sqlite"
)

func TestImportKeepsEveryRefUpdateAndItsID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-graph-store.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/standalone_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{string(schema),
		`INSERT INTO ref_updates VALUES (3, 100, 'br_1', 'reference-transaction', 'commit', 15, '/srv/src/webapp', '/srv/src/webapp', 'refs/heads/main', 'a', 'b')`,
		`INSERT INTO ref_updates VALUES (9, 200, '', 'post-checkout', 'checkout', NULL, '/srv/src/other', '/srv/src/other-wt', 'HEAD', 'b', 'c')`,
	} {
		if _, err := source.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	source.Close()

	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if counts.RefUpdates != 2 || counts.WithARepoStoreID != 1 || counts.LastRefUpdateID != 9 || counts.NewestRecordedUnix != 200 {
		t.Fatalf("counts %+v", counts)
	}
	store := New(db)
	updates, err := store.ListRefUpdates(RefUpdateFilter{})
	if err != nil || len(updates) != 2 {
		t.Fatalf("%v %+v", err, updates)
	}
	if updates[0].ID != 9 || updates[0].RepoStoreID != 0 || updates[0].WorktreePath != "/srv/src/other-wt" ||
		updates[1].ID != 3 || updates[1].RepoStoreID != 15 || updates[1].SessionID != "br_1" {
		t.Fatalf("came across as %+v", updates)
	}
	if err := store.InsertRefUpdates([]SpooledRefUpdate{{RecordedAtUnixNano: 300, Hook: HookPostCheckout, GitCommonDirectory: "/srv/src/webapp/.git", Ref: "HEAD"}}, nil); err != nil {
		t.Fatal(err)
	}
	newest, err := store.ListRefUpdates(RefUpdateFilter{Limit: 1})
	if err != nil || newest[0].ID != 10 {
		t.Fatalf("the next id: %v %+v", err, newest)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("a second import: %v", err)
	}
}
