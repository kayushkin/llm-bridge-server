package bundlestore

import (
	"errors"
	"strings"
	"testing"
)

func mustUpsert(t *testing.T, s *Store, b *Bundle) {
	t.Helper()
	if _, err := s.UpsertBundle(b); err != nil {
		t.Fatal(err)
	}
}

func hasName(refs []MemberRef, name string) bool {
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func hasID(refs []MemberRef, id int64) bool {
	for _, r := range refs {
		if r.ID == id {
			return true
		}
	}
	return false
}

func hasBundle(refs []BundleRef, name string) bool {
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// Member ids below are the ones a live skill-store and tool-store
// assigned, so a reader can check the fixture against the live registries.
const (
	skillBrowserAutomation int64 = 9001
	skillBackendPatterns   int64 = 320
	toolPlaywright         int64 = 13
	toolChromeDevTools     int64 = 14
)

// seed installs a base + react + go-service set mirroring the cmd seeds.
func seed(t *testing.T, s *Store) {
	t.Helper()
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	mustUpsert(t, s, &Bundle{
		Name: "react", Extends: "base", MatchTags: []string{"react", "frontend"}, Enabled: true,
		Members: []Member{
			{Kind: MemberSkill, ID: skillBrowserAutomation, Name: "browser-automation"},
			{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"},
			{Kind: MemberTool, ID: toolChromeDevTools, Name: "chrome-devtools", Condition: "perf"},
		},
	})
	mustUpsert(t, s, &Bundle{
		Name: "go-service", Extends: "base", MatchTags: []string{"go"}, Enabled: true,
		Members: []Member{{Kind: MemberSkill, ID: skillBackendPatterns, Name: "cc-skill-backend-patterns"}},
	})
}

func TestResolveSelectsByTagAndInherits(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"go", "react"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasBundle(r.Bundles, "base") || !hasBundle(r.Bundles, "react") || !hasBundle(r.Bundles, "go-service") {
		t.Fatalf("expected base+react+go-service, got %+v", r.Bundles)
	}
	for _, b := range r.Bundles {
		if b.ID == 0 {
			t.Fatalf("contributing bundle reported with no id: %+v", b)
		}
	}
	if !hasID(r.Skills, skillBrowserAutomation) || !hasID(r.Skills, skillBackendPatterns) {
		t.Fatalf("skills: %+v", r.Skills)
	}
	if !hasID(r.Tools, toolPlaywright) {
		t.Fatalf("tools: %+v", r.Tools)
	}
	if hasID(r.Tools, toolChromeDevTools) {
		t.Fatalf("chrome-devtools must be gated off without task=perf: %+v", r.Tools)
	}
}

// The response has to carry ids: skill-store installs only by id, so a name is
// not enough for a consumer to act on.
func TestResolveReturnsOwningStoreIDs(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Skills) == 0 || len(r.Tools) == 0 {
		t.Fatalf("nothing resolved: %+v", r)
	}
	for _, m := range append(append([]MemberRef{}, r.Skills...), r.Tools...) {
		if m.ID <= 0 {
			t.Fatalf("resolved member with no id — a consumer cannot install it: %+v", m)
		}
		if m.Name == "" {
			t.Fatalf("resolved member with no display name: %+v", m)
		}
	}
	if !hasName(r.Skills, "browser-automation") {
		t.Fatalf("display name dropped: %+v", r.Skills)
	}
}

// Deduplication is on the id. Two bundles contributing skill 9001 give one
// entry; two different skills that share a name give two.
func TestResolveDedupesOnIDNotName(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "a", MatchTags: []string{"x"}, Enabled: true, Members: []Member{
		{Kind: MemberSkill, ID: 4, Name: "claude-api"}, // anthropics
	}})
	mustUpsert(t, s, &Bundle{Name: "b", MatchTags: []string{"x"}, Enabled: true, Members: []Member{
		{Kind: MemberSkill, ID: 4, Name: "claude-api"},   // the same skill again
		{Kind: MemberSkill, ID: 341, Name: "claude-api"}, // antigravity's, a different skill
	}})
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Skills) != 2 {
		t.Fatalf("want 2 skills (id 4 once, id 341 once), got %+v", r.Skills)
	}
	if !hasID(r.Skills, 4) || !hasID(r.Skills, 341) {
		t.Fatalf("both distinct skills must survive: %+v", r.Skills)
	}
}

