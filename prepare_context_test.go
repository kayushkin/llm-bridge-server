package memorystore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStoreForContextTest(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newWorkspaceWithOneRecentFile(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "recent.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return root
}

func prepareSessionConfigFor(root string) PrepareSessionConfig {
	return PrepareSessionConfig{
		RootDir:        root,
		IdentityText:   "I am a test agent",
		AgentName:      "test-agent",
		RecencyWindow:  24 * time.Hour,
		RecentFilesTTL: 10 * time.Minute,
	}
}

// A caller who has gone must not be told the session was prepared, and must not
// have memories written on its behalf on the way to being told so.
func TestPrepareSessionFailsForACanceledCaller(t *testing.T) {
	store := newStoreForContextTest(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.PrepareSession(canceled, prepareSessionConfigFor(newWorkspaceWithOneRecentFile(t)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareSession returned %v, want context.Canceled", err)
	}

	if identity, err := store.Get("identity"); err == nil && identity != nil {
		t.Error("identity was written for a caller that had already given up")
	}
}

// The complement, and the reason the check above reads the caller's context
// rather than the error: a scan that runs out of its own time leaves the
// session without recent-file hints, which is a degraded prepare and not a
// failed one.
func TestPrepareSessionSurvivesARecencyScanThatRunsOutOfTime(t *testing.T) {
	store := newStoreForContextTest(t)

	cfg := prepareSessionConfigFor(newWorkspaceWithOneRecentFile(t))
	cfg.RecencyScanTimeout = time.Nanosecond

	if err := store.PrepareSession(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareSession returned %v, want nil — a slow scan is survivable", err)
	}

	identity, err := store.Get("identity")
	if err != nil || identity == nil {
		t.Fatalf("identity not loaded after a timed-out scan: %v", err)
	}
}

// The fixture complement: with a live caller and the default bound, the scan
// still runs and still records what it found, so the two tests above cannot
// pass by the scan never producing anything.
func TestPrepareSessionRecordsRecentFilesWithinItsBound(t *testing.T) {
	store := newStoreForContextTest(t)

	root := newWorkspaceWithOneRecentFile(t)
	if err := store.PrepareSession(context.Background(), prepareSessionConfigFor(root)); err != nil {
		t.Fatalf("PrepareSession: %v", err)
	}

	recorded, err := store.Get("recent:recent.go")
	if err != nil || recorded == nil {
		t.Fatalf("recent file not recorded: %v", err)
	}
}
