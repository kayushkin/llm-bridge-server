package hookstore

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

const standaloneHooksSchema = `CREATE TABLE hooks (id TEXT PRIMARY KEY, harness TEXT NOT NULL, event TEXT NOT NULL,
	matcher TEXT NOT NULL DEFAULT '', command TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`

func writeStandaloneHooksFile(t *testing.T, extraStatements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, statement := range append([]string{standaloneHooksSchema,
		`INSERT INTO hooks VALUES ('01HOOKGLOBAL', 'claude_code', 'PreToolUse', 'Bash', '/usr/local/bin/check', 'global', '', 1,
			'2026-04-23 10:00:00.123456789 +0000 UTC', '2026-04-23 10:00:00.123456789 +0000 UTC')`,
		`INSERT INTO hooks VALUES ('01HOOKSESSION', 'codex', 'Stop', '', 'echo done', 'session', 'br_1', 0,
			'2026-04-23 11:00:00', '2026-04-24 11:00:00')`,
	}, extraStatements...) {
		if _, err := source.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestImportCopiesEveryHookAsItIs(t *testing.T) {
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	counts, err := ImportStandaloneDatabase(context.Background(), db, writeStandaloneHooksFile(t))
	if err != nil || counts != (ImportCounts{Hooks: 2, DisabledHooks: 1}) {
		t.Fatalf("import: %+v %v", counts, err)
	}
	store := New(db)
	hook, err := store.GetHook("01HOOKSESSION")
	if err != nil {
		t.Fatal(err)
	}
	if hook.Enabled || hook.ScopeID != "br_1" || !hook.UpdatedAt.Equal(time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("hook: %+v", hook)
	}
	global, err := store.GetHook("01HOOKGLOBAL")
	if err != nil || !global.Enabled || !global.CreatedAt.Equal(time.Date(2026, 4, 23, 10, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("global hook: %+v %v", global, err)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), db, writeStandaloneHooksFile(t)); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesAHooksFileItCannotCopyWhole(t *testing.T) {
	for name, statement := range map[string]string{
		"an unknown table":  `CREATE TABLE hook_runs (id TEXT)`,
		"an unknown column": `ALTER TABLE hooks ADD COLUMN timeout_seconds INTEGER`,
	} {
		t.Run(name, func(t *testing.T) {
			module, err := Module()
			if err != nil {
				t.Fatal(err)
			}
			db := postgrestest.NewDatabase(t, module).Open(module)
			if _, err := ImportStandaloneDatabase(context.Background(), db, writeStandaloneHooksFile(t, statement)); err == nil {
				t.Fatal("import took a file it cannot copy whole")
			}
		})
	}
}
