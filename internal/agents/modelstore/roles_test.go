package modelstore

import (
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore opens a throwaway store with two real models so role
// assignment has something valid to point at.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	for _, m := range []Model{
		{ID: "claude-fable-5-1", Provider: "anthropic", Name: "Fable 5.1", MaxTokens: 1000000, Enabled: true},
		{ID: "gpt-5.6-luna", Provider: "openai", Name: "Luna", MaxTokens: 400000, Enabled: true},
	} {
		if err := s.AddModel(m); err != nil {
			t.Fatalf("seed model %s: %v", m.ID, err)
		}
	}
	return s
}

func TestSetAndResolveRole(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetRole(RoleEfficient, "gpt-5.6-luna"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	m, err := s.ResolveRole(RoleEfficient)
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if m.ID != "gpt-5.6-luna" {
		t.Fatalf("efficient resolved to %q, want gpt-5.6-luna", m.ID)
	}
}

func TestSetRoleRejectsUnknownRole(t *testing.T) {
	s := newTestStore(t)
	// A typo must not create a silent fourth role.
	if err := s.SetRole("effcient", "gpt-5.6-luna"); err == nil {
		t.Fatal("SetRole accepted an unknown role name; want rejection")
	}
}

func TestSetRoleRejectsUnknownModel(t *testing.T) {
	s := newTestStore(t)
	// A role may only ever name a model that exists.
	if err := s.SetRole(RoleBest, "claude-imaginary-9"); err == nil {
		t.Fatal("SetRole accepted a nonexistent model; want rejection")
	}
}

func TestResolveUnassignedRoleFailsLoud(t *testing.T) {
	s := newTestStore(t)
	// No fabricated fallback: an unassigned role is an error, not a guess.
	if _, err := s.ResolveRole(RoleDefault); err == nil {
		t.Fatal("ResolveRole on an unassigned role returned no error; want fail-loud")
	}
}

func TestSetRoleIsIdempotentUpsert(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetRole(RoleDefault, "claude-fable-5-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(RoleDefault, "gpt-5.6-luna"); err != nil {
		t.Fatal(err)
	}
	m, err := s.ResolveRole(RoleDefault)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "gpt-5.6-luna" {
		t.Fatalf("re-set role resolved to %q, want gpt-5.6-luna", m.ID)
	}
	roles, err := s.Roles()
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role row after upsert, got %d", len(roles))
	}
}

func TestAllModelsWithStatusCarriesAliases(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddAlias("gpt-5.6-luna", "luna"); err != nil {
		t.Fatal(err)
	}
	statuses, err := s.AllModelsWithStatus()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ms := range statuses {
		if ms.ID == "gpt-5.6-luna" {
			found = true
			if len(ms.Aliases) != 1 || ms.Aliases[0] != "luna" {
				t.Fatalf("aliases = %v, want [luna] — the aliases:null regression is back", ms.Aliases)
			}
		}
	}
	if !found {
		t.Fatal("gpt-5.6-luna missing from AllModelsWithStatus")
	}
}

