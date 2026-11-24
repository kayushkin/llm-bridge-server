package bundlestore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testContext is the context every store call in these tests runs under.
var testContext = context.Background()

// openTest gives the test a store on a Postgres database of its own, made from
// a template with the module migrated (postgresstorage/postgrestest).
func openTest(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module))
}

// writeBehindTheAPI runs a statement straight against the module's tables, so
// a test can make a row the API refuses to make. Everything reachable through
// UpsertBundle is written through UpsertBundle; this is only for proving what
// the read-side guards do when they meet a row that did not come that way.
func writeBehindTheAPI(t *testing.T, s *Store, stmt string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(testContext, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// dropTheParentForeignKey takes the inheritance foreign key off the test's own
// database, for a test that needs a link to a parent that does not exist: a
// row the database would otherwise refuse, as it would come from a file
// written by hand.
func dropTheParentForeignKey(t *testing.T, s *Store) {
	t.Helper()
	writeBehindTheAPI(t, s, `ALTER TABLE bundles DROP CONSTRAINT bundles_extends_id_fkey`)
}

// expectTheDatabaseToRefuse runs a statement and asserts Postgres itself
// rejects it with a RESTRICT refusal (23001). DeleteBundle looks for children in
// Go before it issues the DELETE, and that check would pass just as happily
// against ON DELETE SET NULL — so a test that only calls DeleteBundle proves the
// Go guard and says nothing about the schema clause underneath it. This is how
// the clause gets tested.
func expectTheDatabaseToRefuse(t *testing.T, s *Store, stmt string, args ...any) {
	t.Helper()
	_, err := s.db.ExecContext(testContext, stmt, args...)
	if err == nil {
		t.Fatalf("the database accepted %q — the foreign key is missing or is not RESTRICT", stmt)
	}
	if !hasPostgresCode(err, restrictViolation) {
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
	inserted, err := s.UpsertBundle(testContext, b)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("expected insert")
	}
	got, err := s.GetBundleByName(testContext, "react")
	if err != nil {
		t.Fatal(err)
	}
	if got.Extends != "base" {
		t.Fatalf("extends not roundtripped: %q", got.Extends)
	}
	base, err := s.GetBundleByName(testContext, "base")
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
	got, err := s.GetBundleByName(testContext, "x")
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
	_, err := s.UpsertBundle(testContext, b)
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
	_, err := s.UpsertBundle(testContext, b)
	if err == nil {
		t.Fatal("expected a member carrying only a name to be rejected")
	}
	if !strings.Contains(err.Error(), "skill-store") {
		t.Fatalf("error should send the author to the owning store, got: %v", err)
	}
	if _, err := s.GetBundleByName(testContext, "nameonly"); err != ErrNotFound {
		t.Fatalf("a rejected bundle must not be half-written: %v", err)
	}
}

func TestDuplicateMemberIDRejected(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "dup", Members: []Member{
		{Kind: MemberSkill, ID: 7, Name: "seven"},
		{Kind: MemberSkill, ID: 7, Name: "seven again"},
	}}
	_, err := s.UpsertBundle(testContext, b)
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
	got, err := s.GetBundleByName(testContext, "collide")
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
	got, err := s.ListBundles(testContext, ListFilter{Tag: "react"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "react" {
		t.Fatalf("tag filter wrong: %+v", got)
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
	_, err := s.UpsertBundle(testContext, &Bundle{Name: "orphan", Extends: "no-such-parent", Enabled: true})
	if err == nil {
		t.Fatal("expected a bundle extending a parent that does not exist to be rejected")
	}
	if !strings.Contains(err.Error(), "no-such-parent") {
		t.Fatalf("the rejection must name the parent it could not find, got: %v", err)
	}
	if _, err := s.GetBundleByName(testContext, "orphan"); err != ErrNotFound {
		t.Fatalf("a rejected bundle must not be half-written: %v", err)
	}
}

func TestExtendsIDPointingAtNothingRejected(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(testContext, &Bundle{Name: "orphan", ExtendsID: 999999, Enabled: true})
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
	base, err := s.GetBundleByName(testContext, "base")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpsertBundle(testContext, &Bundle{Name: "child", ExtendsID: base.ID, Extends: "other", Enabled: true})
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
	base, err := s.GetBundleByName(testContext, "base")
	if err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, s, &Bundle{Name: "child", ExtendsID: base.ID, Enabled: true})
	got, err := s.GetBundleByName(testContext, "child")
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

	_, err := s.UpsertBundle(testContext, &Bundle{Name: "b", Extends: "a", Enabled: true})
	if err == nil {
		t.Fatal("expected re-parenting b under its own descendant to be rejected")
	}
	if !strings.Contains(err.Error(), "own inheritance chain") {
		t.Fatalf("the rejection must say what is wrong, got: %v", err)
	}
	// And it must not have half-applied: b still has no parent.
	b, err := s.GetBundleByName(testContext, "b")
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
	if _, err := s.UpsertBundle(testContext, &Bundle{Name: "loop", Extends: "loop", Enabled: true}); err == nil {
		t.Fatal("expected a bundle extending itself to be rejected")
	}
}

// ON DELETE RESTRICT, driven for real. SET NULL would have made the child a
// root, inheriting nothing, with no error anywhere.
func TestDeletingAnExtendedBundleRefused(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "react", Extends: "base", Enabled: true})
	base, err := s.GetBundleByName(testContext, "base")
	if err != nil {
		t.Fatal(err)
	}

	err = s.DeleteBundle(testContext, base.ID)
	if !errors.Is(err, ErrParentHasChildren) {
		t.Fatalf("deleting an extended bundle must be refused, got: %v", err)
	}
	if !strings.Contains(err.Error(), "react") {
		t.Fatalf("the refusal must name the children, got: %v", err)
	}
	if _, err := s.GetBundleByName(testContext, "base"); err != nil {
		t.Fatalf("the refused delete must not have removed anything: %v", err)
	}

	// And the schema clause holds on its own, without the Go check in front of
	// it — which is what makes this RESTRICT rather than SET NULL.
	expectTheDatabaseToRefuse(t, s, `DELETE FROM bundles WHERE id=$1`, base.ID)

	// Cut the link and it deletes.
	mustUpsert(t, s, &Bundle{Name: "react", Enabled: true})
	if err := s.DeleteBundle(testContext, base.ID); err != nil {
		t.Fatalf("deleting a bundle nothing extends: %v", err)
	}
}
