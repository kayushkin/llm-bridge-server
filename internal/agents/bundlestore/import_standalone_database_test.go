package bundlestore

import (
	"database/sql"
	_ "embed"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

// standaloneLegacyMembersSchema is the table the standalone store moved
// name-keyed members into, as its start-up migration made it.
const standaloneLegacyMembersSchema = `
	CREATE TABLE bundle_members_legacy_name_join (
		bundle_id INTEGER NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
		kind TEXT NOT NULL, name TEXT NOT NULL, condition TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (bundle_id, kind, name), CHECK (kind IN ('skill', 'tool')));`

// writeStandaloneDatabase makes a standalone store's SQLite file from schema
// and then rows, and returns its path.
func writeStandaloneDatabase(t *testing.T, schema, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle-store.db")
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

// standaloneRows is a small made-up registry. The child "react" (id 2)
// extends "web" (id 5), a parent with the higher id, which a copy in id order
// would trip over. Bundle 3 is disabled, bundle 4 was numbered and deleted.
const standaloneRows = `
	INSERT INTO bundles (id, name, display_name, description, match_tags, model, effort, enabled, created_at, updated_at, denied_read_paths)
		VALUES (1, 'base', 'Base', 'every session', '', 'sonnet', 'high', 1, 100, 101, '["~/.secrets/**"]');
	INSERT INTO bundles (id, name, extends_id, match_tags, enabled, created_at, updated_at)
		VALUES (2, 'react', NULL, '["react"]', 1, 110, 111);
	INSERT INTO bundles (id, name, match_tags, enabled, created_at, updated_at)
		VALUES (3, 'retired', '["old"]', 0, 120, 121);
	INSERT INTO bundles (id, name, match_tags, enabled, created_at, updated_at)
		VALUES (5, 'web', '["web"]', 1, 130, 131);
	UPDATE bundles SET extends_id = 5 WHERE id = 2;
	INSERT INTO bundle_members (bundle_id, kind, member_id, name, condition, effect) VALUES
		(1, 'tool', 902, 'claude_code.Edit', '', 'deny'),
		(2, 'skill', 9001, 'browser-automation', '', 'include'),
		(2, 'tool', 13, 'playwright', '', 'include'),
		(2, 'tool', 14, 'chrome-devtools', 'perf', 'include'),
		(5, 'tool', 901, 'claude_code.Write', '', 'deny');
`

func TestImportCopiesEveryBundleAndMemberKeepingIDs(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema+standaloneLegacyMembersSchema, standaloneRows+`
		INSERT INTO bundle_members_legacy_name_join (bundle_id, kind, name, condition) VALUES (3, 'skill', 'claude-api', '');`)
	s := openTest(t)
	counts, err := ImportStandaloneDatabase(testContext, s.db, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Bundles: 4, Members: 5, LegacyMembers: 1, LastBundleID: 5, DisabledBundles: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}

	react, err := s.GetBundle(testContext, 2)
	if err != nil {
		t.Fatal(err)
	}
	if react.Name != "react" || react.ExtendsID != 5 || react.Extends != "web" || react.CreatedAt != 110 || react.UpdatedAt != 111 {
		t.Fatalf("react = %+v", react)
	}
	if !reflect.DeepEqual(react.MatchTags, []string{"react"}) || len(react.Members) != 3 {
		t.Fatalf("react's tags or members were not copied: %+v", react)
	}
	base, err := s.GetBundle(testContext, 1)
	if err != nil {
		t.Fatal(err)
	}
	if base.Model != "sonnet" || base.Effort != "high" || !reflect.DeepEqual(base.DeniedReadPaths, []string{"~/.secrets/**"}) ||
		len(base.Members) != 1 || base.Members[0].Effect != MemberDeny {
		t.Fatalf("base = %+v", base)
	}
	retired, err := s.GetBundle(testContext, 3)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Enabled {
		t.Fatal("a disabled bundle came back enabled")
	}

	// The copy resolves as the standalone store did: base, web and react,
	// with web's deny and base's deny both taken away.
	resolution, err := Resolve(testContext, s, ResolveRequest{BundleIDs: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasBundle(resolution.Bundles, "web") || !hasID(resolution.DeniedTools, 901) || !hasID(resolution.DeniedTools, 902) {
		t.Fatalf("resolution = %+v", resolution)
	}

	var legacy int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + LegacyMembersTable).Scan(&legacy); err != nil || legacy != 1 {
		t.Fatalf("legacy members kept = %d, %v; want 1", legacy, err)
	}

	// Numbering goes on after the highest id copied, never reusing 4 or 5.
	next := &Bundle{Name: "new", Enabled: true}
	mustUpsert(t, s, next)
	if next.ID != 6 {
		t.Fatalf("the next bundle got id %d, want 6", next.ID)
	}
}

func TestImportRefusesAModuleThatHoldsRows(t *testing.T) {
	path := writeStandaloneDatabase(t, standaloneSchema, standaloneRows)
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "already-here", Enabled: true})
	_, err := ImportStandaloneDatabase(testContext, s.db, path)
	if err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("want a refusal of a module that holds rows, got %v", err)
	}
}

