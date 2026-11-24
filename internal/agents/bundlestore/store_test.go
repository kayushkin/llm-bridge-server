package bundlestore

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bs"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// writeBehindTheAPI runs a statement against the store's database file on a
// separate connection with foreign keys off, so a test can create a row the API
// refuses to create. Everything reachable through UpsertBundle is written
// through UpsertBundle; this is only for proving what the read-side guards do
// when they meet a row that did not come that way.
func writeBehindTheAPI(t *testing.T, s *Store, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(s.DataDir(), "bundle-store.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// expectTheDatabaseToRefuse runs a statement on its own connection with foreign
// keys ON and asserts SQLite itself rejects it. DeleteBundle looks for children
// in Go before it issues the DELETE, and that check would pass just as happily
// against ON DELETE SET NULL — so a test that only calls DeleteBundle proves the
// Go guard and says nothing about the schema clause underneath it. This is how
// the clause gets tested.
func expectTheDatabaseToRefuse(t *testing.T, s *Store, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(s.DataDir(), "bundle-store.db")+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err == nil {
		t.Fatalf("the database accepted %q — the foreign key is missing or is not RESTRICT", stmt)
	} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("%s: want a foreign key refusal, got: %v", stmt, err)
	}
}

func TestUpsertBundleRoundtrip(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	b := &Bundle{
		Name:      "react",
		Extends:   "base",
		MatchTags: []string{"react"},
		Members: []Member{
			{Kind: MemberSkill, ID: 9001, Name: "browser-automation"},
			{Kind: MemberTool, ID: 13, Name: "playwright"},
			{Kind: MemberTool, ID: 14, Name: "chrome-devtools", Condition: "perf"},
		},
		Enabled: true,
	}
	inserted, err := s.UpsertBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("expected insert")
	}
	got, err := s.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if got.Extends != "base" {
		t.Fatalf("extends not roundtripped: %q", got.Extends)
	}
	base, err := s.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtendsID != base.ID {
		t.Fatalf("the stored link must be the parent's id (%d), got %d — a name was written back instead", base.ID, got.ExtendsID)
	}
	if len(got.Members) != 3 {
		t.Fatalf("members not roundtripped: %+v", got.Members)
	}
	for _, m := range got.Members {
		if m.ID == 0 {
			t.Fatalf("member id not roundtripped, so the name is doing the identifying again: %+v", m)
		}
	}
}

func TestMemberReplacementOnUpdate(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "x", Members: []Member{
		{Kind: MemberSkill, ID: 1, Name: "a"},
		{Kind: MemberSkill, ID: 2, Name: "b"},
	}, Enabled: true}
	mustUpsert(t, s, b)
	b.Members = []Member{{Kind: MemberSkill, ID: 3, Name: "c"}}
	mustUpsert(t, s, b)
	got, err := s.GetBundleByName("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].ID != 3 {
		t.Fatalf("members not replaced on update: %+v", got.Members)
	}
}

// The kind column is guarded twice: by validate here and by
// CHECK (kind IN ('skill', 'tool')) in the schema. Asserting only that the
// upsert failed cannot tell those two apart, and the schema wins by answering
// last — deleting validate's kind check left this test green while the caller
// started receiving "CHECK constraint failed: kind IN ('skill', 'tool')", which
// names neither the bundle nor the offending value. So assert the wording only
// validate produces.
func TestInvalidMemberKindRejected(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "bad", Members: []Member{{Kind: "frobnicate", ID: 1, Name: "x"}}}
	_, err := s.UpsertBundle(b)
	if err == nil {
		t.Fatal("expected validation error for invalid member kind")
	}
	if !strings.Contains(err.Error(), `must be skill or tool`) {
		t.Fatalf("the refusal should be validate's, which says what a kind may be; got: %v", err)
	}
	if !strings.Contains(err.Error(), `"frobnicate"`) {
		t.Fatalf("the refusal should name the offending kind so the author can act on it; got: %v", err)
	}
	if !strings.Contains(err.Error(), "bundle bad") {
		t.Fatalf("the refusal should name the bundle it came from; got: %v", err)
	}
}

