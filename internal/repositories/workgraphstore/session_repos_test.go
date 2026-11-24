package workgraphstore

import (
	"path/filepath"
	"testing"
)

func TestSessionReposCountsEachSessionsRefUpdatesPerRepo(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	update := func(at int64, session, gitDirectory string) SpooledRefUpdate {
		return SpooledRefUpdate{RecordedAtUnixNano: at, SessionID: session, Hook: "reference-transaction", GitSubcommand: "commit",
			GitCommonDirectory: gitDirectory, WorktreePath: filepath.Dir(gitDirectory), Ref: "refs/heads/main", OldSHA: "a", NewSHA: "b"}
	}
	known := map[string]int64{"/repos/dash": 15, "/repos/scheduler": 67}
	if err := store.InsertRefUpdates([]SpooledRefUpdate{
		update(100, "br_old", "/repos/dash/.git"),
		update(200, "br_one", "/repos/dash/.git"),
		update(300, "br_one", "/repos/dash/.git"),
		update(400, "br_one", "/repos/scheduler/.git"),
		update(500, "", "/repos/dash/.git"),
		update(600, "br_one", "/repos/unknown/.git"),
	}, known); err != nil {
		t.Fatal(err)
	}
	repos, err := store.SessionRepos(150)
	if err != nil {
		t.Fatal(err)
	}
	want := []SessionRepo{{"br_one", 15, 2, 300}, {"br_one", 67, 1, 400}}
	if len(repos) != len(want) {
		t.Fatalf("got %+v, want %+v", repos, want)
	}
	for index := range want {
		if repos[index] != want[index] {
			t.Errorf("row %d: got %+v, want %+v", index, repos[index], want[index])
		}
	}
}
