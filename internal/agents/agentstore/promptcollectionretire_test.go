package agentstore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiringACollectionDeletesItsSectionsWithRevisionsAndHidesIt(t *testing.T) {
	f, id, _ := newMoveFixture(t)
	root := filepath.Join(f.home, "repos", "producer")
	sectionsBefore := f.count(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, id)
	if sectionsBefore == 0 {
		t.Fatal("fixture collection has no sections")
	}

	retirement, err := f.store.RetirePromptCollection(id, "an import made it for a copy of the repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(retirement.DeletedSectionIDs) != sectionsBefore {
		t.Fatalf("deleted %d sections, want %d", len(retirement.DeletedSectionIDs), sectionsBefore)
	}
	if strings.Join(retirement.RemovedOutputPaths, ",") != "AGENTS.md,CLAUDE.md" {
		t.Fatalf("removed outputs %v", retirement.RemovedOutputPaths)
	}
	if n := f.count(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, id); n != 0 {
		t.Fatalf("%d sections remain", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM prompt_collection_outputs WHERE collection_id=?`, id); n != 0 {
		t.Fatalf("%d outputs remain", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM prompt_section_revisions WHERE collection_id=? AND operation='delete' AND note=?`,
		id, "an import made it for a copy of the repo"); n != sectionsBefore {
		t.Fatalf("%d delete revisions carry the note, want %d", n, sectionsBefore)
	}
	if row := f.collectionRow(id); row.RetiredAt == 0 {
		t.Fatal("retired_at not stamped")
	}

	collections, err := f.store.ListPromptCollections()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range collections {
		if c.ID == id {
			t.Fatal("a retired collection is still listed")
		}
	}
	// The files it rendered stay on disk.
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("retiring touched the rendered file: %v", err)
	}
}

func TestARetiredRootIsNeitherReusedNorReimported(t *testing.T) {
	f, id, _ := newMoveFixture(t)
	root := filepath.Join(f.home, "repos", "producer")
	if _, err := f.store.RetirePromptCollection(id, "duplicate"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnsurePromptCollection(promptCollectionScopeProject, root, "", ""); !errors.Is(err, ErrPromptCollectionRetired) {
		t.Fatalf("EnsurePromptCollection on a retired root: %v, want ErrPromptCollectionRetired", err)
	}
	// Its files are untracked again now that no output claims them; an import
	// must leave them alone rather than fail or bring the collection back.
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ImportUntrackedPromptFiles([]string{"AGENTS.md", "CLAUDE.md"}); err != nil {
		t.Fatalf("import after retiring: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM prompt_sections WHERE collection_id=?`, id); n != 0 {
		t.Fatalf("import put %d sections back", n)
	}
	if _, err := f.store.RetirePromptCollection(id, "again"); !errors.Is(err, ErrPromptCollectionRetired) {
		t.Fatalf("retiring twice: %v, want ErrPromptCollectionRetired", err)
	}
}

func TestRetiringRefusesTheGlobalCollectionAndAMissingNote(t *testing.T) {
	f, id, _ := newMoveFixture(t)
	var globalID int64
	if err := f.store.db.QueryRow(`SELECT id FROM prompt_collections WHERE scope='global'`).Scan(&globalID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RetirePromptCollection(globalID, "no"); !errors.Is(err, ErrPromptCollectionRetireRefused) {
		t.Fatalf("retiring the global collection: %v", err)
	}
	if _, err := f.store.RetirePromptCollection(id, "  "); !errors.Is(err, ErrPromptCollectionRetireRefused) {
		t.Fatalf("retiring with no note: %v", err)
	}
	if row := f.collectionRow(id); row.RetiredAt != 0 {
		t.Fatal("a refused retirement stamped retired_at")
	}
}

func TestRetireRouteAnswersAndRefuses(t *testing.T) {
	_, id, mux := newMoveFixture(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return recorder
	}
	path := "/prompt-collections/" + itoa(id) + "/retire"
	if got := post(path, `{"nope":1}`); got.Code != 400 {
		t.Fatalf("unknown field: %d %s", got.Code, got.Body)
	}
	if got := post(path, `{"note":"duplicate"}`); got.Code != 200 || !strings.Contains(got.Body.String(), `"removed_output_paths"`) {
		t.Fatalf("retire: %d %s", got.Code, got.Body)
	}
	if got := post(path, `{"note":"duplicate"}`); got.Code != 409 {
		t.Fatalf("retire twice: %d %s", got.Code, got.Body)
	}
	if got := post("/prompt-collections/999999/retire", `{"note":"x"}`); got.Code != 404 {
		t.Fatalf("missing collection: %d %s", got.Code, got.Body)
	}
}
