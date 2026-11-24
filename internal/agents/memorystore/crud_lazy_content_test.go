package memorystore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A lazy memory stores a path instead of its text, and Get reads the file on the
// way out. loadLazyContent is where that happens, and it has three answers: the
// reference names no path, the path does not read, or the content loads.
//
// Measured before this file existed: replacing the whole of loadLazyContent with
// `return nil` left the entire suite green. Nothing in the suite ever saved a
// memory with IsLazy set, so neither its two failure producers nor its success
// path was executed — a memory could have come back from Get with an empty body
// and every test would still have passed.

func newLazyContentTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func saveLazyReference(t *testing.T, store *Store, id, refType, refTarget string) {
	t.Helper()

	if err := store.Save(Memory{
		ID:        id,
		Content:   "", // the point of a lazy reference: the text is not here
		RefType:   refType,
		RefTarget: refTarget,
		IsLazy:    true,
		Source:    "system",
	}); err != nil {
		t.Fatalf("save lazy reference: %v", err)
	}
}

// The success path, and it asserts the content rather than the absence of an
// error. Get returning nil error is what a gutted loadLazyContent also returns.
func TestGetLoadsALazyReferenceFromDisk(t *testing.T) {
	store := newLazyContentTestStore(t)

	path := filepath.Join(t.TempDir(), "identity.md")
	body := "I am the agent this file describes."
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	saveLazyReference(t, store, "lazy-identity", "file", path)

	loaded, err := store.Get("lazy-identity")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.Content != body {
		t.Errorf("Content = %q, want %q — the reference was returned without its text", loaded.Content, body)
	}
	if loaded.Tokens != len(body)/4 {
		t.Errorf("Tokens = %d, want %d — the token count is derived from the loaded content", loaded.Tokens, len(body)/4)
	}
}

// Producer one: a reference that names no path. It is a different fault from a
// path that will not read — this one is a malformed record rather than a missing
// file — so the test names the message, not merely the failure.
func TestALazyReferenceWithNoPathSaysSo(t *testing.T) {
	store := newLazyContentTestStore(t)
	saveLazyReference(t, store, "lazy-empty", "file", "")

	_, err := store.Get("lazy-empty")
	if err == nil {
		t.Fatal("Get of a lazy reference with no ref_target succeeded")
	}
	if !strings.Contains(err.Error(), "missing ref_target") {
		t.Errorf("Get returned %q, want it to name the missing ref_target — this is a malformed record, not an unreadable file", err)
	}
}

// Producer two: a path that does not read. The message must name the path, which
// is the only thing that tells an operator which reference went stale.
func TestALazyReferenceToAMissingFileNamesThePath(t *testing.T) {
	store := newLazyContentTestStore(t)
	missing := filepath.Join(t.TempDir(), "was-here.md")
	saveLazyReference(t, store, "lazy-missing", "file", missing)

	_, err := store.Get("lazy-missing")
	if err == nil {
		t.Fatal("Get of a lazy reference to a missing file succeeded")
	}
	// Asserted as "read <path>:", not as "the path appears somewhere". The
	// wrapped os.ReadFile error names the path by itself, so a bare Contains
	// check on the path passes even when this layer's own format string stops
	// naming it — measured: dropping %s from the wrapper left that check green.
	// The prefix is the part memory-store writes.
	if !strings.Contains(err.Error(), "read "+missing+":") {
		t.Errorf("Get returned %q, want this layer to name the reference as \"read %s:\"", err, missing)
	}
	if strings.Contains(err.Error(), "missing ref_target") {
		t.Errorf("Get returned %q, which is the other producer's message", err)
	}
}

// The known-negative control, and it is reached by the branch that matters.
//
// A plain memory would not do: its RefType defaults to "memory", so the switch
// inside loadLazyContent returns nil for it whether or not Get's IsLazy guard
// ran — measured, and dropping that guard was silent against such a fixture.
// The shape that separates them carries a file RefType with IsLazy false, and
// POST /memories accepts exactly that (ref_type and ref_target are settable
// with is_lazy absent). For it, the guard is the only thing keeping the posted
// content from being replaced by whatever is on disk.
func TestAMemoryWithAFileRefButNotLazyKeepsItsStoredContent(t *testing.T) {
	store := newLazyContentTestStore(t)

	path := filepath.Join(t.TempDir(), "on-disk.md")
	if err := os.WriteFile(path, []byte("what the file says"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := store.Save(Memory{
		ID:        "ref-but-not-lazy",
		Content:   "what the caller stored",
		RefType:   "file",
		RefTarget: path,
		IsLazy:    false,
		Source:    "user",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := store.Get("ref-but-not-lazy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.Content != "what the caller stored" {
		t.Errorf("Content = %q, want the stored text — a reference that is not lazy was resolved anyway", loaded.Content)
	}
}
