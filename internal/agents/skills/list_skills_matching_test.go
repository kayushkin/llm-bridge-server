package skills

import (
	"strings"
	"testing"
)

func listedNames(t *testing.T, s *Store, f ListSkillsFilter) string {
	t.Helper()
	skills, err := s.ListSkills(testContext, f)
	if err != nil {
		t.Fatalf("list skills %+v: %v", f, err)
	}
	names := make([]string, 0, len(skills))
	for _, sk := range skills {
		names = append(names, sk.Name)
	}
	return strings.Join(names, ",")
}

// The standalone store searched with SQLite's LIKE: case-insensitive, with %
// and _ as wildcards and a backslash an ordinary character, and it sorted by
// byte order. The Postgres store must answer the same.
func TestSkillSearchMatchesAndSortsAsTheSQLiteStoreDid(t *testing.T) {
	s := newTestStore(t)
	source := &Source{Name: "src", RepoURL: "https://example.invalid/src.git"}
	if err := s.UpsertSource(testContext, source); err != nil {
		t.Fatal(err)
	}
	for _, sk := range []*Skill{
		{Name: "beta", Description: "Reads PDF files", Tags: []string{"Docs"}},
		{Name: "Zeta", Description: "back\\slash"},
		{Name: "alpha", Description: "plain"},
		{Name: "a_b", Description: "underscore"},
	} {
		sk.SourceID, sk.SkillMD = source.ID, "body"
		if _, err := s.UpsertSkill(testContext, sk); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		filter ListSkillsFilter
		want   string
	}{
		{ListSkillsFilter{}, "Zeta,a_b,alpha,beta"},
		{ListSkillsFilter{Query: "pdf"}, "beta"},
		{ListSkillsFilter{Query: "ALPHA"}, "alpha"},
		{ListSkillsFilter{Query: "l_h"}, "alpha"},
		{ListSkillsFilter{Query: "k\\s"}, "Zeta"},
		{ListSkillsFilter{Tag: "docs"}, "beta"},
		{ListSkillsFilter{Limit: 2}, "Zeta,a_b"},
	} {
		if got := listedNames(t, s, check.filter); got != check.want {
			t.Errorf("ListSkills(%+v) = %s, want %s", check.filter, got, check.want)
		}
	}
}
