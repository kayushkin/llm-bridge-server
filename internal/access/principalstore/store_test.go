package principalstore

import (
	"errors"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, kind, displayName, email string) *Principal {
	t.Helper()
	created, err := s.Create(&Principal{Kind: kind, DisplayName: displayName, Email: email})
	if err != nil {
		t.Fatalf("create %s %q: %v", kind, displayName, err)
	}
	return created
}

func TestCreateAssignsAPrefixedID(t *testing.T) {
	s := newTestStore(t)
	first := mustCreate(t, s, KindHuman, "Slava Kayushkin", "slava@kayushkin.com")
	if first.ID != "principal_000001" {
		t.Fatalf("first id = %q, want principal_000001", first.ID)
	}
	second := mustCreate(t, s, KindGroup, "Data Team", "")
	if second.ID != "principal_000002" {
		t.Fatalf("second id = %q, want principal_000002", second.ID)
	}
	if strings.Contains(first.ID, "/") {
		t.Fatalf("id %q contains a slash", first.ID)
	}
}

func TestUnknownKindNamesTheVocabulary(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(&Principal{Kind: "robot", DisplayName: "Marvin"})
	if !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("err = %v, want ErrInvalidPrincipal", err)
	}
	for _, k := range Kinds {
		if !strings.Contains(err.Error(), k) {
			t.Fatalf("error should list %q, got: %v", k, err)
		}
	}
	// Case is not a reason to refuse.
	if _, err := s.Create(&Principal{Kind: "Human", DisplayName: "Ok"}); err != nil {
		t.Fatalf("mixed-case kind refused: %v", err)
	}
}

func TestDisplayNameIsRequired(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(&Principal{Kind: KindHuman, DisplayName: "   "})
	if !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("err = %v, want ErrInvalidPrincipal", err)
	}
	if !strings.Contains(err.Error(), "display_name") {
		t.Fatalf("error should name the field, got: %v", err)
	}
}

func TestMembershipEnforcesKinds(t *testing.T) {
	s := newTestStore(t)
	slava := mustCreate(t, s, KindHuman, "Slava Kayushkin", "")
	priya := mustCreate(t, s, KindHuman, "Priya Raman", "")
	data := mustCreate(t, s, KindGroup, "Data Team", "")

	// A human in the group slot.
	_, err := s.AddMember(slava.ID, priya.ID)
	if !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("human as group: err = %v, want ErrInvalidMembership", err)
	}
	if !strings.Contains(err.Error(), "not a group") {
		t.Fatalf("error should say it is not a group, got: %v", err)
	}
	// A missing row on either side is a 404, not a 400.
	if _, err := s.AddMember("principal_999999", priya.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: err = %v, want ErrNotFound", err)
	}
	if _, err := s.AddMember(data.ID, "principal_999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: err = %v, want ErrNotFound", err)
	}
	if _, err := s.ListMembers(slava.ID, false); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("members of a human: err = %v, want ErrInvalidMembership", err)
	}
	if _, err := s.ListGroups(data.ID, false); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("groups of a group: err = %v, want ErrInvalidMembership", err)
	}
}

func TestNoNestedGroups(t *testing.T) {
	s := newTestStore(t)
	data := mustCreate(t, s, KindGroup, "Data Team", "")
	security := mustCreate(t, s, KindGroup, "Security", "")
	_, err := s.AddMember(data.ID, security.ID)
	if !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("err = %v, want ErrInvalidMembership", err)
	}
	if !strings.Contains(err.Error(), "nested groups") {
		t.Fatalf("error should say nested groups are unsupported, got: %v", err)
	}
}

func TestAddMemberIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	priya := mustCreate(t, s, KindHuman, "Priya Raman", "")
	data := mustCreate(t, s, KindGroup, "Data Team", "")
	created, err := s.AddMember(data.ID, priya.ID)
	if err != nil || !created {
		t.Fatalf("first add: created=%v err=%v", created, err)
	}
	created, err = s.AddMember(data.ID, priya.ID)
	if err != nil || created {
		t.Fatalf("second add: created=%v err=%v, want false and nil", created, err)
	}
	members, err := s.ListMembers(data.ID, false)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %d (%v), want 1", len(members), err)
	}
	groups, err := s.ListGroups(priya.ID, false)
	if err != nil || len(groups) != 1 || groups[0].ID != data.ID {
		t.Fatalf("groups = %v (%v), want [%s]", groups, err, data.ID)
	}
	if err := s.RemoveMember(data.ID, priya.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.RemoveMember(data.ID, priya.ID); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("second remove: err = %v, want ErrNotAMember", err)
	}
}

