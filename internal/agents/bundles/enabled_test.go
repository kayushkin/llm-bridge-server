package bundles

import (
	"errors"
	"strconv"
	"testing"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// Store.SetEnabled was executed by no test until this file existed. It is four
// lines and every one of them carries a decision: which column the UPDATE
// writes, that updated_at moves with it, that a zero-row UPDATE is ErrNotFound
// rather than success, and that the WHERE clause names one row.

func TestSetEnabledTogglesInBothDirections(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "go-service", MatchTags: []string{"go"}, Enabled: true}
	mustUpsert(t, s, b)

	if err := s.SetEnabled(testContext, b.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBundle(testContext, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("SetEnabled(false) left the bundle enabled")
	}

	if err := s.SetEnabled(testContext, b.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetBundle(testContext, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("SetEnabled(true) left the bundle disabled")
	}
}

// The RowsAffected branch. An UPDATE that matches nothing is not an error in
// SQLite, so without this check the store would answer nil and every caller
// above it would report success for a bundle that does not exist.
func TestSetEnabledOnAMissingBundleIsNotFound(t *testing.T) {
	s := openTest(t)
	err := s.SetEnabled(testContext, 4242, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound — a zero-row UPDATE is not an error to SQLite, "+
			"so this can only come from the RowsAffected check", err)
	}
}

// DeleteBundle's identical RowsAffected branch. This one is outside the seam
// this file was opened for, and it is here because the scorer found it: a case
// aimed at SetEnabled matched the same text in DeleteBundle first and scored
// UNNOTICED, which was a correct verdict about the wrong function. Deleting a
// bundle that does not exist answered nil.
func TestDeleteBundleOnAMissingBundleIsNotFound(t *testing.T) {
	s := openTest(t)
	err := s.DeleteBundle(testContext, 4242)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound — a DELETE that matches no row is not an error to SQLite", err)
	}
}

