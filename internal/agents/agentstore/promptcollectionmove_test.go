package agentstore

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRepoPrompt = "# Producer\n\nDispatches workers.\n\n## Deploy\n\nRun deploy.sh.\n"

// newMoveFixture imports the host prompt and a project collection rendering
// to AGENTS.md and CLAUDE.md under repos/producer, and returns the project
// collection's id.
func newMoveFixture(t *testing.T) (*promptFixture, int64, *http.ServeMux) {
	t.Helper()
	f := newPromptFixture(t)
	f.write("repos/producer/AGENTS.md", testRepoPrompt)
	f.write("repos/producer/CLAUDE.md", testRepoPrompt)
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ImportUntrackedPromptFiles([]string{"AGENTS.md", "CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.store.db.QueryRow(`SELECT id FROM prompt_collections WHERE scope='project' AND root_path=?`,
		filepath.Join(f.home, "repos", "producer")).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterHandlers(mux, f.store)
	return f, id, mux
}

func (f *promptFixture) collectionRow(id int64) PromptCollection {
	f.t.Helper()
	c, err := f.store.getPromptCollection(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return *c
}

func (f *promptFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestMovingACollectionRootKeepsItsIdHistoryAndFileVersions(t *testing.T) {
	f, id, mux := newMoveFixture(t)
	oldRoot := filepath.Join(f.home, "repos", "producer")
	newRoot := filepath.Join(f.home, "repos", "orchestrator")

	before, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	trackedIDs := map[string]int64{}
	for _, output := range before.Outputs {
		if output.TrackedFileID == 0 {
			t.Fatalf("%s has no tracked file before the move", output.RelativePath)
		}
		trackedIDs[output.RelativePath] = output.TrackedFileID
	}
	revisionsBefore := f.count(`SELECT COUNT(*) FROM prompt_section_revisions WHERE collection_id=?`, id)
	versionsBefore := f.count(`SELECT COUNT(*) FROM tracked_file_versions WHERE tracked_file_id IN (SELECT id FROM tracked_files WHERE path LIKE ?)`, oldRoot+"/%")

	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	response := do(t, mux, "PUT", "/prompt-collections/"+itoa(id)+"/root", `{"root_path":"`+newRoot+`","slug":"project-orchestrator"}`)
	if response.Code != 200 {
		t.Fatalf("move = %d %s", response.Code, response.Body.String())
	}
	var move PromptCollectionRootMove
	if err := json.Unmarshal(response.Body.Bytes(), &move); err != nil {
		t.Fatal(err)
	}
	if move.PreviousRootPath != oldRoot || move.PreviousSlug != "project-producer" || len(move.RenamedTrackedFiles) != 2 {
		t.Fatalf("move = %+v", move)
	}

	after := f.collectionRow(id)
	if after.RootPath != newRoot || after.Slug != "project-orchestrator" || after.CreatedAt != before.Collection.CreatedAt {
		t.Fatalf("collection after move = %+v", after)
	}
	view, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sections) != len(before.Sections) || view.Sections[0].ID != before.Sections[0].ID {
		t.Fatal("the move changed the collection's sections")
	}
	for _, output := range view.Outputs {
		if output.Path != filepath.Join(newRoot, output.RelativePath) || !output.ExistsOnDisk || output.Drifted {
			t.Fatalf("output after move = %+v", output)
		}
		if output.TrackedFileID != trackedIDs[output.RelativePath] {
			t.Fatalf("%s tracked file id = %d, want %d kept", output.RelativePath, output.TrackedFileID, trackedIDs[output.RelativePath])
		}
	}
	if got := f.count(`SELECT COUNT(*) FROM prompt_section_revisions WHERE collection_id=?`, id); got != revisionsBefore {
		t.Fatalf("revisions = %d, want %d", got, revisionsBefore)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_file_versions WHERE tracked_file_id IN (SELECT id FROM tracked_files WHERE path LIKE ?)`, newRoot+"/%"); got != versionsBefore {
		t.Fatalf("versions under the new root = %d, want the %d from the old root", got, versionsBefore)
	}

	// The next scan finds the files where the rows say they are: no new rows,
	// no drift, and nothing left behind at the old path.
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, newRoot+"/%"); got != 2 {
		t.Fatalf("tracked files under the new root after a scan = %d, want 2", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, oldRoot+"/%"); got != 0 {
		t.Fatalf("tracked files left at the old root = %d", got)
	}
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 0 {
		t.Fatalf("drifts after the move = %+v", reconciliation.Detected)
	}

	// An empty slug keeps the slug.
	thirdRoot := filepath.Join(f.home, "repos", "dispatcher")
	if _, err := f.store.MovePromptCollectionRoot(id, thirdRoot, ""); err != nil {
		t.Fatal(err)
	}
	if c := f.collectionRow(id); c.Slug != "project-orchestrator" || c.RootPath != thirdRoot {
		t.Fatalf("collection after a move without slug = %+v", c)
	}
}

// The scan runs on its own timer, so it can track the moved files at their
// new paths before the collection moves. The move then folds each such row
// into the row that carries the history, instead of refusing.
func TestMovingACollectionRootMergesRowsTheScanMadeAtTheNewPaths(t *testing.T) {
	f, id, mux := newMoveFixture(t)
	oldRoot := filepath.Join(f.home, "repos", "producer")
	newRoot := filepath.Join(f.home, "repos", "orchestrator")
	before, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	historyIDs := map[string]int64{}
	for _, output := range before.Outputs {
		historyIDs[output.RelativePath] = output.TrackedFileID
	}
	versionsBefore := f.count(`SELECT COUNT(*) FROM tracked_file_versions`)

	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	f.write("repos/orchestrator/CLAUDE.md", testRepoPrompt+"\nEdited after the move.\n")
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	scannedRows := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, newRoot+"/%")
	if scannedRows != 2 {
		t.Fatalf("the scan tracked %d files at the new root, want 2 for the test to mean anything", scannedRows)
	}
	versionsAfterScan := f.count(`SELECT COUNT(*) FROM tracked_file_versions`)

	response := do(t, mux, "PUT", "/prompt-collections/"+itoa(id)+"/root", `{"root_path":"`+newRoot+`"}`)
	if response.Code != 200 {
		t.Fatalf("move = %d %s", response.Code, response.Body.String())
	}
	var move PromptCollectionRootMove
	if err := json.Unmarshal(response.Body.Bytes(), &move); err != nil {
		t.Fatal(err)
	}
	if len(move.MergedTrackedFiles) != 2 || len(move.RenamedTrackedFiles) != 2 {
		t.Fatalf("move = %+v, want two merged and two renamed", move)
	}
	for _, merge := range move.MergedTrackedFiles {
		if merge.IntoTrackedFileID != historyIDs[filepath.Base(merge.Path)] {
			t.Errorf("merge %+v went into the wrong row; want %d", merge, historyIDs[filepath.Base(merge.Path)])
		}
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, newRoot+"/%"); got != 2 {
		t.Fatalf("tracked files under the new root = %d, want 2", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, oldRoot+"/%"); got != 0 {
		t.Fatalf("tracked files left at the old root = %d", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_file_versions`); got != versionsAfterScan || got < versionsBefore {
		t.Fatalf("versions = %d, want all %d kept", got, versionsAfterScan)
	}
	view, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range view.Outputs {
		if output.TrackedFileID != historyIDs[output.RelativePath] || !output.ExistsOnDisk {
			t.Fatalf("output after move = %+v, want tracked file %d", output, historyIDs[output.RelativePath])
		}
	}
	var status string
	if err := f.store.db.QueryRow(`SELECT status FROM tracked_files WHERE id=?`, historyIDs["CLAUDE.md"]).Scan(&status); err != nil || status != "present" {
		t.Fatalf("history row status = %q (%v), want present taken from the scanned row", status, err)
	}

	// The edit made after the move is a drift like any other, and the next
	// scan adds no row.
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, newRoot+"/%"); got != 2 {
		t.Fatalf("tracked files under the new root after a scan = %d, want 2", got)
	}
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) == 0 {
		t.Fatalf("the edit made after the move produced no drift")
	}
}

