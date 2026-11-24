package memorystore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// FindRecentlyModified answers "which files changed inside this window" twice,
// and until this file nothing put a file anywhere near the edge of it:
//
//	recency.go:107  findRecentlyModifiedGit    time.Since(info.ModTime()) <= since
//	recency.go:146  findRecentlyModifiedMtime  info.ModTime().After(cutoff)
//
// Both were executed on every run of the suite. `prepare_test.go` straddles the
// window at 2 hours and 48 hours against a 24-hour setting — a 46-hour gap the
// window could be moved anywhere inside — and the one test that reaches the git
// strategy commits a file and scans it milliseconds later against 24 hours. So
// the mechanism was exercised, the window was named in the fixtures, and its
// position was pinned by nothing. Measured, both strategies mutated together:
//
//	window -> since/2   SURVIVED
//	window -> since*2   SURVIVED
//
// Card `3c18632a`: a suite can name an axis, exercise it on every row, and still
// have every row land on the same side of the cut.
//
// The rows below sit one minute either side of a two-hour window, so halving or
// doubling it changes an answer in each strategy separately.

// The window is deliberately short. A test that straddles a 24-hour window needs
// files dated a day back, and `os.Chtimes` can write those — but the two
// strategies disagree about what a *stale* file means, and keeping the window
// near the fixture's own timescale keeps the reason a row is excluded down to
// one thing: the window.
const probeWindow = 2 * time.Hour

// oneMinute is the straddle margin. It has to outlast the test's own runtime,
// since `time.Since` keeps moving while the scan walks, and it has to be small
// against `probeWindow` so that halving or doubling the window moves the answer.
// A minute is ~4 orders of magnitude over the scan and 1/120th of the window.
const straddleMargin = time.Minute

// writeFileAged writes a file and backdates it to exactly age old.
func writeFileAged(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("content of "+name), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	modTime := time.Now().Add(-age)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

func relativePathsOf(files []RecentFile) []string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.RelativePath)
	}
	sort.Strings(paths)
	return paths
}

// TestTheMtimeScanStraddlesItsWindow pins the window in findRecentlyModifiedMtime.
//
// A plain directory is not a git repository, so findRecentlyModifiedGit fails to
// stat `.git` and FindRecentlyModified falls through to the walk. The assertion
// is on the exact set, not on membership: "the fresh one is present" stays true
// when the window widens to admit the stale one too.
func TestTheMtimeScanStraddlesItsWindow(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "inside.go", probeWindow-straddleMargin)
	writeFileAged(t, root, "outside.go", probeWindow+straddleMargin)

	files, err := FindRecentlyModified(context.Background(), root, probeWindow)
	if err != nil {
		t.Fatalf("FindRecentlyModified: %v", err)
	}
	for _, f := range files {
		if f.Source != "mtime" {
			t.Fatalf("this test aims at the mtime strategy, but %s came from %q", f.RelativePath, f.Source)
		}
	}

	got := relativePathsOf(files)
	if len(got) != 1 || got[0] != "inside.go" {
		t.Errorf("a %v window over files aged %v and %v\n  got  %v\n  want [inside.go]",
			probeWindow, probeWindow-straddleMargin, probeWindow+straddleMargin, got)
	}
}

// TestTheGitScanStraddlesItsWindow pins the window in findRecentlyModifiedGit.
//
// The git strategy has two filters and only the second one is under test here.
// `git log --since` selects by COMMIT time, and both files are committed now, so
// both survive it and reach the mtime comparison at recency.go:107 — which is
// the cut this test is about. Backdating the file does not backdate the commit,
// which is what makes the two separable at all.
func TestTheGitScanStraddlesItsWindow(t *testing.T) {
	root := t.TempDir()
	writeFileAged(t, root, "inside.go", probeWindow-straddleMargin)
	writeFileAged(t, root, "outside.go", probeWindow+straddleMargin)

	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"add", "inside.go", "outside.go"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--quiet", "-m", "add both files"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", arguments, err, output)
		}
	}
	// `git add` and `git commit` rewrite neither file's mtime, but a checkout or
	// a filter would. Re-assert the ages the rest of this test rests on.
	for name, wantAge := range map[string]time.Duration{
		"inside.go":  probeWindow - straddleMargin,
		"outside.go": probeWindow + straddleMargin,
	} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if drift := time.Since(info.ModTime()) - wantAge; drift < 0 || drift > time.Second {
			t.Fatalf("committing moved %s's mtime: it is now %v old, want %v",
				name, time.Since(info.ModTime()).Round(time.Second), wantAge)
		}
	}

	files, err := FindRecentlyModified(context.Background(), root, probeWindow)
	if err != nil {
		t.Fatalf("FindRecentlyModified: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the git strategy returned nothing, so this test would pass with the window anywhere")
	}
	for _, f := range files {
		if f.Source != "git" {
			t.Fatalf("this test aims at the git strategy, but %s came from %q", f.RelativePath, f.Source)
		}
	}

	got := relativePathsOf(files)
	if len(got) != 1 || got[0] != "inside.go" {
		t.Errorf("a %v window over committed files aged %v and %v\n  got  %v\n  want [inside.go]",
			probeWindow, probeWindow-straddleMargin, probeWindow+straddleMargin, got)
	}
}
