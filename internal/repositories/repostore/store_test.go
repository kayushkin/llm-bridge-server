package repostore

import (
	"path/filepath"
	"testing"
)

func hasTag(tags []string, want string) bool {
	for _, x := range tags {
		if x == want {
			return true
		}
	}
	return false
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rs"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertGetRepo(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "dash", Path: "/x/dash", Tags: []string{"go", "react"}, TagOverrides: []string{"primary"}}
	inserted, err := s.UpsertRepo(r)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("expected insert on first upsert")
	}
	if r.ID == 0 {
		t.Fatal("ID not set after insert")
	}

	got, err := s.GetRepoByName("dash")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go", "react", "primary"} {
		if !hasTag(got.EffectiveTags, want) {
			t.Fatalf("effective tags missing %q: %v", want, got.EffectiveTags)
		}
	}

	// Update must preserve id, not re-insert, and change the path.
	r.Path = "/y/dash"
	ins2, err := s.UpsertRepo(r)
	if err != nil {
		t.Fatal(err)
	}
	if ins2 {
		t.Fatal("expected update, not insert, on second upsert")
	}
	got2, err := s.GetRepo(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Path != "/y/dash" {
		t.Fatalf("path not updated: %s", got2.Path)
	}
}

func TestListByTagIncludesOverrides(t *testing.T) {
	s := openTest(t)
	mustUpsertRepo(t, s, &Repo{Name: "a", Tags: []string{"go"}})
	mustUpsertRepo(t, s, &Repo{Name: "b", Tags: []string{"react"}})
	mustUpsertRepo(t, s, &Repo{Name: "c", TagOverrides: []string{"react"}}) // override-only match

	got, err := s.ListRepos(ListFilter{Tag: "react"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 react repos (including override-only), got %d: %v", len(got), repoNames(got))
	}
}

func TestGetByPathAndDelete(t *testing.T) {
	s := openTest(t)
	r := &Repo{Name: "z", Path: "/p/z"}
	mustUpsertRepo(t, s, r)

	byPath, err := s.GetRepoByPath("/p/z")
	if err != nil || byPath.Name != "z" {
		t.Fatalf("get by path: %v / %+v", err, byPath)
	}

	if err := s.DeleteRepo(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRepo(r.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func mustUpsertRepo(t *testing.T, s *Store, r *Repo) {
	t.Helper()
	if _, err := s.UpsertRepo(r); err != nil {
		t.Fatal(err)
	}
}

func repoNames(rs []Repo) []string {
	var o []string
	for _, r := range rs {
		o = append(o, r.Name)
	}
	return o
}
