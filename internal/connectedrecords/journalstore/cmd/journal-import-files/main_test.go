package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	journalstore "github.com/kayushkin/journal-store"
)

// The whole path from a directory of made-up files to stored entries, run
// twice to show a second run changes nothing.
func TestImportingADirectoryTwiceCreatesThenSkips(t *testing.T) {
	registry := journalstore.NewStaticEntityTypeRegistry(nil)
	if err := registry.Refresh(); err != nil {
		t.Fatal(err)
	}
	store, err := journalstore.Open(t.TempDir(), registry, journalstore.NewNoteboardClient("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	journalstore.RegisterHandlers(mux, store)
	server := httptest.NewServer(mux)
	defer server.Close()

	directory := t.TempDir()
	for name, content := range map[string]string{"a.txt": "Groceries\nbread", "sub/b.md": "# Idea\n2020-05-06\nbuild a shed", "c.jpg": "x"} {
		path := filepath.Join(directory, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var output strings.Builder
	if err := run(directory, server.URL, "samsung-notes", true, 1, time.UTC, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "would have: created 2, updated 0, unchanged 0; 1 files not read") {
		t.Errorf("dry run said:\n%s", output.String())
	}
	if entries, _ := store.List(journalstore.Filter{}); len(entries) != 0 {
		t.Fatalf("the dry run wrote %d entries", len(entries))
	}

	output.Reset()
	if err := run(directory, server.URL, "samsung-notes", false, 1, time.UTC, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "imported: created 2, updated 0, unchanged 0") {
		t.Errorf("first run said:\n%s", output.String())
	}
	entries, err := store.List(journalstore.Filter{Tag: "samsung-notes"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("after the first run: %d entries, %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.Kind != "note" || entry.Visibility != "private" || entry.Source != "samsung-notes" {
			t.Errorf("imported entry = %+v", entry)
		}
		if entry.SourceRef == "sub/b.md" && (entry.Title != "Idea" || entry.WrittenAt == 0) {
			t.Errorf("sub/b.md imported as %+v", entry)
		}
	}

	output.Reset()
	if err := run(directory, server.URL, "samsung-notes", false, 100, time.UTC, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "imported: created 0, updated 0, unchanged 2") {
		t.Errorf("second run said:\n%s", output.String())
	}

	os.WriteFile(filepath.Join(directory, "a.txt"), []byte("Groceries\nbread\nbutter"), 0o644)
	output.Reset()
	if err := run(directory, server.URL, "samsung-notes", false, 100, time.UTC, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "imported: created 0, updated 1, unchanged 1") {
		t.Errorf("run after an edit said:\n%s", output.String())
	}
}

func TestAStoreThatRefusesTheImportIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	directory := t.TempDir()
	os.WriteFile(filepath.Join(directory, "a.txt"), []byte("a"), 0o644)
	var output strings.Builder
	err := run(directory, server.URL, "samsung-notes", false, 10, time.UTC, &output)
	if err == nil || !strings.Contains(err.Error(), "answered 400") {
		t.Fatalf("run = %v, want the store's 400", err)
	}
}
