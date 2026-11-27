package snapshots

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

func writeStandaloneSnapshotsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshots.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, statement := range []string{
		`CREATE TABLE snapshots (session_id TEXT NOT NULL, tool_use_id TEXT NOT NULL, phase TEXT NOT NULL, file_path TEXT NOT NULL,
			blob_sha TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, is_binary INTEGER NOT NULL DEFAULT 0,
			too_large INTEGER NOT NULL DEFAULT 0, missing INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (session_id, tool_use_id, phase, file_path))`,
		`INSERT INTO snapshots VALUES ('br_1', 'toolu_1', 'before', '/work/a.txt', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 5, 0, 0, 0,
			'2026-10-08 08:42:00.123456789 +0000 UTC')`,
		`INSERT INTO snapshots VALUES ('br_1', 'toolu_1', 'after', '/work/a.txt', '', 0, 0, 0, 1, '2026-10-08 08:42:01 +0000 UTC')`,
	} {
		if _, err := source.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestImportCopiesEverySnapshotRowAsItIs(t *testing.T) {
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	path := writeStandaloneSnapshotsFile(t)
	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil || counts != (ImportCounts{Snapshots: 2, DistinctBlobs: 1}) {
		t.Fatalf("import: %+v %v", counts, err)
	}
	store, err := New(db, Config{GitDir: filepath.Join(t.TempDir(), "snapshots.git")})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.Get("br_1", "toolu_1")
	if err != nil || len(snapshots) != 2 {
		t.Fatalf("get: %+v %v", snapshots, err)
	}
	before := findPhase(snapshots, "/work/a.txt", PhaseBefore)
	after := findPhase(snapshots, "/work/a.txt", PhaseAfter)
	if before.Size != 5 || before.BlobSHA == "" || !before.CreatedAt.Equal(time.Date(2026, 10, 8, 8, 42, 0, 123456000, time.UTC)) || !after.Missing {
		t.Fatalf("snapshots: %+v %+v", before, after)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesAFileOnTheFirstSchema(t *testing.T) {
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	firstSchema := filepath.Join(t.TempDir(), "first.db")
	source, err := sql.Open("sqlite", firstSchema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`CREATE TABLE snapshots (session_id TEXT NOT NULL, tool_use_id TEXT NOT NULL, phase TEXT NOT NULL, file_path TEXT NOT NULL,
		blob_sha TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, is_binary INTEGER NOT NULL DEFAULT 0,
		too_large INTEGER NOT NULL DEFAULT 0, missing INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (session_id, tool_use_id, phase))`); err != nil {
		t.Fatal(err)
	}
	source.Close()
	if _, err := ImportStandaloneDatabase(context.Background(), db, firstSchema); err == nil || !strings.Contains(err.Error(), "first schema") {
		t.Fatalf("import of a first-schema file: %v", err)
	}
}
