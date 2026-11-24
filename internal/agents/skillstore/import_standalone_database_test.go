package skillstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store kept
// one, with a few made-up rows: two sources, three skills with NULLs where the
// old schema allowed them, an installation, seed versions holding bytes, two
// machine profiles and seed state pointing at a version, gaps in every
// numbering, and a sqlite_sequence above the last skill (the newest was
// deleted).
func newStandaloneDatabase(t *testing.T, extraStatements ...string) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "skill-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO sources (id, name, kind, local_path, repo_url, branch, subpath, trust_tier, license, last_synced_at, last_commit, last_skill_count, enabled, created_at, updated_at)
			VALUES (1, 'upstream', 'git', '', 'https://example.invalid/skills.git', 'main', 'skills', 'official', NULL, 1800000100, 'abc123', 2, 1, 1800000000, 1800000100)`,
		`INSERT INTO sources (id, name, kind, local_path, repo_url, branch, subpath, trust_tier, license, last_synced_at, last_commit, last_skill_count, enabled, created_at, updated_at)
			VALUES (3, 'authored', 'local', '/srv/authored', '', 'main', '', 'personal', 'MIT', NULL, NULL, 1, 0, 1800000200, 1800000200)`,
		`INSERT INTO skills VALUES (2, 1, 'alpha', 'Alpha', 'the first skill', '---\nname: alpha\n---\nbody', 'skills/alpha', 'abc123', 'hash-a', 1, '["go","test"]', 1, 1800000000, 1800000100)`,
		`INSERT INTO skills VALUES (5, 1, 'beta', NULL, NULL, '---\nname: beta\n---\n', 'skills/beta', NULL, 'hash-b', 0, NULL, 0, 1800000000, 1800000000)`,
		`INSERT INTO skills VALUES (6, 3, 'gamma', '', '', 'gamma body', 'gamma', '', 'hash-c', 0, '', 1, 1800000200, 1800000200)`,
		`UPDATE sqlite_sequence SET seq = 9 WHERE name = 'skills'`,
		`INSERT INTO installations VALUES (2, '/home/someone/.claude/skills/alpha', 1800000300)`,
		`INSERT INTO seed_file_versions VALUES (4, 2, 'SKILL.md', 'sha-one', 5, X'68656C6C6F', 'ingest', NULL, NULL, 1800000300)`,
		`INSERT INTO seed_file_versions VALUES (7, 2, 'SKILL.md', 'sha-two', 3, X'00FF01', 'runner-drift', 'machine-a', 'edited there', 1800000400)`,
		`INSERT INTO seed_file_versions VALUES (8, 2, 'empty.txt', 'sha-empty', 0, X'', 'ingest', NULL, NULL, 1800000400)`,
		`INSERT INTO machine_seed_profiles VALUES ('machine-a', 1, 1800000300, 1800000300)`,
		`INSERT INTO machine_seed_profiles VALUES ('machine-b', 0, 1800000300, 1800000500)`,
		`INSERT INTO machine_seed_state VALUES ('machine-a', 2, 'SKILL.md', 'sha-two', 1800000400, 7, 1800000350)`,
		`INSERT INTO machine_seed_state VALUES ('machine-b', 2, 'SKILL.md', NULL, NULL, NULL, NULL)`,
	}
	for _, statement := range append(statements, extraStatements...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	s := newTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Sources: 2, Skills: 3, Installations: 1, SeedFileVersions: 3, SeedFileVersionBytes: 8,
		MachineSeedProfiles: 2, MachineSeedStates: 2, LastSourceID: 3, LastSkillID: 9, LastSeedFileVersionID: 8}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	sources, err := s.ListSources(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Name != "authored" || sources[0].ID != 3 || sources[0].Enabled ||
		sources[0].License != "MIT" || sources[0].LastSyncedAt != 0 || sources[0].Kind != SourceKindLocal ||
		sources[1].Name != "upstream" || !sources[1].Enabled || sources[1].LastCommit != "abc123" || sources[1].LastSkillCount != 2 {
		t.Fatalf("sources = %+v", sources)
	}

	alpha, err := s.GetSkill(testContext, 2)
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Name != "alpha" || alpha.SourceName != "upstream" || alpha.DisplayName != "Alpha" || alpha.SkillMD != "---\\nname: alpha\\n---\\nbody" ||
		strings.Join(alpha.Tags, ",") != "go,test" || !alpha.Installed || alpha.InstallPath != "/home/someone/.claude/skills/alpha" ||
		alpha.InstalledAt != 1800000300 || alpha.ContentHash != "hash-a" || alpha.FileCount != 1 || !alpha.Enabled {
		t.Fatalf("alpha = %+v", alpha)
	}
	beta, err := s.GetSkill(testContext, 5)
	if err != nil {
		t.Fatal(err)
	}
	if beta.DisplayName != "" || beta.Description != "" || beta.Tags != nil || beta.Enabled || beta.Installed {
		t.Fatalf("beta = %+v", beta)
	}

	drift, err := s.GetSeedVersion(testContext, 7)
	if err != nil {
		t.Fatal(err)
	}
	if drift.MachineID != "machine-a" || drift.Note != "edited there" || drift.Source != SeedSourceRunnerDrift || drift.Size != 3 {
		t.Fatalf("version 7 = %+v", drift)
	}
	content, err := s.ReadSeedVersionContent(testContext, 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "\x00\xff\x01" {
		t.Fatalf("version 7 bytes = %q, want 00 ff 01", content)
	}
	empty, err := s.ReadSeedVersionContent(testContext, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("version 8 bytes = %q, want none", empty)
	}

	profile, err := s.GetSeedProfile(testContext, "machine-b")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Enabled || profile.UpdatedAt != 1800000500 {
		t.Fatalf("machine-b profile = %+v", profile)
	}
	var pushed sql.NullInt64
	if err := s.db.QueryRow(`SELECT last_pushed_version_id FROM machine_seed_state WHERE machine_id='machine-a'`).Scan(&pushed); err != nil {
		t.Fatal(err)
	}
	if pushed.Int64 != 7 {
		t.Fatalf("machine-a last pushed version = %v, want 7", pushed)
	}

	// Numbering goes on past every number the standalone store handed out,
	// including the deleted skill 9.
	next := &Skill{SourceID: 1, Name: "delta", SkillMD: "delta"}
	if _, err := s.UpsertSkill(testContext, next); err != nil {
		t.Fatal(err)
	}
	if next.ID != 10 {
		t.Fatalf("the next skill got id %d, want 10", next.ID)
	}
	source := &Source{Name: "another", RepoURL: "https://example.invalid/more.git"}
	if err := s.UpsertSource(testContext, source); err != nil {
		t.Fatal(err)
	}
	if source.ID != 4 {
		t.Fatalf("the next source got id %d, want 4", source.ID)
	}
	version, err := s.AppendSeedVersion(testContext, 2, "new.txt", []byte("new"), SeedSourceIngest, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if version.ID != 9 {
		t.Fatalf("the next version got id %d, want 9", version.ID)
	}
}

func TestImportRefusesAModuleThatAlreadyHoldsRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertSource(testContext, &Source{Name: "already-here", RepoURL: "https://example.invalid/x.git"}); err != nil {
		t.Fatal(err)
	}
	_, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t))
	if err == nil || !strings.Contains(err.Error(), "already holds 1 rows in sources") {
		t.Fatalf("import into a module holding a source: %v, want a refusal", err)
	}
}

func TestImportRefusesAColumnItDoesNotKnow(t *testing.T) {
	s := newTestStore(t)
	_, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t, `ALTER TABLE skills ADD COLUMN rating INTEGER`))
	if err == nil || !strings.Contains(err.Error(), "table skills has columns") {
		t.Fatalf("import of a skills table with an unknown column: %v, want a refusal", err)
	}
	var held int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("a refused import left %d sources", held)
	}
}

func TestImportRefusesAFlagThatIsNeitherZeroNorOne(t *testing.T) {
	s := newTestStore(t)
	_, err := ImportStandaloneDatabase(testContext, s.db, newStandaloneDatabase(t, `UPDATE skills SET enabled = 2 WHERE id = 6`))
	if err == nil || !strings.Contains(err.Error(), "skill 6 (gamma) has enabled = 2") {
		t.Fatalf("import of enabled = 2: %v, want a refusal", err)
	}
	var held int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM skills`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("a refused import left %d skills; it must copy everything or nothing", held)
	}
}

func TestImportRefusesAFileThatIsNotASkillStore(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, s.db, path); err == nil || !strings.Contains(err.Error(), "not a skill-store database") {
		t.Fatalf("import of another database: %v, want a refusal", err)
	}
}
