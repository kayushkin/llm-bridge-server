package snapshots

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newStoreWithConfig gives the test a store on a Postgres database of its
// own, made from a template with the module migrated
// (postgresstorage/postgrestest), with its blobs under the test's temp dir
// unless cfg names a folder.
func newStoreWithConfig(t *testing.T, cfg Config) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	if cfg.GitDir == "" {
		cfg.GitDir = filepath.Join(t.TempDir(), "snapshots.git")
	}
	s, err := New(postgrestest.NewDatabase(t, module).Open(module), cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return newStoreWithConfig(t, Config{})
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// findPhase returns the snapshot for the given file/phase from a Get result,
// or nil. Convenience for the new Get shape (slice of all rows).
func findPhase(snaps []Snapshot, filePath string, phase Phase) *Snapshot {
	for i := range snaps {
		if snaps[i].FilePath == filePath && snaps[i].Phase == phase {
			return &snaps[i]
		}
	}
	return nil
}

func TestCaptureAndGetRoundtrip(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	writeFile(t, path, []byte("hello world\n"))

	before, err := s.Capture("sess1", "tool1", PhaseBefore, path)
	if err != nil {
		t.Fatalf("capture before: %v", err)
	}
	if before.BlobSHA == "" || before.Size != 12 || before.IsBinary || before.Missing || before.TooLarge {
		t.Fatalf("unexpected before: %+v", before)
	}

	writeFile(t, path, []byte("hello world\nchanged\n"))
	after, err := s.Capture("sess1", "tool1", PhaseAfter, path)
	if err != nil {
		t.Fatalf("capture after: %v", err)
	}
	if after.BlobSHA == "" || after.BlobSHA == before.BlobSHA {
		t.Fatalf("expected distinct after blob, got %q (before %q)", after.BlobSHA, before.BlobSHA)
	}

	snaps, err := s.Get("sess1", "tool1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gotBefore := findPhase(snaps, path, PhaseBefore)
	gotAfter := findPhase(snaps, path, PhaseAfter)
	if gotBefore == nil || gotAfter == nil {
		t.Fatalf("expected both phases, got %+v", snaps)
	}
	if gotBefore.BlobSHA != before.BlobSHA || gotAfter.BlobSHA != after.BlobSHA {
		t.Fatalf("sha mismatch: got before=%q after=%q", gotBefore.BlobSHA, gotAfter.BlobSHA)
	}

	content, err := s.ReadBlob(before.BlobSHA)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(content, []byte("hello world\n")) {
		t.Fatalf("blob content mismatch: %q", content)
	}
}

// TestMultipleFilesPerToolCall exercises the v2 schema: one tool_use_id can
// hold snapshots for many file paths, each with its own before/after pair.
// Use case: a Bash command that touches several files in one invocation.
func TestMultipleFilesPerToolCall(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	writeFile(t, pathA, []byte("alpha\n"))
	writeFile(t, pathB, []byte("beta\n"))

	if _, err := s.Capture("sess1", "tool1", PhaseBefore, pathA); err != nil {
		t.Fatalf("capture A before: %v", err)
	}
	if _, err := s.Capture("sess1", "tool1", PhaseBefore, pathB); err != nil {
		t.Fatalf("capture B before: %v", err)
	}
	// Simulate a delete of A and an edit of B.
	if err := os.Remove(pathA); err != nil {
		t.Fatalf("remove A: %v", err)
	}
	writeFile(t, pathB, []byte("beta two\n"))

	if _, err := s.Capture("sess1", "tool1", PhaseAfter, pathA); err != nil {
		t.Fatalf("capture A after: %v", err)
	}
	if _, err := s.Capture("sess1", "tool1", PhaseAfter, pathB); err != nil {
		t.Fatalf("capture B after: %v", err)
	}

	snaps, err := s.Get("sess1", "tool1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(snaps) != 4 {
		t.Fatalf("expected 4 rows (2 files × 2 phases), got %d: %+v", len(snaps), snaps)
	}
	if snap := findPhase(snaps, pathA, PhaseAfter); snap == nil || !snap.Missing {
		t.Fatalf("expected A after to be missing, got %+v", snap)
	}
	if snap := findPhase(snaps, pathB, PhaseAfter); snap == nil || snap.BlobSHA == "" {
		t.Fatalf("expected B after to have blob, got %+v", snap)
	}
}

func TestCaptureMissingFile(t *testing.T) {
	s := newStore(t)
	snap, err := s.Capture("sess1", "tool1", PhaseBefore, "/nonexistent/does-not-exist")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !snap.Missing || snap.BlobSHA != "" {
		t.Fatalf("expected missing snapshot, got %+v", snap)
	}
}

func TestCaptureTooLarge(t *testing.T) {
	dir := t.TempDir()
	s := newStoreWithConfig(t, Config{GitDir: filepath.Join(dir, "git"), MaxSize: 16})

	path := filepath.Join(dir, "big.txt")
	writeFile(t, path, bytes.Repeat([]byte("x"), 100))

	snap, err := s.Capture("sess1", "tool1", PhaseBefore, path)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !snap.TooLarge || snap.BlobSHA != "" {
		t.Fatalf("expected too_large snapshot, got %+v", snap)
	}
	if snap.Size != 100 {
		t.Fatalf("expected size 100, got %d", snap.Size)
	}
}

func TestCaptureBinary(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bin.dat")
	writeFile(t, path, []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0x03})

	snap, err := s.Capture("sess1", "tool1", PhaseBefore, path)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !snap.IsBinary || snap.BlobSHA != "" {
		t.Fatalf("expected binary snapshot, got %+v", snap)
	}
}

func TestBlobDedup(t *testing.T) {
	s := newStore(t)
	sha1, err := s.WriteBlob([]byte("duplicate content"))
	if err != nil {
		t.Fatalf("write1: %v", err)
	}
	sha2, err := s.WriteBlob([]byte("duplicate content"))
	if err != nil {
		t.Fatalf("write2: %v", err)
	}
	if sha1 != sha2 {
		t.Fatalf("expected dedup, got %q vs %q", sha1, sha2)
	}
}

func TestHasBlob(t *testing.T) {
	s := newStore(t)
	sha, err := s.WriteBlob([]byte("hi"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	ok, err := s.HasBlob(sha)
	if err != nil || !ok {
		t.Fatalf("expected present, ok=%v err=%v", ok, err)
	}
	ok, err = s.HasBlob("0000000000000000000000000000000000000000")
	if err != nil || ok {
		t.Fatalf("expected absent, ok=%v err=%v", ok, err)
	}
}

func TestPurgeOlderThan(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, []byte("one"))

	if _, err := s.Capture("sess1", "tool-old", PhaseBefore, path); err != nil {
		t.Fatalf("capture old: %v", err)
	}
	// Age the row.
	if _, err := s.db.Exec(`UPDATE snapshots SET created_at = $1 WHERE tool_use_id = 'tool-old'`,
		time.Now().Add(-48*time.Hour).UTC()); err != nil {
		t.Fatalf("age row: %v", err)
	}
	if _, err := s.Capture("sess1", "tool-new", PhaseBefore, path); err != nil {
		t.Fatalf("capture new: %v", err)
	}

	n, err := s.PurgeOlderThan(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged, got %d", n)
	}

	snaps, err := s.Get("sess1", "tool-new")
	if err != nil || len(snaps) == 0 {
		t.Fatalf("expected new snapshot to remain, err=%v snaps=%v", err, snaps)
	}
	snaps, err = s.Get("sess1", "tool-old")
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected old snapshot purged, got %+v", snaps)
	}
}

func TestIsBinary(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"empty", nil, false},
		{"ascii", []byte("hello world"), false},
		{"utf8", []byte("héllo wörld"), false},
		{"null byte", []byte{'a', 0, 'b'}, true},
		{"png header", []byte{0x89, 'P', 'N', 'G', 0x00}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBinary(tc.buf); got != tc.want {
				t.Fatalf("IsBinary(%v) = %v, want %v", tc.buf, got, tc.want)
			}
		})
	}
}

