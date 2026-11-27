package memories

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// walkableTreeFileCount is large enough that walking the tree costs orders of
// magnitude more than refusing to walk it, so a test can tell the two apart by
// measurement rather than by inspecting the code.
const walkableTreeFileCount = 12000

// newTreeWithoutGit builds a directory of freshly-created files with no git
// history, which is the workspace shape that sends FindRecentlyModified down
// its expensive path.
func newTreeWithoutGit(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	const filesPerDirectory = 300
	for directory := 0; directory < walkableTreeFileCount/filesPerDirectory; directory++ {
		path := filepath.Join(root, fmt.Sprintf("d%03d", directory))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create directory: %v", err)
		}
		for file := 0; file < filesPerDirectory; file++ {
			name := filepath.Join(path, fmt.Sprintf("f%03d.txt", file))
			if err := os.WriteFile(name, nil, 0o644); err != nil {
				t.Fatalf("create file: %v", err)
			}
		}
	}
	return root
}

// newRepositoryWithARecentCommit builds a git repository whose log answers the
// recency query with a file, so a test can tell a git scan that ran from one
// that was refused.
func newRepositoryWithARecentCommit(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "committed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"add", "committed.go"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--quiet", "-m", "add a file"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", arguments, err, output)
		}
	}
	return root
}

// A canceled context must reach the git subprocess. Before it did, the only
// thing that ended a git log was git finishing.
func TestFindRecentlyModifiedRefusesToRunGitForACanceledCaller(t *testing.T) {
	root := newRepositoryWithARecentCommit(t)

	liveFiles, err := FindRecentlyModified(context.Background(), root, 24*time.Hour)
	if err != nil {
		t.Fatalf("scan with a live context: %v", err)
	}
	if len(liveFiles) == 0 {
		t.Fatal("git scan returned no files, so a canceled scan returning none would prove nothing")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	files, err := FindRecentlyModified(canceled, root, 24*time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled scan returned err %v, want context.Canceled", err)
	}
	if len(files) != 0 {
		t.Errorf("canceled scan returned %d files, want none", len(files))
	}
}

// The mtime walk is the fallback the git scan falls into when it fails, and a
// canceled git scan fails — so before either context check existed, canceling
// the cheap strategy is what started the expensive one.
//
// This pins the two checks jointly. Removing either one alone leaves the
// property intact, which is measured and written down where each is declared.
func TestACanceledScanDoesNotWalkTheTree(t *testing.T) {
	root := newTreeWithoutGit(t)

	start := time.Now()
	files, err := FindRecentlyModified(context.Background(), root, 24*time.Hour)
	fullWalk := time.Since(start)
	if err != nil {
		t.Fatalf("scan with a live context: %v", err)
	}
	if len(files) != walkableTreeFileCount {
		t.Fatalf("live scan found %d files, want %d — the fallback must still run when git cannot answer", len(files), walkableTreeFileCount)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	start = time.Now()
	files, err = FindRecentlyModified(canceled, root, 24*time.Hour)
	refused := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled scan returned err %v, want context.Canceled", err)
	}
	if len(files) != 0 {
		t.Errorf("canceled scan returned %d files, want none", len(files))
	}
	if refused > fullWalk/10 {
		t.Errorf("canceled scan took %v against a %v walk of the same tree, so it walked it", refused, fullWalk)
	}
}

func TestTreeWalkStopsForACanceledCaller(t *testing.T) {
	root := newTreeWithoutGit(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	files, err := findRecentlyModifiedMtime(canceled, root, 24*time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled walk returned err %v, want context.Canceled", err)
	}
	if len(files) != 0 {
		t.Errorf("canceled walk returned %d files, want none — a truncated scan is not a short list", len(files))
	}
}

// Cancellation has to be noticed while the walk is running, not only before it
// starts: the whole cost of the walk is in the middle of it.
func TestTreeWalkStopsPartWayThrough(t *testing.T) {
	root := newTreeWithoutGit(t)

	start := time.Now()
	if _, err := findRecentlyModifiedMtime(context.Background(), root, 24*time.Hour); err != nil {
		t.Fatalf("walk with a live context: %v", err)
	}
	fullWalk := time.Since(start)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(fullWalk/10, cancel)

	start = time.Now()
	files, err := findRecentlyModifiedMtime(ctx, root, 24*time.Hour)
	stopped := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("walk canceled mid-flight returned err %v, want context.Canceled", err)
	}
	if len(files) != 0 {
		t.Errorf("walk canceled mid-flight returned %d files, want none", len(files))
	}
	if stopped >= fullWalk {
		t.Errorf("walk canceled mid-flight took %v, no less than the %v it takes to finish", stopped, fullWalk)
	}
}

func TestRecencyScanContextKeepsWhicheverDeadlineIsEarlier(t *testing.T) {
	callerDeadline := time.Now().Add(20 * time.Millisecond)
	caller, cancelCaller := context.WithDeadline(context.Background(), callerDeadline)
	defer cancelCaller()

	scan, endScan := recencyScanContext(caller, time.Hour)
	defer endScan()

	deadline, ok := scan.Deadline()
	if !ok {
		t.Fatal("scan context has no deadline")
	}
	if !deadline.Equal(callerDeadline) {
		t.Errorf("scan deadline is %v, want the caller's %v — a caller who allowed less must not be overridden", deadline, callerDeadline)
	}
}

func TestRecencyScanContextAppliesItsOwnBoundWhenTheCallerHasNone(t *testing.T) {
	scan, endScan := recencyScanContext(context.Background(), 0)
	defer endScan()

	deadline, ok := scan.Deadline()
	if !ok {
		t.Fatal("scan context has no deadline, so a caller without one leaves the scan unbounded")
	}
	if remaining := time.Until(deadline); remaining > DefaultRecencyScanTimeout {
		t.Errorf("scan deadline is %v away, want at most the default %v", remaining, DefaultRecencyScanTimeout)
	}
}

func TestRecencyScanContextHonorsAnExplicitlyDisabledBound(t *testing.T) {
	scan, endScan := recencyScanContext(context.Background(), -1)
	defer endScan()

	if _, ok := scan.Deadline(); ok {
		t.Error("scan context has a deadline, want none — a negative timeout asks for no bound")
	}
}
