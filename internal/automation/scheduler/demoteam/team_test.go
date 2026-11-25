package demoteam

import "testing"

func TestSlugsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Roster {
		if seen[m.Slug] {
			t.Errorf("duplicate slug %q — the slug is what everything joins on", m.Slug)
		}
		seen[m.Slug] = true
	}
}

func TestEveryMemberHasAGovernedRole(t *testing.T) {
	known := map[Role]bool{
		RoleBuilder: true, RoleReviewer: true, RoleTester: true,
		RoleResearcher: true, RoleDocs: true,
	}
	for _, m := range Roster {
		if !known[m.Role] {
			t.Errorf("%s has role %q, which is outside the governed vocabulary", m.Slug, m.Role)
		}
		if m.Voice == "" {
			t.Errorf("%s has no voice; the model would have nothing to write as", m.Slug)
		}
	}
}

// Exactly one member writes code. Peer agents writing in parallel is the
// documented failure case in TEAM-ORCHESTRATION; verifiers are the useful
// shape, and this pins the team to that shape.
func TestOnlyOneMemberWritesCode(t *testing.T) {
	builders := WithRole(RoleBuilder)
	if len(builders) != 1 {
		t.Fatalf("%d members hold role builder: %v — two agents writing the same branch is the failure case",
			len(builders), builders)
	}
}

func TestLookupsFindMembersAndRejectStrangers(t *testing.T) {
	if _, ok := BySlug("northwind-priya"); !ok {
		t.Error("BySlug missed a member of the roster")
	}
	if _, ok := BySlug("northwind-nobody"); ok {
		t.Error("BySlug invented a member")
	}
	if m, ok := ByEmail("PRIYA.RAMAN@NORTHWIND-ENG.EXAMPLE"); !ok || m.Slug != "northwind-priya" {
		t.Error("ByEmail should match regardless of case")
	}
	if _, ok := ByEmail("ci-bot@northwind-eng.example"); ok {
		t.Error("a machine sender resolved to a team identity")
	}
}