func TestGCRuns(t *testing.T) {
	s := newStore(t)
	if _, err := s.WriteBlob([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.GC(); err != nil {
		t.Fatalf("gc: %v", err)
	}
}

// The phase column is guarded twice: by Capture's own check and by
// CHECK (phase IN ('before','after')) in the schema. Nothing pinned either one.
// Asserting only that Capture failed would not tell them apart, and the schema
// wins by answering last — so assert the wording only Capture produces, and
// assert it names the offending value.
func TestCaptureRefusesAPhaseTheSchemaWouldAlsoRefuse(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, []byte("hello"))

	_, err := s.Capture("sess1", "tool1", Phase("during"), path)
	if err == nil {
		t.Fatal("expected Capture to refuse a phase that is neither before nor after")
	}
	if !strings.Contains(err.Error(), "snapshotstore: invalid phase") {
		t.Fatalf("the refusal should be Capture's own, not the driver's CHECK constraint; got: %v", err)
	}
	if !strings.Contains(err.Error(), `"during"`) {
		t.Fatalf("the refusal should name the offending phase so the caller can act on it; got: %v", err)
	}
}

// Capture must refuse a bad phase BEFORE it does any work: an invalid phase is
// a caller bug, not a reason to read the file, write a blob and let the
// database reject the row several layers later. "It returned an error" cannot
// show this — the CHECK constraint returns one too. Counting blobs can.
func TestABadPhaseWritesNoBlob(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	writeFile(t, path, []byte("content worth storing"))

	before := countBlobs(t, s.GitDir())
	if _, err := s.Capture("sess1", "tool1", Phase("during"), path); err == nil {
		t.Fatal("expected a bad phase to be refused")
	}
	if after := countBlobs(t, s.GitDir()); after != before {
		t.Fatalf("a refused capture stored %d blob(s): the phase was checked after the file was read, not before", after-before)
	}
}

// countBlobs reports how many objects the bare blob repo holds.
func countBlobs(t *testing.T, gitDir string) int {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", gitDir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").Output()
	if err != nil {
		t.Fatalf("list blob objects: %v", err)
	}
	return len(bytes.Fields(out))
}
