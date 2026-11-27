package agents

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// standaloneFixture writes a SQLite file shaped as a standalone agent-store
// left it: every table the import copies, with the columns it expects,
// plus sqlite_sequence and an empty left-behind table. adjust runs against it
// before it is closed, to bend it into a file the import must refuse.
func standaloneFixture(t *testing.T, adjust func(db *sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, table := range standaloneTables {
		columns := append([]string{}, table.columns...)
		if table.identity != "" {
			// AUTOINCREMENT is what gives the file a sqlite_sequence.
			columns[0] = table.identity + " INTEGER PRIMARY KEY AUTOINCREMENT"
		}
		exec(`CREATE TABLE "` + table.name + `" (` + strings.Join(columns, ", ") + `)`)
	}
	exec(`CREATE TABLE orchestrators (id TEXT PRIMARY KEY, display_name TEXT)`)

	exec(`INSERT INTO agents VALUES (3, 'claxon', 'Claxon', '🦀', 'si,dash', 'the crab', 'lead', 1, 10, 11, 'ws', 7)`)
	exec(`INSERT INTO agents VALUES (7, 'brigid', 'Brigid', NULL, NULL, NULL, NULL, 0, 12, 13, '', NULL)`)
	exec(`INSERT INTO harness VALUES ('codex', 'Codex', 3, NULL, NULL, 1, 2, '💻')`)
	exec(`INSERT INTO agent_harness (agent_id, harness_id, harness_agent_id, enabled, model_primary, created_at, updated_at, shelved) VALUES (3, 'codex', 'claxon', 1, 'gpt', 1, 2, 0)`)
	exec(`INSERT INTO tracked_files VALUES (5, '/home/example/CLAUDE.md', 'global', NULL, 1, 'abc', 3, 4, 5, 'present', 6, 7, NULL)`)
	exec(`INSERT INTO tracked_file_versions VALUES (9, 5, 'abc', 4, ?, 'scan-import', NULL, 'first', 8)`, []byte("a\x00\xffb"))
	exec(`INSERT INTO prompt_collections VALUES (2, 'global', 'Host', 'global', '/home/example', NULL, 1, 2, 0)`)
	exec(`INSERT INTO prompt_sections_legacy_per_file VALUES (1, 2, 'old', NULL, 'old body', 'claude', 0, 1, NULL, 1, 1)`)
	exec(`INSERT INTO prompt_sections VALUES (4, 2, 'Rules', '# Rules', 'Write plainly.', '["style"]', 10, 1, 1, 2)`)
	exec(`INSERT INTO prompt_sections VALUES (6, 2, 'Tools', '## Tools', 'Use the stores.', '[]', 20, 1, 1, 2)`)
	exec(`INSERT INTO prompt_section_revisions VALUES (1, 4, 2, 'create', 'Rules', '# Rules', 'Write plainly.', '["style"]', 10, 1, 'import', NULL, NULL, 1)`)
	exec(`INSERT INTO prompt_collection_outputs VALUES (8, 2, 'CLAUDE.md', 1, 'abc', 3, 1, 2)`)
	exec(`INSERT INTO prompt_harness_deliveries VALUES ('claude_code', 'native_file', 'CLAUDE.md', NULL, 1)`)
	exec(`INSERT INTO prompt_harness_deliveries VALUES ('jig', 'inject', NULL, NULL, 1)`)
	exec(`INSERT INTO prompt_drifts VALUES (11, 2, 8, 'abc', 'def', ?, 'held', 'removes or renames sections', '[]', NULL, 5, NULL)`, []byte("# Rules\n"))
	// Deleted rows: AUTOINCREMENT had handed out higher numbers than the
	// rows left show.
	exec(`UPDATE sqlite_sequence SET seq = 40 WHERE name = 'agents'`)
	exec(`UPDATE sqlite_sequence SET seq = 900 WHERE name = 'prompt_sections'`)
	if adjust != nil {
		adjust(db)
	}
	return path
}

func TestImportCopiesAStandaloneFileWholeAndKeepsItsIds(t *testing.T) {
	path := standaloneFixture(t, nil)
	s := testStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), s.db, path)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"agents": 2, "harness": 1, "agent_harness": 1, "tracked_files": 1, "tracked_file_versions": 1,
		"prompt_collections": 1, "prompt_sections_legacy_per_file": 1, "prompt_sections": 2, "prompt_section_revisions": 1,
		"prompt_collection_outputs": 1, "prompt_harness_deliveries": 2, "prompt_drifts": 1, "machine_seed_state": 0} {
		if counts.Rows[table] != want {
			t.Errorf("rows copied from %s = %d, want %d", table, counts.Rows[table], want)
		}
	}
	if counts.NumberingContinuesAfter["agents"] != 40 || counts.NumberingContinuesAfter["prompt_sections"] != 900 || counts.NumberingContinuesAfter["prompt_drifts"] != 11 {
		t.Errorf("numbering continues after %+v", counts.NumberingContinuesAfter)
	}

	claxon, err := s.GetAgentBySlug("claxon")
	if err != nil || claxon.ID != 3 || claxon.Emoji != "🦀" || !claxon.Enabled {
		t.Fatalf("claxon = %+v, %v", claxon, err)
	}
	var parent sql.NullInt64
	if err := s.db.QueryRow(`SELECT parent_agent_id FROM agents WHERE id=3`).Scan(&parent); err != nil || parent.Int64 != 7 {
		t.Fatalf("claxon's parent = %v, %v; want 7, which the file holds after it", parent, err)
	}
	content, err := s.ReadVersionContent(9)
	if err != nil || !bytes.Equal(content, []byte("a\x00\xffb")) {
		t.Fatalf("version content = %q, %v", content, err)
	}
	drift, err := s.GetPromptDriftDiskContent(11)
	if err != nil || string(drift) != "# Rules\n" {
		t.Fatalf("drift content = %q, %v", drift, err)
	}

	// The next ids follow the standalone store's high-water marks, so a
	// deleted id is not handed out again.
	next := &Agent{Slug: "new", DisplayName: "New", Enabled: true}
	if err := s.UpsertAgent(next); err != nil {
		t.Fatal(err)
	}
	if next.ID != 41 {
		t.Fatalf("the next agent id = %d, want 41", next.ID)
	}

	resolved, err := s.ResolveContext("jig", "/home/example/repos/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Content != "# Rules\n\nWrite plainly.\n\n## Tools\n\nUse the stores." {
		t.Fatalf("resolved prompt = %q", resolved.Content)
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	path := standaloneFixture(t, nil)
	s := testStore(t)
	if _, err := ImportStandaloneDatabase(context.Background(), s.db, path); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(context.Background(), s.db, path)
	if err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("a second import = %v, want a refusal", err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	for name, adjust := range map[string]func(db *sql.DB){
		"a column it does not know": func(db *sql.DB) {
			db.Exec(`ALTER TABLE prompt_sections ADD COLUMN level INTEGER`)
		},
		"a missing table": func(db *sql.DB) {
			db.Exec(`DROP TABLE prompt_drifts`)
		},
		"a table it does not know": func(db *sql.DB) {
			db.Exec(`CREATE TABLE prompt_notes (id INTEGER)`)
		},
		"a left-behind table holding a row": func(db *sql.DB) {
			db.Exec(`INSERT INTO orchestrators VALUES ('inber', 'inber')`)
		},
		"a row the schema refuses": func(db *sql.DB) {
			db.Exec(`INSERT INTO prompt_sections VALUES (12, 99, 'Orphan', '# Orphan', 'x', '[]', 10, 1, 1, 2)`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := standaloneFixture(t, adjust)
			s := testStore(t)
			if _, err := ImportStandaloneDatabase(context.Background(), s.db, path); err == nil {
				t.Fatal("the import copied a file it should have refused")
			}
			var agents int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agents); err != nil || agents != 0 {
				t.Fatalf("agents after a refused import = %d, %v; want 0", agents, err)
			}
		})
	}
}
