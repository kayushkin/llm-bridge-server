package server

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Discovery decides a session is "new" by asking THIS instance's DB, then
// imports that session's whole transcript into log-store — a store the
// instance does not own and cannot clean up. So the only thing stopping a
// transcript from being written twice is a database log-store cannot see.
// Point the binary at a fresh DB and every session on disk is new again.
//
// That is not hypothetical. On 2026-08-01 a canary booted against an empty
// /tmp DB, with every isolation env var set except LLMBRIDGE_LOG_STORE_URL,
// re-imported 2,863 sessions into the production log-store in two minutes.
// The throwaway DB that could have identified them was deleted minutes
// later; the 13,226 events it wrote are permanent.
//
// The import cannot be made idempotent from here (log-store keys transcripts
// by bridge id, and discovery mints a fresh one every time), so what is
// pinned instead is the warning: discovery must name its log-store BEFORE it
// writes anything, because after the fact the operator gets one line per
// session — 2,510 of them that night — and not one names log-store.
//
// ⚠️ This test relies on the same thing the rest of this suite relies on:
// testServer's deliberately unreachable "http://localhost:0". That one
// string is all that stands between a full non-short test run and a repeat
// of the incident above. Do not point a test's LogStoreURL at a live host.
func TestDiscoverAnnouncesLogStoreBeforeImporting(t *testing.T) {
	if testing.Short() {
		t.Skip("discover spawns harness binaries; skipping in short mode")
	}

	const sentinelLogStore = "http://127.0.0.1:1/sentinel-log-store"

	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Unreachable on purpose: port 1 refuses immediately, so ImportHistory's
	// pushes fail at connect and no event reaches any real store.
	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     sentinelLogStore,
	}
	srv := New(st, nil, nil, nil, nil, nil, nil, cfg)

	var captured bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})

	resp := doJSON(t, srv, "GET", "/sessions/discover", nil)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	// Whether discovery found anything must be established WITHOUT reading
	// the log, or the check keys on the very string under test: delete the
	// announcement and a log-only detector reads the silence as "no sessions
	// to announce" and passes. The first version of this test did exactly
	// that and survived having its subject deleted. The rows discovery just
	// inserted are the independent witness.
	discovered, err := st.ListSessions()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(discovered) == 0 {
		t.Skip("no harness sessions discovered on this host; the announcement is not exercised")
	}

	lines := strings.Split(captured.String(), "\n")

	announcementLine := -1
	firstImportLine := -1
	for i, line := range lines {
		if announcementLine == -1 &&
			strings.Contains(line, "will have their transcripts imported into log-store") &&
			strings.Contains(line, sentinelLogStore) {
			announcementLine = i
		}
		if firstImportLine == -1 && strings.Contains(line, "imported") &&
			strings.Contains(line, "events for session") {
			firstImportLine = i
		}
	}

	if announcementLine == -1 {
		t.Fatalf("discovery adopted %d sessions without naming the log-store %q it imports their transcripts into; got:\n%s",
			len(discovered), sentinelLogStore, captured.String())
	}
	// Only reachable when a transcript actually imported, which needs a
	// log-store that answers. Kept because "warn after writing" is the
	// regression that would make the announcement worthless while leaving
	// the check above green.
	if firstImportLine != -1 && firstImportLine < announcementLine {
		t.Errorf("import line %d precedes the log-store announcement at line %d — the warning is useless after the write; got:\n%s",
			firstImportLine, announcementLine, captured.String())
	}
}
