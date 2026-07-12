package server

import (
	"os"
	"path/filepath"
	"strings"
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

	state.commitRun(&msg.ConformanceMatrix{GeneratedAt: time.Now()}, runOutcome{
		missingBinaries: []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex},
	})

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
}

// The reason a run tests nothing is that no wrapper binary was on PATH — those
// harnesses are skipped, never launched, so none of them ever produces an error
// string. Reporting only launch errors left last_run_error empty in exactly the
// case it exists for, and a destructive run read as a clean one.
func TestCommitRunReportsThatNoWrapperBinaryWasOnPath(t *testing.T) {
	state := &conformanceState{path: filepath.Join(t.TempDir(), "conformance.json"), matrix: goodMatrix()}

	state.commitRun(&msg.ConformanceMatrix{GeneratedAt: time.Now()}, runOutcome{
		missingBinaries: []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex},
	})

	if state.lastRunError == "" {
		t.Fatal("lastRunError is empty; a run that tested nothing is indistinguishable from a clean run")
	}
	for _, want := range []string{string(msg.HarnessClaudeCode), string(msg.HarnessCodex)} {
		if !strings.Contains(state.lastRunError, want) {
			t.Errorf("lastRunError does not name the skipped harness %q: got %q", want, state.lastRunError)
		}
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
	state.commitRun(fresh, runOutcome{harnessErrors: []string{"nanoclaw: image not found"}})

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

	if !strings.Contains(state.lastRunError, "nanoclaw: image not found") {
		t.Errorf("harness launch error was dropped: got %q", state.lastRunError)
	}
}

// A partial run is the same failure as an empty one, scaled down: a harness
// whose binary vanished was never tested, so committing the run throws away the
// results it last reported. That loss has to be visible.
func TestCommitRunReportsDiscardedResults(t *testing.T) {
	state := &conformanceState{path: filepath.Join(t.TempDir(), "conformance.json"), matrix: goodMatrix()}

	// goodMatrix holds claudecode's results; this run only reached aider.
	state.commitRun(&msg.ConformanceMatrix{
		GeneratedAt: time.Now(),
		Harnesses:   []msg.ConformanceHarnessResult{{Harness: "aider"}},
	}, runOutcome{missingBinaries: []msg.Harness{msg.HarnessClaudeCode}})

	if !strings.Contains(state.lastRunError, string(msg.HarnessClaudeCode)) {
		t.Errorf("claudecode's last-good results were discarded without a word: got %q", state.lastRunError)
	}
}

// A harness with no results to lose is not worth reporting: harnesses that were
// never installed are the steady state, and crying wolf about them on every run
// would make last_run_error useless as a signal.
func TestCommitRunStaysQuietWhenNoResultsAreLost(t *testing.T) {
	state := &conformanceState{path: filepath.Join(t.TempDir(), "conformance.json"), matrix: goodMatrix()}

	state.commitRun(&msg.ConformanceMatrix{
		GeneratedAt: time.Now(),
		Harnesses:   []msg.ConformanceHarnessResult{{Harness: "claudecode"}, {Harness: "aider"}},
	}, runOutcome{missingBinaries: []msg.Harness{msg.HarnessGoose}})

	if state.lastRunError != "" {
		t.Errorf("a never-installed harness was reported as an error: got %q", state.lastRunError)
	}
}

// A clean run clears the error from the previous failed run.
func TestCommitRunClearsPreviousRunError(t *testing.T) {
	state := &conformanceState{path: filepath.Join(t.TempDir(), "conformance.json")}
	state.commitRun(&msg.ConformanceMatrix{}, runOutcome{
		missingBinaries: []msg.Harness{msg.HarnessCodex},
	})
	if state.lastRunError == "" {
		t.Fatal("setup: expected the failed run to record an error")
	}

	state.commitRun(&msg.ConformanceMatrix{
		Harnesses: []msg.ConformanceHarnessResult{{Harness: "aider"}},
	}, runOutcome{})

	if state.lastRunError != "" {
		t.Errorf("stale error survived a clean run: got %q", state.lastRunError)
	}
}

// Wrapper binaries resolve through exec.LookPath, so an empty PATH is the exact
// condition that empties a conformance run. Every enabled harness must come back
// as a skipped harness the caller can report, not be dropped on the floor.
func TestPlanConformanceRunReportsEveryHarnessSkippedForAnEmptyPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	targets, outcome := planConformanceRun()

	if len(targets) != 0 {
		t.Fatalf("resolved %d harness binaries against an empty PATH: %+v", len(targets), targets)
	}
	if len(outcome.missingBinaries) != len(allHarnesses) {
		t.Fatalf("skipped harnesses went unrecorded: got %d, want %d (every enabled harness)",
			len(outcome.missingBinaries), len(allHarnesses))
	}
}

// The counterpart: a harness whose binary is on PATH is planned as a target, so
// a degraded run still tests everything it can reach.
func TestPlanConformanceRunFindsHarnessOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, msg.HarnessBinaryName(msg.HarnessClaudeCode))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("stub wrapper binary: %v", err)
	}
	t.Setenv("PATH", dir)

	targets, outcome := planConformanceRun()

	if len(targets) != 1 || targets[0].harness != msg.HarnessClaudeCode {
		t.Fatalf("the one harness on PATH was not planned as a target: %+v", targets)
	}
	if targets[0].binary != bin {
		t.Errorf("target resolved to the wrong path: got %q, want %q", targets[0].binary, bin)
	}
	for _, h := range outcome.missingBinaries {
		if h == msg.HarnessClaudeCode {
			t.Error("claude_code is on PATH but was reported as missing")
		}
	}
}
