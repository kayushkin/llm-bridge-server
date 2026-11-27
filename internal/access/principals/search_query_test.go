package principals

import (
	"errors"
	"testing"
)

func TestSearchQueriesReadAsTheStandaloneStoreReadThem(t *testing.T) {
	for query, want := range map[string]string{
		"pri":                      `'pri':*`,
		"Priya northwind":          `'priya':* & 'northwind':*`,
		"marcus.feld":              `(('marcus':A <-> 'feld':*A) | ('marcus':B <-> 'feld':*B))`,
		`"Data Team"`:              `(('data':A <-> 'team':A) | ('data':B <-> 'team':B))`,
		`"Data Team"*`:             `(('data':A <-> 'team':*A) | ('data':B <-> 'team':*B))`,
		"display_name:data":        `'data':A`,
		`email:"northwind eng"`:    `('northwind':B <-> 'eng':B)`,
		"priya OR marcus":          `('priya' | 'marcus')`,
		"priya NOT raman":          `('priya' & !'raman')`,
		"pri* AND (feld OR raman)": `('pri':* & ('feld' | 'raman'))`,
		"it's":                     `(('it':A <-> 's':*A) | ('it':B <-> 's':*B))`,
		`a\b`:                      `(('a':A <-> 'b':*A) | ('a':B <-> 'b':*B))`,
	} {
		got, err := searchTSQueryOf(query)
		if err != nil {
			t.Errorf("%q: %v", query, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %s, want %s", query, got, want)
		}
	}
}

func TestASearchQueryThatCannotBeReadIsTheCallersMistake(t *testing.T) {
	for _, query := range []string{
		`"unbalanced`, `pri NEAR feld`, `title:x`, `(pri`, `pri OR`, `NOT pri`, `*`, `"x" ^pri`, `""`, `display_name: data`,
	} {
		if _, err := searchTSQueryOf(query); !errors.Is(err, ErrInvalidPrincipal) {
			t.Errorf("%q: err = %v, want ErrInvalidPrincipal", query, err)
		}
	}
}

func TestSearchRunsAgainstTheDocument(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, KindHuman, "Priya Raman", "priya.raman@northwind-eng.example")
	mustCreate(t, s, KindHuman, "Raman Priya", "")
	mustCreate(t, s, KindGroup, "Data Team", "")
	mustCreate(t, s, KindHuman, "José Ortiz", "")
	for query, want := range map[string][]string{
		// A phrase never runs from the end of the name into the address.
		`"raman priya"`:          {"Raman Priya"},
		"priya OR data":          {"Data Team", "Priya Raman", "Raman Priya"},
		"priya NOT eng":          {"Raman Priya"},
		"email:northwind":        {"Priya Raman"},
		"display_name:northwind": {},
		"northwind-eng":          {"Priya Raman"},
	} {
		got, err := s.List(testContext, Filter{Query: query})
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		names := []string{}
		for _, p := range got {
			names = append(names, p.DisplayName)
		}
		if len(names) != len(want) {
			t.Errorf("%q = %v, want %v", query, names, want)
			continue
		}
		for index := range names {
			if names[index] != want[index] {
				t.Errorf("%q = %v, want %v", query, names, want)
				break
			}
		}
	}
}

func TestListOrderFoldsASCIICaseAndThenComparesBytes(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"bob", "Alice", "Émile", "alice", "Zed"} {
		mustCreate(t, s, KindHuman, name, "")
	}
	got, err := s.List(testContext, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Alice", "alice", "bob", "Zed", "Émile"}
	for index, p := range got {
		if p.DisplayName != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOnlyTheExactFormOfAnIDNamesARow(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, KindHuman, "Priya Raman", "")
	for _, id := range []string{"principal_0000001", "principal_1", "principal_00000a", "Principal_000001", "timeoff_000001"} {
		if _, err := s.Get(testContext, id, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := s.Get(testContext, "principal_000001", false); err != nil {
		t.Fatal(err)
	}
}