func TestResolveConditionGateFires(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"react"}, TaskTags: []string{"perf"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(r.Tools, toolChromeDevTools) {
		t.Fatalf("chrome-devtools must appear with task=perf: %+v", r.Tools)
	}
}

func TestResolveBaseAlwaysApplies(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"haskell-unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasBundle(r.Bundles, "base") {
		t.Fatalf("base should always apply, got %+v", r.Bundles)
	}
	if len(r.Skills) != 0 || len(r.Tools) != 0 {
		t.Fatalf("unknown repo should resolve only the empty base: skills=%+v tools=%+v", r.Skills, r.Tools)
	}
}

// expandChain's cycle guard, driven against a cycle the API would have refused.
// The write-time check (TestCycleRefusedAtTheWrite) is the one that fires in
// practice; this one covers a database written by hand or by an older binary,
// and proves the walk still reports rather than looping or truncating.
func TestResolveCycleDetected(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "b", Enabled: true})
	mustUpsert(t, s, &Bundle{Name: "a", Extends: "b", MatchTags: []string{"x"}, Enabled: true})
	a, err := s.GetBundleByName("a")
	if err != nil {
		t.Fatal(err)
	}
	writeBehindTheAPI(t, s, `UPDATE bundles SET extends_id=? WHERE name='b'`, a.ID)

	_, err = Resolve(s, ResolveRequest{RepoTags: []string{"x"}})
	if err == nil {
		t.Fatal("expected inheritance cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("the error must say what is wrong, got: %v", err)
	}
}

// The same for a parent that is gone. ON DELETE RESTRICT means the API cannot
// produce this state either, so it is written behind the API's back.
func TestResolveMissingParentErrors(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "a", MatchTags: []string{"x"}, Enabled: true})
	writeBehindTheAPI(t, s, `UPDATE bundles SET extends_id=999999 WHERE name='a'`)

	_, err := Resolve(s, ResolveRequest{RepoTags: []string{"x"}})
	if err == nil {
		t.Fatal("expected missing-parent error")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Fatalf("the error must name the parent it could not find, got: %v", err)
	}
}

// The defect this change exists to stop. Renaming a parent used to strand every
// child: `extends` held the old name, and the next resolve failed somewhere
// else entirely. The link is an id now, so the rename passes straight through
// and the display name follows it.
func TestRenamingAParentDoesNotStrandItsChildren(t *testing.T) {
	s := openTest(t)
	seed(t, s)

	// The rename itself: there is no rename route yet (POST /bundles upserts by
	// name, so a "rename" through the API is a create plus an orphan), which is
	// exactly why the stored link must not be the name.
	writeBehindTheAPI(t, s, `UPDATE bundles SET name='base-renamed' WHERE name='base'`)

	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatalf("renaming a parent broke resolution for its children: %v", err)
	}
	if !hasBundle(r.Bundles, "base-renamed") {
		t.Fatalf("the renamed parent must still contribute: %+v", r.Bundles)
	}
	child, err := s.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if child.Extends != "base-renamed" {
		t.Fatalf("the display name is read from the parent row, so it must follow the rename, got %q", child.Extends)
	}
}

// A session started with an explicit bundle asks by id, and gets exactly that
// bundle's chain plus base — never a tag match it did not ask for.
func TestResolveByExplicitBundleIDs(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	goService, err := s.GetBundleByName("go-service")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(s, ResolveRequest{BundleIDs: []int64{goService.ID}, RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasBundle(r.Bundles, "base") || !hasBundle(r.Bundles, "go-service") {
		t.Fatalf("expected base+go-service, got %+v", r.Bundles)
	}
	if hasBundle(r.Bundles, "react") {
		t.Fatalf("react was not asked for and must not be applied by tag when ids are explicit: %+v", r.Bundles)
	}
	if !hasID(r.Skills, skillBackendPatterns) {
		t.Fatalf("skills: %+v", r.Skills)
	}
}

func TestResolveByUnknownOrDisabledBundleIDIsLoud(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	if _, err := Resolve(s, ResolveRequest{BundleIDs: []int64{999999}}); !errors.Is(err, ErrBundleNotApplicable) {
		t.Fatalf("unknown id: want ErrBundleNotApplicable, got %v", err)
	}
	goService, err := s.GetBundleByName("go-service")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(goService.ID, false); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(s, ResolveRequest{BundleIDs: []int64{goService.ID}})
	if !errors.Is(err, ErrBundleNotApplicable) || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled id: want ErrBundleNotApplicable naming disabled, got %v", err)
	}
}