// A standalone file its store never finished migrating is refused, not
// guessed at: name-keyed members carry no id, a name-linked parent has no id
// yet, and a pre-deny table has no effect column. The standalone store
// migrates each of these on its own start, so the way out is to start it once.
func TestImportRefusesAFileTheStandaloneStoreHasNotMigrated(t *testing.T) {
	for name, schema := range map[string]string{
		"members keyed by name": `
			CREATE TABLE bundles (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
				display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
				extends_id INTEGER, match_tags TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
				effort TEXT NOT NULL DEFAULT '', denied_read_paths TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
			CREATE TABLE bundle_members (bundle_id INTEGER NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL,
				condition TEXT NOT NULL DEFAULT '', PRIMARY KEY (bundle_id, kind, name));`,
		"parent linked by name": `
			CREATE TABLE bundles (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
				display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
				extends TEXT NOT NULL DEFAULT '', match_tags TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
				effort TEXT NOT NULL DEFAULT '', denied_read_paths TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
			CREATE TABLE bundle_members (bundle_id INTEGER NOT NULL, kind TEXT NOT NULL, member_id INTEGER NOT NULL,
				name TEXT NOT NULL DEFAULT '', condition TEXT NOT NULL DEFAULT '', effect TEXT NOT NULL DEFAULT 'include',
				PRIMARY KEY (bundle_id, kind, member_id));`,
		"no effect column": `
			CREATE TABLE bundles (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
				display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
				extends_id INTEGER, match_tags TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
				effort TEXT NOT NULL DEFAULT '', denied_read_paths TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
			CREATE TABLE bundle_members (bundle_id INTEGER NOT NULL, kind TEXT NOT NULL, member_id INTEGER NOT NULL,
				name TEXT NOT NULL DEFAULT '', condition TEXT NOT NULL DEFAULT '', PRIMARY KEY (bundle_id, kind, member_id));`,
		"a column the import does not know": standaloneSchema + `ALTER TABLE bundles ADD COLUMN owner TEXT NOT NULL DEFAULT '';`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeStandaloneDatabase(t, schema, "")
			s := openTest(t)
			_, err := ImportStandaloneDatabase(testContext, s.db, path)
			if err == nil || !strings.Contains(err.Error(), "copies only a table it can copy whole") {
				t.Fatalf("want a refusal naming the columns, got %v", err)
			}
		})
	}
}

// A row the module's schema refuses stops the whole import: nothing of the
// file is left behind.
func TestImportCopiesEverythingOrNothing(t *testing.T) {
	for name, rows := range map[string]string{
		"a member of a bundle the file does not hold": standaloneRows + `
			INSERT INTO bundle_members (bundle_id, kind, member_id, name) VALUES (77, 'tool', 1, 'stray');`,
		"an enabled flag that is neither 0 nor 1": standaloneRows + `UPDATE bundles SET enabled = 2 WHERE id = 3;`,
		"denied read paths that are not a list":   standaloneRows + `UPDATE bundles SET denied_read_paths = 'not json' WHERE id = 3;`,
	} {
		t.Run(name, func(t *testing.T) {
			// SQLite enforces foreign keys only when asked, so the stray row is
			// written as a file that never asked would hold it.
			path := writeStandaloneDatabase(t, strings.ReplaceAll(standaloneSchema, "PRAGMA foreign_keys = ON;", ""), rows)
			s := openTest(t)
			if _, err := ImportStandaloneDatabase(testContext, s.db, path); err == nil {
				t.Fatal("the import accepted a file it cannot copy whole")
			}
			bundles, err := s.ListBundles(testContext, ListFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(bundles) != 0 {
				t.Fatalf("a refused import left %d bundles behind", len(bundles))
			}
		})
	}
}

func TestImportRefusesAFileThatIsNotThere(t *testing.T) {
	s := openTest(t)
	_, err := ImportStandaloneDatabase(testContext, s.db, filepath.Join(t.TempDir(), "missing.db"))
	if err == nil || !strings.Contains(err.Error(), "the standalone database") {
		t.Fatalf("a missing file must be an error naming it, got %v", err)
	}
}