// A member with a name and no id is the defect this schema exists to stop: it
// cannot be installed (skill-store's only install route is by id) and it cannot
// be identified (13 skill names are carried by two sources each).
func TestMemberWithoutIDRejected(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "nameonly", Members: []Member{{Kind: MemberSkill, Name: "claude-api"}}}
	_, err := s.UpsertBundle(b)
	if err == nil {
		t.Fatal("expected a member carrying only a name to be rejected")
	}
	if !strings.Contains(err.Error(), "skill-store") {
		t.Fatalf("error should send the author to the owning store, got: %v", err)
	}
	if _, err := s.GetBundleByName("nameonly"); err != ErrNotFound {
		t.Fatalf("a rejected bundle must not be half-written: %v", err)
	}
}

func TestDuplicateMemberIDRejected(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "dup", Members: []Member{
		{Kind: MemberSkill, ID: 7, Name: "seven"},
		{Kind: MemberSkill, ID: 7, Name: "seven again"},
	}}
	_, err := s.UpsertBundle(b)
	if err == nil {
		t.Fatal("expected the same member id listed twice to be rejected")
	}
	// bundle_members carries PRIMARY KEY (bundle_id, kind, member_id), and the
	// index answers LAST — after validate has already passed the member set
	// through. So "an error came back" cannot tell validate's guard from the
	// database's, and this test used to assert only that. Measured: deleting
	// validate's duplicate check left this test, and the whole suite, green.
	// Name validate's own wording instead.
	if !strings.Contains(err.Error(), "member id 7 listed twice") {
		t.Fatalf("want validate's own refusal naming the repeated id, got %v — a "+
			"PRIMARY KEY constraint error here means the schema refused the row that "+
			"validate was supposed to catch first, several layers later and without "+
			"naming the bundle or the member", err)
	}
}

// Two skills that share a name are two members. Under the old name-keyed
// primary key the second insert replaced the first and the bundle silently lost
// a skill.
func TestTwoMembersSharingOneNameBothSurvive(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "collide", Enabled: true, Members: []Member{
		{Kind: MemberSkill, ID: 4, Name: "claude-api"},   // anthropics
		{Kind: MemberSkill, ID: 341, Name: "claude-api"}, // antigravity
	}}
	mustUpsert(t, s, b)
	got, err := s.GetBundleByName("collide")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("two skills sharing a name must stay two members, got %+v", got.Members)
	}
}

func TestListByMatchTag(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "react", MatchTags: []string{"react"}, Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "go-service", MatchTags: []string{"go"}, Enabled: true})
	got, err := s.ListBundles(ListFilter{Tag: "react"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "react" {
		t.Fatalf("tag filter wrong: %+v", got)
	}
}

// A database written before member_id existed must open, must not resolve its
// name-keyed rows, and must not lose them either — an id cannot be invented
// from a name, so the rows are kept where an operator can read them back.
func TestLegacyNameKeyedMembersAreQuarantinedNotDropped(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bundle-store.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `
		CREATE TABLE bundles (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			extends TEXT NOT NULL DEFAULT '', match_tags TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE bundle_members (
			bundle_id INTEGER NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, name TEXT NOT NULL, condition TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (bundle_id, kind, name), CHECK (kind IN ('skill','tool')));
		CREATE INDEX idx_bundle_members_bundle ON bundle_members(bundle_id);
		INSERT INTO bundles (id, name, created_at, updated_at) VALUES (1, 'react', 1, 1);
		INSERT INTO bundle_members (bundle_id, kind, name) VALUES (1, 'skill', 'browser-automation');
		INSERT INTO bundle_members (bundle_id, kind, name) VALUES (1, 'tool', 'playwright');
	`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open a pre-member_id database: %v", err)
	}
	defer s.Close()

	if s.QuarantinedMembers != 2 {
		t.Fatalf("QuarantinedMembers = %d, want 2 — the count is what tells the operator anything moved", s.QuarantinedMembers)
	}
	got, err := s.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 0 {
		t.Fatalf("name-keyed members must not be resolved, got %+v", got.Members)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + LegacyMembersTable).Scan(&kept); err != nil {
		t.Fatalf("legacy rows must still be readable: %v", err)
	}
	if kept != 2 {
		t.Fatalf("legacy rows kept = %d, want 2 — the migration must not destroy data", kept)
	}

	// The migrated table takes id-carrying members, and keeps its index.
	mustUpsert(t, s, &Bundle{Name: "react", Enabled: true, Members: []Member{
		{Kind: MemberSkill, ID: 9001, Name: "browser-automation"},
	}})
	var indexed int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_bundle_members_bundle' AND tbl_name='bundle_members'`,
	).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Fatal("the member index must follow the real table, not stay on the quarantined one")
	}
}

func TestOpenIsIdempotentAfterMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, s, &Bundle{Name: "x", Enabled: true, Members: []Member{{Kind: MemberTool, ID: 13, Name: "playwright"}}})
	s.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.QuarantinedMembers != 0 {
		t.Fatalf("a second Open must not re-quarantine: %d", again.QuarantinedMembers)
	}
	if again.BackfilledExtendsLinks != 0 {
		t.Fatalf("a second Open must not re-backfill inheritance links: %d", again.BackfilledExtendsLinks)
	}
	got, err := again.GetBundleByName("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].ID != 13 {
		t.Fatalf("members lost across reopen: %+v", got.Members)
	}
}

// ---------------------------------------------------------------------------
// The inheritance link is an id, and every way of writing one that cannot be
// pinned to a real parent row is refused at the write.
// ---------------------------------------------------------------------------

// Writing a child before its parent used to succeed and produce a bundle that
// blew up two layers away, on somebody else's /resolve. It is refused here, and
// the message says what to do about it.
func TestExtendingAMissingParentRejected(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(&Bundle{Name: "orphan", Extends: "no-such-parent", Enabled: true})
	if err == nil {
		t.Fatal("expected a bundle extending a parent that does not exist to be rejected")
	}
	if !strings.Contains(err.Error(), "no-such-parent") {
		t.Fatalf("the rejection must name the parent it could not find, got: %v", err)
	}
	if _, err := s.GetBundleByName("orphan"); err != ErrNotFound {
		t.Fatalf("a rejected bundle must not be half-written: %v", err)
	}
}

func TestExtendsIDPointingAtNothingRejected(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(&Bundle{Name: "orphan", ExtendsID: 999999, Enabled: true})
	if err == nil {
		t.Fatal("expected an extends_id that names no bundle to be rejected")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Fatalf("the rejection must name the id it could not find, got: %v", err)
	}
}

// An id and a name are one fact arriving by two routes. When they disagree one
// of them is wrong, and picking a winner would silently discard the other.
func TestExtendsIDAndNameDisagreeingRejected(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "other", Enabled: true})
	base, err := s.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpsertBundle(&Bundle{Name: "child", ExtendsID: base.ID, Extends: "other", Enabled: true})
	if err == nil {
		t.Fatal("expected extends_id and extends naming different bundles to be rejected")
	}
	if !strings.Contains(err.Error(), "base") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("the rejection must name both sides of the disagreement, got: %v", err)
	}
}

// A caller that already holds the parent's id does not have to know its name;
// the name comes back from the parent row on the read.
func TestExtendsIDAloneIsEnoughAndTheNameComesBack(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	base, err := s.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, s, &Bundle{Name: "child", ExtendsID: base.ID, Enabled: true})
	got, err := s.GetBundleByName("child")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtendsID != base.ID || got.Extends != "base" {
		t.Fatalf("want extends_id=%d extends=%q, got %d / %q", base.ID, "base", got.ExtendsID, got.Extends)
	}
}

// The cycle is refused at the write that closes it, not at some later resolve
// of an unrelated repository. expandChain keeps its own guard for rows written
// outside this path; see TestResolveCycleDetected.
func TestCycleRefusedAtTheWrite(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "b", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "a", Extends: "b", Enabled: true})

	_, err := s.UpsertBundle(&Bundle{Name: "b", Extends: "a", Enabled: true})
	if err == nil {
		t.Fatal("expected re-parenting b under its own descendant to be rejected")
	}
	if !strings.Contains(err.Error(), "own inheritance chain") {
		t.Fatalf("the rejection must say what is wrong, got: %v", err)
	}
	// And it must not have half-applied: b still has no parent.
	b, err := s.GetBundleByName("b")
	if err != nil {
		t.Fatal(err)
	}
	if b.ExtendsID != 0 {
		t.Fatalf("a rejected re-parent must roll back, got extends_id=%d", b.ExtendsID)
	}
}

func TestSelfExtendRefused(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "loop", Enabled: true})
	if _, err := s.UpsertBundle(&Bundle{Name: "loop", Extends: "loop", Enabled: true}); err == nil {
		t.Fatal("expected a bundle extending itself to be rejected")
	}
}