func TestDisabledIsHiddenByDefaultButGettable(t *testing.T) {
	s := newTestStore(t)
	marcus := mustCreate(t, s, KindHuman, "Marcus Feld", "")
	security := mustCreate(t, s, KindGroup, "Security", "")
	if _, err := s.AddMember(security.ID, marcus.ID); err != nil {
		t.Fatalf("add: %v", err)
	}
	disabled, err := s.Disable(marcus.ID)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.DisabledAt == 0 {
		t.Fatalf("disabled_at not set")
	}
	// A second disable does not move the timestamp.
	again, _ := s.Disable(marcus.ID)
	if again.DisabledAt != disabled.DisabledAt {
		t.Fatalf("disable is not idempotent: %d then %d", disabled.DisabledAt, again.DisabledAt)
	}

	list, err := s.List(Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range list {
		if p.ID == marcus.ID {
			t.Fatalf("disabled principal appeared in the default listing")
		}
	}
	list, _ = s.List(Filter{IncludeDisabled: true})
	if len(list) != 2 {
		t.Fatalf("include_disabled listing has %d rows, want 2", len(list))
	}
	// Still gettable: a card assigned to him still needs to render.
	got, err := s.Get(marcus.ID, false)
	if err != nil {
		t.Fatalf("get disabled: %v", err)
	}
	if got.DisabledAt == 0 || got.DisplayName != "Marcus Feld" {
		t.Fatalf("get returned %+v", got)
	}
	// Hidden from the group's member list by default, visible on request.
	members, _ := s.ListMembers(security.ID, false)
	if len(members) != 0 {
		t.Fatalf("disabled member listed by default")
	}
	members, _ = s.ListMembers(security.ID, true)
	if len(members) != 1 {
		t.Fatalf("include_disabled member list has %d rows, want 1", len(members))
	}

	enabled, err := s.Enable(marcus.ID)
	if err != nil || enabled.DisabledAt != 0 {
		t.Fatalf("enable: %+v %v", enabled, err)
	}
}

func TestSearchFindsAPartialName(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, KindHuman, "Priya Raman", "priya.raman@northwind-eng.example")
	mustCreate(t, s, KindHuman, "Marcus Feld", "marcus.feld@northwind-eng.example")
	mustCreate(t, s, KindGroup, "Data Team", "")

	for query, want := range map[string]string{
		"pri":               "Priya Raman",
		"Ram":               "Priya Raman",
		"feld":              "Marcus Feld",
		"marcus.feld":       "Marcus Feld",
		`"Data Team"`:       "Data Team",
		"priya northwind":   "Priya Raman",
		"display_name:data": "Data Team", // a colon is FTS5 syntax: passed through raw, whole-token match
	} {
		got, err := s.List(Filter{Query: query})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(got) != 1 || got[0].DisplayName != want {
			names := make([]string, 0, len(got))
			for _, p := range got {
				names = append(names, p.DisplayName)
			}
			t.Fatalf("search %q = %v, want [%s]", query, names, want)
		}
	}
	got, _ := s.List(Filter{Query: "northwind"})
	if len(got) != 2 {
		t.Fatalf("search by email domain found %d, want 2", len(got))
	}
	got, _ = s.List(Filter{Query: "pri", Kind: KindGroup})
	if len(got) != 0 {
		t.Fatalf("kind filter did not narrow the search")
	}
	if _, err := s.List(Filter{Query: `"unbalanced`}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("malformed query: err = %v, want ErrInvalidPrincipal", err)
	}
	if _, err := s.List(Filter{Kind: "robot"}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("unknown kind filter: err = %v, want ErrInvalidPrincipal", err)
	}
}

func TestPatchRenamesAndReindexes(t *testing.T) {
	s := newTestStore(t)
	p := mustCreate(t, s, KindHuman, "Dinesh Okonkwo", "")
	name := "Dinesh Okonkwo-Reyes"
	updated, err := s.Patch(p.ID, Patch{DisplayName: &name})
	if err != nil || updated.DisplayName != name {
		t.Fatalf("patch: %+v %v", updated, err)
	}
	got, _ := s.List(Filter{Query: "reyes"})
	if len(got) != 1 {
		t.Fatalf("FTS index did not follow the rename")
	}
	blank := " "
	if _, err := s.Patch(p.ID, Patch{DisplayName: &blank}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("blank rename accepted: %v", err)
	}
}

func TestCountsCoverEveryRow(t *testing.T) {
	s := newTestStore(t)
	h := mustCreate(t, s, KindHuman, "Helena Vos", "")
	mustCreate(t, s, KindHuman, "Slava Kayushkin", "")
	mustCreate(t, s, KindGroup, "Security", "")
	if _, err := s.Disable(h.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	c, err := s.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c != (Counts{Principals: 3, Humans: 2, Groups: 1, Disabled: 1}) {
		t.Fatalf("counts = %+v", c)
	}
}

// TestOnlyAHumanCanBeAnAdministrator pins the one fact that lets a principal
// past every per-resource check the other stores make: it is set through
// PATCH, it survives a read, and a group is refused it.
func TestOnlyAHumanCanBeAnAdministrator(t *testing.T) {
	s := newTestStore(t)
	operator := mustCreate(t, s, KindHuman, "Slava Kayushkin", "slava@kayushkin.com")
	if operator.IsAdministrator {
		t.Fatal("a new human must not be an administrator until someone says so")
	}
	yes := true
	promoted, err := s.Patch(operator.ID, Patch{IsAdministrator: &yes})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !promoted.IsAdministrator {
		t.Fatal("PATCH is_administrator=true did not take")
	}
	readBack, err := s.Get(operator.ID, false)
	if err != nil || !readBack.IsAdministrator {
		t.Fatalf("read back: %+v %v", readBack, err)
	}

	group := mustCreate(t, s, KindGroup, "Operators", "")
	if _, err := s.Patch(group.ID, Patch{IsAdministrator: &yes}); err == nil {
		t.Fatal("a group must not be an administrator")
	} else if !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("group refusal should be ErrInvalidPrincipal, got %v", err)
	}

	no := false
	demoted, err := s.Patch(operator.ID, Patch{IsAdministrator: &no})
	if err != nil || demoted.IsAdministrator {
		t.Fatalf("demote: %+v %v", demoted, err)
	}
	// A patch that says nothing about it leaves it alone.
	name := "Slava K"
	unchanged, err := s.Patch(operator.ID, Patch{DisplayName: &name})
	if err != nil || unchanged.IsAdministrator {
		t.Fatalf("a rename must not change administrator: %+v %v", unchanged, err)
	}
}