func TestMovingACollectionRootRefusesAndChangesNothing(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *promptFixture, id int64)
		root   string // relative to home unless it starts with '!'
		slug   string
		status int
		reason string
	}{
		{name: "relative root", root: "!repos/orchestrator", status: 400, reason: "absolute"},
		{name: "empty root", root: "!", status: 400, reason: "absolute"},
		{name: "global collection", root: "repos/orchestrator", status: 400, reason: "collection can move its root",
			setup: func(f *promptFixture, id int64) {}},
		{name: "root taken", root: "repos/taken", status: 409, reason: "already the project collection",
			setup: func(f *promptFixture, id int64) {
				if _, err := f.store.EnsurePromptCollection("project", filepath.Join(f.home, "repos", "taken"), "", ""); err != nil {
					f.t.Fatal(err)
				}
			}},
		{name: "slug taken", root: "repos/orchestrator", slug: "main", status: 409, reason: "belongs to collection"},
		{name: "target is another collection's output", root: "repos/orchestrator", status: 409, reason: "is an output of collection",
			setup: func(f *promptFixture, id int64) {
				other, err := f.store.EnsurePromptCollection("project", filepath.Join(f.home, "repos"), "", "")
				if err != nil {
					f.t.Fatal(err)
				}
				if _, err := f.store.AddPromptCollectionOutput(other.ID, "orchestrator/CLAUDE.md"); err != nil {
					f.t.Fatal(err)
				}
			}},
		{name: "held drift", root: "repos/orchestrator", status: 409, reason: "open or held drifts",
			setup: func(f *promptFixture, id int64) {
				f.write("repos/producer/AGENTS.md", testRepoPrompt+"\n## New\n\nAdded by hand.\n")
				if _, err := f.store.ReconcilePromptDrifts(); err != nil {
					f.t.Fatal(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, id, mux := newMoveFixture(t)
			if tc.name == "global collection" {
				if err := f.store.db.QueryRow(`SELECT id FROM prompt_collections WHERE scope='global'`).Scan(&id); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				tc.setup(f, id)
			}
			root := filepath.Join(f.home, tc.root)
			if strings.HasPrefix(tc.root, "!") {
				root = strings.TrimPrefix(tc.root, "!")
			}
			before := f.collectionRow(id)
			pathsBefore := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, before.RootPath+"/%")

			body, _ := json.Marshal(map[string]string{"root_path": root, "slug": tc.slug})
			response := do(t, mux, "PUT", "/prompt-collections/"+itoa(id)+"/root", string(body))
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.reason) {
				t.Fatalf("move = %d %s, want %d mentioning %q", response.Code, response.Body.String(), tc.status, tc.reason)
			}
			if after := f.collectionRow(id); after != before {
				t.Fatalf("a refused move changed the collection: %+v -> %+v", before, after)
			}
			if got := f.count(`SELECT COUNT(*) FROM tracked_files WHERE path LIKE ?`, before.RootPath+"/%"); got != pathsBefore {
				t.Fatalf("a refused move renamed tracked files: %d -> %d under %s", pathsBefore, got, before.RootPath)
			}
		})
	}
}

func TestMovingAMissingCollectionIsNotFound(t *testing.T) {
	f, _, mux := newMoveFixture(t)
	response := do(t, mux, "PUT", "/prompt-collections/9999/root", `{"root_path":"`+filepath.Join(f.home, "x")+`"}`)
	if response.Code != 404 {
		t.Fatalf("move = %d %s, want 404", response.Code, response.Body.String())
	}
	if response := do(t, mux, "PUT", "/prompt-collections/1/root", `{"root":"/x"}`); response.Code != 400 {
		t.Fatalf("unknown field = %d, want 400", response.Code)
	}
}

func itoa(id int64) string {
	data, _ := json.Marshal(id)
	return string(data)
}