// ON DELETE RESTRICT, driven for real. SET NULL would have made the child a
// root, inheriting nothing, with no error anywhere.
func TestDeletingAnExtendedBundleRefused(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "react", Extends: "base", Enabled: true})
	base, err := s.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}

	err = s.DeleteBundle(base.ID)
	if !errors.Is(err, ErrParentHasChildren) {
		t.Fatalf("deleting an extended bundle must be refused, got: %v", err)
	}
	if !strings.Contains(err.Error(), "react") {
		t.Fatalf("the refusal must name the children, got: %v", err)
	}
	if _, err := s.GetBundleByName("base"); err != nil {
		t.Fatalf("the refused delete must not have removed anything: %v", err)
	}

	// And the schema clause holds on its own, without the Go check in front of
	// it — which is what makes this RESTRICT rather than SET NULL.
	expectTheDatabaseToRefuse(t, s, `DELETE FROM bundles WHERE id=?`, base.ID)

	// Cut the link and it deletes.
	mustUpsert(t, s, &Bundle{Name: "react", Enabled: true})
	if err := s.DeleteBundle(base.ID); err != nil {
		t.Fatalf("deleting a bundle nothing extends: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migrating a database whose inheritance link was the parent's name.
// ---------------------------------------------------------------------------

// legacyBundlesSchema is the pre-extends_id shape, written out in full so this
// test keeps meaning something after schema.sql moves on.
const legacyBundlesSchema = `
	CREATE TABLE bundles (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
		display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
		extends TEXT NOT NULL DEFAULT '', match_tags TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
	CREATE TABLE bundle_members (
		bundle_id INTEGER NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
		kind TEXT NOT NULL, member_id INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '',
		condition TEXT NOT NULL DEFAULT '', PRIMARY KEY (bundle_id, kind, member_id),
		CHECK (kind IN ('skill','tool')), CHECK (member_id > 0));
`

func writeLegacyDatabase(t *testing.T, dir, extra string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "bundle-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacyBundlesSchema + extra); err != nil {
		t.Fatal(err)
	}
}

// This mirrors the live database exactly: three bundles, two of them extending
// "base" by name.
func TestLegacyNameJoinedExtendsIsBackfilledToIDs(t *testing.T) {
	dir := t.TempDir()
	writeLegacyDatabase(t, dir, `
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (1, 'base', '', 1, 1);
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (2, 'react', 'base', 1, 1);
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (3, 'go-service', 'base', 1, 1);
	`)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open a pre-extends_id database: %v", err)
	}
	defer s.Close()

	if s.BackfilledExtendsLinks != 2 {
		t.Fatalf("BackfilledExtendsLinks = %d, want 2 — the count is what tells the operator anything moved", s.BackfilledExtendsLinks)
	}
	for _, name := range []string{"react", "go-service"} {
		got, err := s.GetBundleByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExtendsID != 1 {
			t.Fatalf("%s should now extend bundle id 1, got %d", name, got.ExtendsID)
		}
		if got.Extends != "base" {
			t.Fatalf("%s should still display its parent's name, got %q", name, got.Extends)
		}
	}

	// The old column is gone, so nothing can quietly start reading it again.
	columns, err := tableColumns(s.db, "bundles")
	if err != nil {
		t.Fatal(err)
	}
	if columns["extends"] {
		t.Fatal("the extends name column must be dropped, not left behind to drift out of step with extends_id")
	}

	// And the column the migration added really carries the foreign key, not
	// just an integer that happens to hold the right number.
	expectTheDatabaseToRefuse(t, s, `DELETE FROM bundles WHERE id=1`)
}

// A name that resolves to no row cannot become an id, and must not become NULL
// either: NULL is how a root bundle is spelled, so a stranded child would come
// back looking healthy while inheriting nothing.
func TestLegacyDanglingExtendsFailsTheOpenLoudly(t *testing.T) {
	dir := t.TempDir()
	writeLegacyDatabase(t, dir, `
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (1, 'base', '', 1, 1);
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (2, 'react', 'base', 1, 1);
		INSERT INTO bundles (id, name, extends, created_at, updated_at) VALUES (3, 'stray', 'deleted-parent', 1, 1);
	`)

	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatal("a dangling inheritance link must fail the open, not be silently cleared")
	}
	if !strings.Contains(err.Error(), "stray") || !strings.Contains(err.Error(), "deleted-parent") {
		t.Fatalf("the error must name the child and the parent it wanted, got: %v", err)
	}
}
