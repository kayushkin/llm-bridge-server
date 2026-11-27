package memories

import (
	"context"
	"database/sql"
	_ "embed"
	"flag"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// writeStandaloneDatabase makes a standalone store's SQLite file from schema
// and then rows, and returns its path.
func writeStandaloneDatabase(t *testing.T, schema, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	if _, err := db.Exec(rows); err != nil {
		t.Fatalf("standalone rows: %v", err)
	}
	return path
}

// standaloneRows is a small made-up store. Memory "gone" was forgotten
// (importance 0), "brief" expires and has no summary, "who" is always loaded.
// Tags are written in an order that is not alphabetical. Usage 3 and 4 were
// deleted, and the AUTOINCREMENT counter went to 7, so the next usage is 8.
const standaloneRows = `
	INSERT INTO memories (id, content, summary, original_id, importance, access_count, last_accessed, created_at, source, embedding,
		always_load, expires_at, tokens, ref_type, ref_target, is_lazy, orchestrator)
	VALUES
		('who', 'the operator prefers short answers', 'short answers', NULL, 0.9, 3, 200, 100, 'user', CAST('[0.5,0.25]' AS BLOB),
			1, NULL, 12, 'identity', NULL, 0, 'inber'),
		('brief', 'the scheduler runs a job on a cron expression', NULL, NULL, 0.5, 0, 300, 300, 'agent', CAST('[]' AS BLOB),
			0, 4102444800, 15, 'memory', NULL, 0, ''),
		('gone', 'plex thumbnails are cached on disk', NULL, 'brief', 0, 1, 150, 120, 'compaction', NULL,
			0, NULL, 11, 'file', '/srv/example/notes.md', 1, '');
	INSERT INTO memory_tags (memory_id, tag) VALUES ('who', 'preference'), ('who', 'identity'), ('brief', 'scheduler');
	INSERT INTO memory_usage (id, memory_id, session_id, turn_number, usage_type, accessed_at) VALUES
		(1, 'who', 'br_1', 1, 'context', 210), (2, 'brief', 'br_1', 2, 'search', 310), (5, 'who', 'br_2', 1, 'context', 400);
	UPDATE sqlite_sequence SET seq = 7 WHERE name = 'memory_usage';`

func TestImportStandaloneDatabaseCopiesEveryRowAndKeepsItsIDs(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema, standaloneRows)
	store := openTestStore(t)

	counts, err := ImportStandaloneDatabase(context.Background(), store.db, path)
	if err != nil {
		t.Fatal(err)
	}
	want := ImportCounts{Memories: 3, Tags: 3, Usages: 3, LastUsageNumber: 7, ForgottenMemories: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	type row struct {
		ID, Content                    string
		Summary, OriginalID, RefTarget sql.NullString
		Importance                     float64
		AccessCount, LastAccessed      int64
		CreatedAt, Tokens              int64
		Source, RefType, Orchestrator  string
		Embedding                      []byte
		AlwaysLoad, IsLazy             bool
		ExpiresAt                      sql.NullInt64
	}
	read := func(id string) row {
		var r row
		if err := store.db.QueryRow(`SELECT id, content, summary, original_id, ref_target, importance, access_count, last_accessed,
			created_at, tokens, source, ref_type, orchestrator, embedding, always_load, is_lazy, expires_at FROM memories WHERE id = $1`, id).
			Scan(&r.ID, &r.Content, &r.Summary, &r.OriginalID, &r.RefTarget, &r.Importance, &r.AccessCount, &r.LastAccessed,
				&r.CreatedAt, &r.Tokens, &r.Source, &r.RefType, &r.Orchestrator, &r.Embedding, &r.AlwaysLoad, &r.IsLazy, &r.ExpiresAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}
	if got, want := read("who"), (row{ID: "who", Content: "the operator prefers short answers", Summary: sql.NullString{String: "short answers", Valid: true},
		Importance: 0.9, AccessCount: 3, LastAccessed: 200, CreatedAt: 100, Tokens: 12, Source: "user", RefType: "identity", Orchestrator: "inber",
		Embedding: []byte("[0.5,0.25]"), AlwaysLoad: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("who = %+v, want %+v", got, want)
	}
	if got, want := read("brief"), (row{ID: "brief", Content: "the scheduler runs a job on a cron expression", Importance: 0.5,
		LastAccessed: 300, CreatedAt: 300, Tokens: 15, Source: "agent", RefType: "memory", Embedding: []byte("[]"),
		ExpiresAt: sql.NullInt64{Int64: 4102444800, Valid: true}}); !reflect.DeepEqual(got, want) {
		t.Errorf("brief = %+v, want %+v", got, want)
	}
	if got, want := read("gone"), (row{ID: "gone", Content: "plex thumbnails are cached on disk", OriginalID: sql.NullString{String: "brief", Valid: true},
		RefTarget: sql.NullString{String: "/srv/example/notes.md", Valid: true}, AccessCount: 1, LastAccessed: 150, CreatedAt: 120, Tokens: 11,
		Source: "compaction", RefType: "file", IsLazy: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("gone = %+v, want %+v", got, want)
	}

	tags, err := store.loadTagsForMemories([]string{"who", "brief"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tags["who"], []string{"preference", "identity"}) || !reflect.DeepEqual(tags["brief"], []string{"scheduler"}) {
		t.Errorf("tags = %v, want who [preference identity] and brief [scheduler], in the order written", tags)
	}

	// An imported memory is searchable at once, and a forgotten one is not.
	if got := searchIDs(t, store, "scheduler", 10); !reflect.DeepEqual(got, []string{"brief"}) {
		t.Errorf("search scheduler = %v, want [brief]", got)
	}
	if got := searchIDs(t, store, "thumbnails", 10); len(got) != 0 {
		t.Errorf("search thumbnails = %v, want nothing: that memory was forgotten", got)
	}

	// Usage rows keep their numbers, and the next one takes the number after
	// the last the standalone store handed out.
	var usageIDs []int64
	rows, err := store.db.Query(`SELECT id FROM memory_usage ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		usageIDs = append(usageIDs, id)
	}
	rows.Close()
	if !reflect.DeepEqual(usageIDs, []int64{1, 2, 5}) {
		t.Errorf("usage ids = %v, want [1 2 5]", usageIDs)
	}
	if err := store.TrackMemoryUsage("who", "br_3", 1, "context"); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err := store.db.QueryRow(`SELECT max(id) FROM memory_usage`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 8 {
		t.Errorf("the next usage took number %d, want 8", next)
	}
}

func TestImportStandaloneDatabaseRefusesAModuleThatHoldsRows(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema, standaloneRows)
	store := openTestStore(t)
	saveMemory(t, store, "already", "a memory written before the import")

	_, err := ImportStandaloneDatabase(context.Background(), store.db, path)
	if err == nil || !strings.Contains(err.Error(), "already holds 1 rows in memories") {
		t.Fatalf("import into a module holding a memory: %v, want a refusal", err)
	}
}

func TestImportStandaloneDatabaseRefusesWhatItCannotCopyWhole(t *testing.T) {
	cases := []struct {
		name, schema, rows, refusal string
	}{
		{"a column it does not know", standaloneSchema + `ALTER TABLE memories ADD COLUMN mood TEXT;`, standaloneRows, "has columns"},
		{"a table it does not know", standaloneSchema + `CREATE TABLE sessions (id TEXT PRIMARY KEY);`, standaloneRows, "has a table sessions"},
		{"a flag that is not 0 or 1", standaloneSchema, standaloneRows + `UPDATE memories SET always_load = 2 WHERE id = 'who';`, "always_load is 2"},
		{"a tag of a memory the file does not hold", standaloneSchema, standaloneRows + `INSERT INTO memory_tags (memory_id, tag) VALUES ('nobody', 'x');`, "memory_tags"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeStandaloneDatabase(t, c.schema, c.rows)
			store := openTestStore(t)
			_, err := ImportStandaloneDatabase(context.Background(), store.db, path)
			if err == nil || !strings.Contains(err.Error(), c.refusal) {
				t.Fatalf("import: %v, want a refusal naming %q", err, c.refusal)
			}
			var held int
			if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM memories) + (SELECT count(*) FROM memory_tags) + (SELECT count(*) FROM memory_usage)`).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if held != 0 {
				t.Fatalf("a refused import left %d rows behind", held)
			}
		})
	}
}

// standaloneCopy is a copy (sqlite3 .backup) of a live standalone store's
// database, for checking the import against real data:
//
//	go test -run TestImportOfAStandaloneCopy ./internal/agents/memories -args -standalone-memory-store-copy=<file>
//
// It imports the copy into a test database and prints the counts.
var standaloneCopy = flag.String("standalone-memory-store-copy", "", "a copy of a standalone memory-store database to import")

func TestImportOfAStandaloneCopy(t *testing.T) {
	if *standaloneCopy == "" {
		t.Skip("no -standalone-memory-store-copy given")
	}
	store := openTestStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, *standaloneCopy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("imported %+v", counts)
	var memories, tags, usages int64
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM memories), (SELECT count(*) FROM memory_tags), (SELECT count(*) FROM memory_usage)`).Scan(&memories, &tags, &usages); err != nil {
		t.Fatal(err)
	}
	if memories != counts.Memories || tags != counts.Tags || usages != counts.Usages {
		t.Fatalf("the module holds %d memories, %d tags and %d usages, and the import says it copied %+v", memories, tags, usages, counts)
	}
}