func TestSetEnabledStampsUpdatedAt(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "base", Enabled: true}
	mustUpsert(t, s, b)
	// Move the stored timestamp into the past behind the API so the assertion
	// does not depend on the clock ticking between two calls in the same
	// millisecond.
	writeBehindTheAPI(t, s, `UPDATE bundles SET updated_at=1 WHERE id=$1`, b.ID)

	if err := s.SetEnabled(testContext, b.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBundle(testContext, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt <= 1 {
		t.Fatalf("updated_at is still %d — SetEnabled changed the flag without recording that it did", got.UpdatedAt)
	}
}

// SetEnabled names one column and one row. Both halves are worth pinning: the
// UPDATE could widen (clobbering a curated definition on a disable) or the
// WHERE could go missing (disabling the whole table).
func TestSetEnabledChangesNothingButTheFlagOnOneRow(t *testing.T) {
	s := openTest(t)
	target := &Bundle{
		Name:        "react",
		DisplayName: "React frontend",
		Description: "frontend work",
		MatchTags:   []string{"react", "frontend"},
		Model:       "sonnet",
		Effort:      "high",
		Enabled:     true,
		Members: []Member{
			{Kind: MemberSkill, ID: skillBrowserAutomation, Name: "browser-automation"},
			{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"},
		},
	}
	mustUpsert(t, s, target)
	sibling := &Bundle{Name: "base", DisplayName: "Base", Enabled: true}
	mustUpsert(t, s, sibling)

	before, err := s.GetBundle(testContext, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(testContext, target.ID, false); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetBundle(testContext, target.ID)
	if err != nil {
		t.Fatal(err)
	}

	if after.Name != before.Name || after.DisplayName != before.DisplayName ||
		after.Description != before.Description || after.Model != before.Model ||
		after.Effort != before.Effort {
		t.Fatalf("SetEnabled rewrote the definition: %+v -> %+v", before, after)
	}
	if len(after.MatchTags) != len(before.MatchTags) {
		t.Fatalf("match tags moved: %v -> %v", before.MatchTags, after.MatchTags)
	}
	if len(after.Members) != len(before.Members) {
		t.Fatalf("member set moved: %d -> %d members", len(before.Members), len(after.Members))
	}
	if after.CreatedAt != before.CreatedAt {
		t.Fatalf("created_at moved: %d -> %d", before.CreatedAt, after.CreatedAt)
	}

	other, err := s.GetBundle(testContext, sibling.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !other.Enabled {
		t.Fatal("disabling one bundle disabled another — the UPDATE's WHERE clause is not selecting a single row")
	}
}

// EnabledOnly is the read side of the same flag. The two listings must disagree
// after a disable: the filtered one drops the row, the unfiltered one keeps it.
// Asserting only the filtered list would also pass if the row had been deleted.
func TestDisablingHidesTheBundleFromTheEnabledOnlyListingAndNowhereElse(t *testing.T) {
	s := openTest(t)
	b := &Bundle{Name: "go-service", MatchTags: []string{"go"}, Enabled: true}
	mustUpsert(t, s, b)

	if err := s.SetEnabled(testContext, b.ID, false); err != nil {
		t.Fatal(err)
	}

	enabled, err := s.ListBundles(testContext, ListFilter{EnabledOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range enabled {
		if x.ID == b.ID {
			t.Fatal("a disabled bundle is still in the EnabledOnly listing")
		}
	}

	all, err := s.ListBundles(testContext, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range all {
		if x.ID == b.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the disabled bundle vanished from the unfiltered listing too — disable is not delete")
	}
}

// The flag's whole purpose. Resolve selects over ListBundles(EnabledOnly:true),
// so a disabled bundle must stop contributing members. This is the invariant
// that spans three declarations — Store.SetEnabled writes it, ListFilter
// filters on it, Resolve acts on it — and none of the three tested it alone.
func TestADisabledBundleContributesNothingToResolve(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	react := &Bundle{
		Name:      "react",
		MatchTags: []string{"react"},
		Enabled:   true,
		Members: []Member{
			{Kind: MemberSkill, ID: skillBrowserAutomation, Name: "browser-automation"},
			{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"},
		},
	}
	mustUpsert(t, s, react)

	resp, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(resp.Skills, skillBrowserAutomation) || !hasID(resp.Tools, toolPlaywright) {
		t.Fatalf("fixture is broken: enabled bundle did not contribute (%+v)", resp)
	}

	if err := s.SetEnabled(testContext, react.ID, false); err != nil {
		t.Fatal(err)
	}
	resp, err = Resolve(testContext, s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	if hasID(resp.Skills, skillBrowserAutomation) || hasID(resp.Tools, toolPlaywright) {
		t.Fatalf("a disabled bundle still provisioned its members: %+v", resp)
	}
	if hasBundle(resp.Bundles, "react") {
		t.Fatalf("a disabled bundle is still listed as having contributed: %+v", resp.Bundles)
	}
}

// ⚠️ Characterisation, not an endorsement — and the asymmetry is deliberate to
// record because it is the one place `enabled` does NOT mean "contributes
// nothing".
//
// Resolve filters the SELECTED set with EnabledOnly, but expandChain walks
// ExtendsID through Store.GetBundle, which has no enabled clause. So disabling
// a bundle stops it being selected on its own account and does NOT stop it
// contributing members to its children. Disabling "base" while "react extends
// base" leaves every base member in the resolved output.
//
// Whether that is right is a curator's question, not a test's: inheritance is
// arguably structural, and a parent that vanished mid-chain would silently
// resolve to a smaller bundle — which expandChain refuses to do for a *missing*
// parent, loudly. The behavior is pinned here so that changing it has to be a
// decision rather than an accident, and so that a reader who assumes "disabled
// means gone" meets this test instead of a production surprise.
func TestADisabledParentStillContributesThroughInheritance(t *testing.T) {
	s := openTest(t)
	base := &Bundle{
		Name:    "base",
		Enabled: true,
		Members: []Member{{Kind: MemberSkill, ID: skillBackendPatterns, Name: "cc-skill-backend-patterns"}},
	}
	mustUpsert(t, s, base)
	mustUpsert(t, s, &Bundle{
		Name:      "react",
		Extends:   "base",
		MatchTags: []string{"react"},
		Enabled:   true,
		Members:   []Member{{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"}},
	})

	if err := s.SetEnabled(testContext, base.ID, false); err != nil {
		t.Fatal(err)
	}
	resp, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}

	if !hasID(resp.Tools, toolPlaywright) {
		t.Fatalf("disabling the parent removed the child's own members: %+v", resp)
	}
	if !hasID(resp.Skills, skillBackendPatterns) {
		t.Fatalf("a disabled parent no longer contributes through the Extends chain. That may be "+
			"an improvement, but it is a behavior change: expandChain uses GetBundle, which has "+
			"no enabled filter. Update this test deliberately. Got: %+v", resp)
	}
}
