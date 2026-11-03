package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bundlestore "github.com/kayushkin/bundle-store"
)

// The ids below are made up. They model no real skill-store or tool-store.

func checkFixtureBundles() []bundlestore.Bundle {
	return []bundlestore.Bundle{
		{
			ID:   1,
			Name: "frontend",
			Members: []bundlestore.Member{
				{Kind: bundlestore.MemberSkill, ID: 501, Name: "web-testing", Effect: bundlestore.MemberInclude},
				{Kind: bundlestore.MemberTool, ID: 61, Name: "browser", Effect: bundlestore.MemberInclude},
			},
		},
		{
			ID:      2,
			Name:    "read-only",
			Enabled: false,
			Members: []bundlestore.Member{
				{Kind: bundlestore.MemberTool, ID: 62, Name: "write-file", Effect: bundlestore.MemberDeny},
			},
		},
	}
}

func TestCheckPassesWhenEveryStoredMemberIsCarried(t *testing.T) {
	index := newMemberIndex(
		[]storeEntry{{ID: 501, Name: "web-testing"}},
		[]storeEntry{{ID: 61, Name: "browser"}, {ID: 62, Name: "write-file"}},
	)
	report := checkStoredMembers(index, checkFixtureBundles())
	if len(report.Missing) != 0 || len(report.Relabeled) != 0 {
		t.Fatalf("every id carried under its stored label must report nothing, got %+v", report)
	}
}

// A missing id fails the check, and the line names the bundle, kind, id and
// stored label, so the operator can act without reading the database. Every
// missing member is listed, including a deny in a disabled bundle: a deny that
// points at nothing takes nothing away, and a disabled bundle can come back.
func TestCheckFailsOnEveryMemberIDTheOwningStoreNoLongerCarries(t *testing.T) {
	index := newMemberIndex(
		[]storeEntry{{ID: 501, Name: "web-testing"}},
		nil,
	)
	report := checkStoredMembers(index, checkFixtureBundles())
	if len(report.Missing) != 2 {
		t.Fatalf("want both missing tool ids reported, got %d: %v", len(report.Missing), report.Missing)
	}
	joined := strings.Join(report.Missing, "\n")
	for _, want := range []string{
		"bundle frontend (id 1)", "tool id 61", `"browser"`, "tool-store",
		"bundle read-only (id 2)", "deny tool id 62", `"write-file"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the report must name %q; got:\n%s", want, joined)
		}
	}
	if len(report.Relabeled) != 0 {
		t.Fatalf("a missing id is not a relabel: %v", report.Relabeled)
	}
}

// The id is what a bundle means. An id the store still carries under a new
// name is reported, but it does not fail the check.
func TestCheckReportsARenamedMemberWithoutFailing(t *testing.T) {
	index := newMemberIndex(
		[]storeEntry{{ID: 501, Name: "web-testing-v2"}},
		[]storeEntry{{ID: 61, Name: "browser"}, {ID: 62, Name: "write-file"}},
	)
	report := checkStoredMembers(index, checkFixtureBundles())
	if len(report.Missing) != 0 {
		t.Fatalf("a renamed member must not fail the check: %v", report.Missing)
	}
	if len(report.Relabeled) != 1 {
		t.Fatalf("want one relabel line, got %d: %v", len(report.Relabeled), report.Relabeled)
	}
	for _, want := range []string{"bundle frontend", "skill id 501", `"web-testing"`, `"web-testing-v2"`, "skill-store"} {
		if !strings.Contains(report.Relabeled[0], want) {
			t.Fatalf("the relabel line must name %q; got: %s", want, report.Relabeled[0])
		}
	}
}

// A skill and a tool can share an id number: they come from different stores.
// The index must not let one answer for the other.
func TestCheckKeepsTheTwoStoresApart(t *testing.T) {
	index := newMemberIndex(
		[]storeEntry{{ID: 61, Name: "browser"}},
		nil,
	)
	bundles := []bundlestore.Bundle{{
		Name:    "crossed",
		Members: []bundlestore.Member{{Kind: bundlestore.MemberTool, ID: 61, Name: "browser"}},
	}}
	report := checkStoredMembers(index, bundles)
	if len(report.Missing) != 1 {
		t.Fatalf("a tool id carried only by skill-store must be missing, got %+v", report)
	}
}

func TestFetchEntriesReportsAnUnreachableStoreLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := fetchEntries(context.Background(), srv.URL+"/skills"); err == nil {
		t.Fatal("a non-200 from an owning store must be an error, not an empty index")
	}
}

// The check reads the bundles the store holds, so a store that holds none
// passes with nothing to say.
func TestCheckOfAnEmptyStorePasses(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bundles, err := store.ListBundles(bundlestore.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 0 {
		t.Fatalf("a fresh store must hold no bundles, got %d", len(bundles))
	}
	report := checkStoredMembers(newMemberIndex(nil, nil), bundles)
	if len(report.Missing) != 0 || len(report.Relabeled) != 0 {
		t.Fatalf("an empty store must report nothing, got %+v", report)
	}
}
