package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// `findRecentlyModified` answers "which files changed inside this window" twice,
// by two strategies, and until this file nothing put anything near the edge of
// either one:
//
//	recent_files.go:100  findRecentlyModifiedGit    sinceTime := time.Now().Add(-since)
//	recent_files.go:146  findRecentlyModifiedMtime  cutoff    := time.Now().Add(-since)
//
// Both ran on every run of the suite. `cancellation_test.go` reaches them three
// times, always at `time.Hour`, in tests about cancellation — so the mechanism
// was exercised, the window was named in a fixture, and its position was pinned
// by nothing. Measured before this file landed, by
// `scripts/sabotage-recency-window.py`, each window moved on its own:
//
//	git   window -> since/2               UNNOTICED
//	git   window -> since*2               UNNOTICED
//	git   --since dropped altogether      UNNOTICED
//	mtime window -> since/2               UNNOTICED
//	mtime window -> since*2               UNNOTICED
//
// 0/5, with the known-positive control CAUGHT and the known-negative UNNOTICED.
// Card `319a55bf`, residual of `3c18632a`: a suite can name an axis, exercise it
// on every row, and still have every row land on the same side of the cut.
//
// The rows below sit one minute either side of a two-hour window, so halving or
// doubling it changes an answer in each strategy separately.

// The window is deliberately short. A test that straddles a 24-hour window needs
// fixtures dated a day back, and both `os.Chtimes` and a backdated commit can
// write those — but keeping the window near the fixture's own timescale keeps the
// reason a row is excluded down to one thing: the window.
const probeWindow = 2 * time.Hour

// straddleMargin is how far either side of the window the fixtures sit. It has to
// outlast the test's own runtime, since both strategies re-read the clock while
// they work, and it has to be small against probeWindow so that halving or
// doubling the window moves the answer. A minute is ~3 orders of magnitude over
// the scan and 1/120th of the window.
const straddleMargin = time.Minute

// The two strategies are driven directly rather than through
// `findRecentlyModified`. That entry point falls back from git to the mtime walk
// whenever git returns nothing, so a mutation that empties the git result does not
// produce an empty answer — it produces the *other strategy's* answer, over files
// this test wrote seconds ago. Going through the entry point would score that as a
// fixture falling over instead of as a window that moved.

func relativePathsOf(files []recentFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.RelativePath)
	}
	sort.Strings(paths)
	return paths
}

// writeFileAged lives in recent_files_test.go. Both files independently grew a
// helper of this name and this parameter order; the other one also MkdirAlls the
// nested paths its own callers need and returns the path it wrote, so it does
// everything this copy did and more. Two declarations in one package is a
// compile error, and the surviving one is the superset.

// commitAged commits the named files with both git dates set to exactly age old.
// `git log --since` reads the COMMITTER date, which is what makes this the knob
// the git strategy's window turns on.
func commitAged(t *testing.T, directory, message string, age time.Duration, names ...string) {
	t.Helper()
	stamp := time.Now().Add(-age).Format(time.RFC3339)
	for _, arguments := range [][]string{
		append([]string{"add"}, names...),
		{"commit", "--quiet", "-m", message},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = directory
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+stamp,
			"GIT_COMMITTER_DATE="+stamp,
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
}

func initEmptyRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = directory
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	return directory
}

// TestTheMtimeWalkStraddlesItsWindow pins the window in findRecentlyModifiedMtime.
//
// The assertion is on the exact set, not on membership: "the fresh one is present"
// stays true when the window widens to admit the stale one too, so a membership
// check would catch a halved window and miss a doubled one.
func TestTheMtimeWalkStraddlesItsWindow(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "inside.go", probeWindow-straddleMargin)
	writeFileAged(t, root, "outside.go", probeWindow+straddleMargin)

	files, err := findRecentlyModifiedMtime(root, probeWindow)
	if err != nil {
		t.Fatalf("findRecentlyModifiedMtime: %v", err)
	}

	got := relativePathsOf(files)
	if len(got) != 1 || got[0] != "inside.go" {
		t.Errorf("a %v window over files aged %v and %v\n  got  %v\n  want [inside.go]",
			probeWindow, probeWindow-straddleMargin, probeWindow+straddleMargin, got)
	}
}

// TestTheGitLogStraddlesItsWindow pins the window in findRecentlyModifiedGit.
//
// The two strategies do not filter on the same clock, and that is why they need
// separate fixtures rather than one shared one. `git log --since` selects on
// COMMIT time and tool-store's git strategy applies no second filter, so what
// straddles this window is two commits at different dates — not two files at
// different mtimes. Both files here are written at the same instant on purpose:
// if the mtime were doing the work, the fixture would be measuring the wrong cut.
func TestTheGitLogStraddlesItsWindow(t *testing.T) {
	root := initEmptyRepository(t)
	if err := os.WriteFile(filepath.Join(root, "outside.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAged(t, root, "committed before the window opens", probeWindow+straddleMargin, "outside.go")
	if err := os.WriteFile(filepath.Join(root, "inside.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAged(t, root, "committed inside the window", probeWindow-straddleMargin, "inside.go")

	files, err := findRecentlyModifiedGit(context.Background(), root, probeWindow)
	if err != nil {
		t.Fatalf("findRecentlyModifiedGit: %v", err)
	}

	got := relativePathsOf(files)
	if len(got) != 1 || got[0] != "inside.go" {
		t.Errorf("a %v window over commits made %v and %v ago\n  got  %v\n  want [inside.go]",
			probeWindow, probeWindow-straddleMargin, probeWindow+straddleMargin, got)
	}
}

// TestTheGitStrategyKeepsAFreshCommitWithAStaleFile records what this copy of the
// function does with a file whose commit is recent and whose mtime is not — a
// rebase, a checkout, a `git restore`, any history rewrite.
//
// ⚠️ This is a characterization test, not an endorsement. memory-store's copy of
// `FindRecentlyModified` re-filters the git result by mtime and would drop this
// file; tool-store's does not and keeps it. Same function name, same three
// arguments, different answers. Which is right is a product question — whether a
// recently-committed, long-untouched file counts as "recently modified" — and card
// `319a55bf` leaves it open deliberately. The test is here so that answering it
// has to redden something instead of passing silently.
func TestTheGitStrategyKeepsAFreshCommitWithAStaleFile(t *testing.T) {
	root := initEmptyRepository(t)
	if err := os.WriteFile(filepath.Join(root, "rewritten.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAged(t, root, "a commit inside the window", probeWindow-straddleMargin, "rewritten.go")

	// Backdate the file far outside the window, leaving the commit where it is.
	staleAge := 30 * 24 * time.Hour
	staleTime := time.Now().Add(-staleAge)
	if err := os.Chtimes(filepath.Join(root, "rewritten.go"), staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	files, err := findRecentlyModifiedGit(context.Background(), root, probeWindow)
	if err != nil {
		t.Fatalf("findRecentlyModifiedGit: %v", err)
	}

	got := relativePathsOf(files)
	if len(got) != 1 || got[0] != "rewritten.go" {
		t.Errorf("a file committed %v ago with an mtime %v old, against a %v window\n  got  %v\n  want [rewritten.go]",
			probeWindow-straddleMargin, staleAge, probeWindow, got)
	}
}
