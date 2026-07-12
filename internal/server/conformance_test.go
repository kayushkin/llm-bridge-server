package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func goodMatrix() *msg.ConformanceMatrix {
	return &msg.ConformanceMatrix{
		GeneratedAt: time.Date(2026, 6, 11, 18, 11, 47, 0, time.UTC),
		Harnesses: []msg.ConformanceHarnessResult{
			{Harness: "claudecode", Binary: "/home/user/bin/llm-bridge-claudecode"},
		},
	}
}

// A run where every harness failed to launch produced no information. It must
// not overwrite the last-good matrix, on disk or in memory.
func TestCommitRunKeepsLastGoodMatrixWhenNoHarnessRan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conformance.json")
	state := &conformanceState{path: path, matrix: goodMatrix()}
	state.save()

	state.commitRun(&msg.ConformanceMatrix{GeneratedAt: time.Now()},
		[]string{"claudecode: exec: no such file", "codex: exec: no such file"})

	if got := len(state.matrix.Harnesses); got != 1 {
		t.Fatalf("in-memory matrix was clobbered by a zero-result run: got %d harnesses, want 1", got)
	}
	if !state.matrix.GeneratedAt.Equal(goodMatrix().GeneratedAt) {
		t.Fatalf("generated_at was overwritten: got %s, want %s",
			state.matrix.GeneratedAt, goodMatrix().GeneratedAt)
	}

	reloaded := &conformanceState{path: path}
	reloaded.load()
	if reloaded.matrix == nil || len(reloaded.matrix.Harnesses) != 1 {
		t.Fatalf("persisted matrix was clobbered by a zero-result run: %+v", reloaded.matrix)
	}

	if state.lastRunError == "" {
		t.Error("lastRunError is empty; the failed run left no trace for the caller")
	}
}

// A run that did produce results is the new truth and replaces the old one,
// even if some harnesses failed to launch along the way.
func TestCommitRunReplacesMatrixWhenHarnessesRan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conformance.json")
	state := &conformanceState{path: path, matrix: goodMatrix()}
	state.save()

	fresh := &msg.ConformanceMatrix{
		GeneratedAt: time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC),
		Harnesses: []msg.ConformanceHarnessResult{
			{Harness: "aider"},
			{Harness: "codex"},
		},
	}
	state.commitRun(fresh, []string{"nanoclaw: image not found"})

	if got := len(state.matrix.Harnesses); got != 2 {
		t.Fatalf("fresh results were not committed: got %d harnesses, want 2", got)
	}

	reloaded := &conformanceState{path: path}
	reloaded.load()
	if reloaded.matrix == nil || len(reloaded.matrix.Harnesses) != 2 {
		t.Fatalf("fresh results were not persisted: %+v", reloaded.matrix)
	}
	if reloaded.matrix.Harnesses[0].Harness != "aider" {
		t.Fatalf("persisted the wrong matrix: %+v", reloaded.matrix.Harnesses)
	}

	if state.lastRunError != "nanoclaw: image not found" {
		t.Errorf("harness launch error was dropped: got %q", state.lastRunError)
	}
}

// A clean run clears the error from the previous failed run.
func TestCommitRunClearsPreviousRunError(t *testing.T) {
	state := &conformanceState{path: filepath.Join(t.TempDir(), "conformance.json")}
	state.commitRun(&msg.ConformanceMatrix{}, []string{"codex: exec: no such file"})
	if state.lastRunError == "" {
		t.Fatal("setup: expected the failed run to record an error")
	}

	state.commitRun(&msg.ConformanceMatrix{
		Harnesses: []msg.ConformanceHarnessResult{{Harness: "aider"}},
	}, nil)

	if state.lastRunError != "" {
		t.Errorf("stale error survived a clean run: got %q", state.lastRunError)
	}
}
