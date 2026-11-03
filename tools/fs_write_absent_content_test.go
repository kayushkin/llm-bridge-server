package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests hold the one thing card 0b5941db's two candidate repairs agree on:
// a write_files call that never sent a "content" key must not be read as a
// request to write empty bytes. An explicit "content":"" is a different call and
// still truncates, which is deliberate and is pinned below.

const existingContent = "ORIGINAL BYTES THAT MUST SURVIVE\n"

func writeVictim(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(existingContent), 0644); err != nil {
		t.Fatalf("seeding victim %s: %v", path, err)
	}
	return path
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	return string(b)
}

// TestFakeVictimIsSeeded is the rig guard. A victim that was never written would
// make every survival assertion below pass for the wrong reason.
func TestFakeVictimIsSeeded(t *testing.T) {
	path := writeVictim(t, t.TempDir(), "seeded.txt")
	if got := readBack(t, path); got != existingContent {
		t.Fatalf("rig broken: victim holds %q, want %q", got, existingContent)
	}
}

func TestWriteFiles_AbsentContentKeyLeavesTheFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := writeVictim(t, dir, "victim.txt")

	out, err := WriteFile().Run(context.Background(), `{"path":`+quote(path)+`}`)
	if err != nil {
		t.Fatalf("Run returned a transport error: %v", err)
	}

	// The file is the assertion, not the message. A repair that reports the
	// refusal after doing the write would still pass a message-only check.
	if got := readBack(t, path); got != existingContent {
		t.Fatalf("absent content key destroyed the file: holds %q, want %q (tool said %q)", got, existingContent, out)
	}
	if !strings.Contains(out, "content") {
		t.Errorf("refusal does not name the missing key: %q", out)
	}
	if strings.Contains(out, "wrote 0 bytes") {
		t.Errorf("refusal is reported as a successful zero-byte write: %q", out)
	}
}

func TestWriteFiles_NullContentLeavesTheFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := writeVictim(t, dir, "victim.txt")

	out, err := WriteFile().Run(context.Background(), `{"path":`+quote(path)+`,"content":null}`)
	if err != nil {
		t.Fatalf("Run returned a transport error: %v", err)
	}
	if got := readBack(t, path); got != existingContent {
		t.Fatalf("null content destroyed the file: holds %q, want %q (tool said %q)", got, existingContent, out)
	}
}

// The batch arm is the worst case on the card: one good write and one destroyed
// file inside a single success message that reads entirely normal.
func TestWriteFiles_BatchEntryWithoutContentDoesNotDestroyItsNeighbor(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	victim := writeVictim(t, dir, "victim.txt")

	raw := `{"files":[{"path":` + quote(good) + `,"content":"NEW"},{"path":` + quote(victim) + `}]}`
	out, err := WriteFile().Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("Run returned a transport error: %v", err)
	}
	if got := readBack(t, victim); got != existingContent {
		t.Fatalf("batch entry without content destroyed the file: holds %q, want %q (tool said %q)", got, existingContent, out)
	}
	if got := readBack(t, good); got != "NEW" {
		t.Errorf("the well-formed entry in the same batch did not land: holds %q, want %q", got, "NEW")
	}
}

// An explicit empty string is a real request and keeps its current meaning. This
// is the line that stops the repair from quietly answering the question the card
// says not to decide here.
func TestWriteFiles_ExplicitEmptyContentStillTruncates(t *testing.T) {
	dir := t.TempDir()
	path := writeVictim(t, dir, "victim.txt")

	out, err := WriteFile().Run(context.Background(), `{"path":`+quote(path)+`,"content":""}`)
	if err != nil {
		t.Fatalf("Run returned a transport error: %v", err)
	}
	if got := readBack(t, path); got != "" {
		t.Fatalf("explicit empty content no longer truncates: holds %q (tool said %q)", got, out)
	}
	if !strings.Contains(out, "wrote 0 bytes") {
		t.Errorf("deliberate truncation is not reported as a write: %q", out)
	}
}

func TestWriteFiles_ExplicitEmptyContentInABatchStillTruncates(t *testing.T) {
	dir := t.TempDir()
	path := writeVictim(t, dir, "victim.txt")

	raw := `{"files":[{"path":` + quote(path) + `,"content":""}]}`
	if _, err := WriteFile().Run(context.Background(), raw); err != nil {
		t.Fatalf("Run returned a transport error: %v", err)
	}
	if got := readBack(t, path); got != "" {
		t.Fatalf("explicit empty content in a batch no longer truncates: holds %q", got)
	}
}