// ResolveModel is the one resolver every caller (including `ms resolve`) goes
// through, so a canonical role name must resolve there too — not only via
// ResolveRole.
func TestResolveModelAcceptsCanonicalRoleName(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetRole(RoleEfficient, "gpt-5.6-luna"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	m, err := s.ResolveModel(RoleEfficient)
	if err != nil {
		t.Fatalf("ResolveModel(%q): %v", RoleEfficient, err)
	}
	if m.ID != "gpt-5.6-luna" {
		t.Fatalf("ResolveModel(%q) = %q, want gpt-5.6-luna", RoleEfficient, m.ID)
	}
}

func TestResolveModelUnassignedRoleFailsLoud(t *testing.T) {
	s := newTestStore(t)
	m, err := s.ResolveModel(RoleBest)
	if err == nil {
		t.Fatalf("ResolveModel(%q) with no assignment returned %q, want error", RoleBest, m.ID)
	}
	if !strings.Contains(err.Error(), "not assigned") {
		t.Fatalf("error should say the role is unassigned, got: %v", err)
	}
}

func TestResolveModelAliasStillResolves(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddAlias("claude-fable-5-1", "fable"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	m, err := s.ResolveModel("fable")
	if err != nil {
		t.Fatalf("ResolveModel(alias): %v", err)
	}
	if m.ID != "claude-fable-5-1" {
		t.Fatalf("alias resolved to %q, want claude-fable-5-1", m.ID)
	}
}

// A real alias that happens to share a role's name wins over the role: the
// lookup order is id, alias, role, and this pins it.
func TestResolveModelAliasNamedLikeRoleWinsOverRole(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetRole(RoleBest, "gpt-5.6-luna"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if err := s.AddAlias("claude-fable-5-1", RoleBest); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	m, err := s.ResolveModel(RoleBest)
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if m.ID != "claude-fable-5-1" {
		t.Fatalf("alias %q should win over the role, got %q", RoleBest, m.ID)
	}
}

func TestResolveModelUnknownNameStillErrors(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ResolveModel("no-such-model"); err == nil {
		t.Fatal("ResolveModel of an unknown name returned nil error")
	}
}

func addThirdModel(t *testing.T, s *Store) {
	t.Helper()
	if err := s.AddModel(Model{ID: "gpt-5.6-terra", Provider: "openai", Name: "Terra", MaxTokens: 400000, Enabled: true}); err != nil {
		t.Fatalf("seed terra: %v", err)
	}
}

func TestSetRoleModelsKeepsOrder(t *testing.T) {
	s := newTestStore(t)
	addThirdModel(t, s)
	want := []string{"gpt-5.6-terra", "claude-fable-5-1", "gpt-5.6-luna"}
	if err := s.SetRoleModels(RoleBalanced, want); err != nil {
		t.Fatalf("SetRoleModels: %v", err)
	}
	got, err := s.RoleModelIDs(RoleBalanced)
	if err != nil {
		t.Fatalf("RoleModelIDs: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list = %v, want %v", got, want)
	}
	// A reader that knows only the first model still gets it.
	first, err := s.ResolveRole(RoleBalanced)
	if err != nil || first.ID != "gpt-5.6-terra" {
		t.Fatalf("ResolveRole = %v, %v; want gpt-5.6-terra", first, err)
	}
	models, err := s.RoleModels(RoleBalanced)
	if err != nil || len(models) != 3 || models[2].Provider != "openai" {
		t.Fatalf("RoleModels = %v, %v", models, err)
	}
}

func TestSetRoleModelsRefusesBadLists(t *testing.T) {
	s := newTestStore(t)
	for name, list := range map[string][]string{
		"empty":         {},
		"duplicate":     {"gpt-5.6-luna", "gpt-5.6-luna"},
		"unknown model": {"gpt-5.6-luna", "claude-imaginary-9"},
	} {
		if err := s.SetRoleModels(RoleEfficient, list); err == nil {
			t.Errorf("%s list accepted; want refusal", name)
		}
	}
	if err := s.SetRoleModels("budget", []string{"gpt-5.6-luna"}); err == nil {
		t.Error("unknown role accepted; want refusal")
	}
	// A refused list leaves nothing half-written.
	if _, err := s.RoleModelIDs(RoleEfficient); err == nil {
		t.Error("a refused list left the role assigned")
	}
}

func TestSetRoleKeepsFallbacksAndDropsTheNewFirstFromThem(t *testing.T) {
	s := newTestStore(t)
	addThirdModel(t, s)
	if err := s.SetRoleModels(RoleDefault, []string{"claude-fable-5-1", "gpt-5.6-terra", "gpt-5.6-luna"}); err != nil {
		t.Fatalf("SetRoleModels: %v", err)
	}
	if err := s.SetRole(RoleDefault, "gpt-5.6-terra"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	got, _ := s.RoleModelIDs(RoleDefault)
	if strings.Join(got, ",") != "gpt-5.6-terra,gpt-5.6-luna" {
		t.Fatalf("list after SetRole = %v, want [gpt-5.6-terra gpt-5.6-luna]", got)
	}
}

func TestRemoveRoleClearsFallbacks(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetRoleModels(RoleBest, []string{"claude-fable-5-1", "gpt-5.6-luna"}); err != nil {
		t.Fatalf("SetRoleModels: %v", err)
	}
	if err := s.RemoveRole(RoleBest); err != nil {
		t.Fatalf("RemoveRole: %v", err)
	}
	if err := s.SetRole(RoleBest, "gpt-5.6-luna"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	got, _ := s.RoleModelIDs(RoleBest)
	if strings.Join(got, ",") != "gpt-5.6-luna" {
		t.Fatalf("list = %v; a removed role kept its old fallbacks", got)
	}
}
