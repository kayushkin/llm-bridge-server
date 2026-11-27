package bundles

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Tool ids for harness built-ins are made up here; tool-store assigns the
// real ones.
const (
	toolClaudeCodeWrite int64 = 901
	toolClaudeCodeEdit  int64 = 902
)

func TestAMemberWrittenWithoutAnEffectIsStoredAsInclude(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "x", Enabled: true, Members: []Member{{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"}}})
	got, err := s.GetBundleByName(testContext, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Members[0].Effect != MemberInclude {
		t.Fatalf("effect = %q, want include", got.Members[0].Effect)
	}
}

func TestADeniedToolRoundTrips(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "x", Enabled: true,
		DeniedReadPaths: []string{"~/.config/**", "/proc/**"},
		Members:         []Member{{Kind: MemberTool, ID: toolClaudeCodeWrite, Name: "claude_code.Write", Effect: MemberDeny}}})
	got, err := s.GetBundleByName(testContext, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Members[0].Effect != MemberDeny {
		t.Fatalf("effect = %q, want deny", got.Members[0].Effect)
	}
	if !reflect.DeepEqual(got.DeniedReadPaths, []string{"~/.config/**", "/proc/**"}) {
		t.Fatalf("denied_read_paths = %v", got.DeniedReadPaths)
	}
}

func TestOnlyAToolCanBeDenied(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(testContext, &Bundle{Name: "x", Members: []Member{{Kind: MemberSkill, ID: skillBackendPatterns, Effect: MemberDeny}}})
	if err == nil || !strings.Contains(err.Error(), "only a tool can be denied") {
		t.Fatalf("a denied skill must be refused, got: %v", err)
	}
}

func TestAnUnknownEffectIsRefused(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(testContext, &Bundle{Name: "x", Members: []Member{{Kind: MemberTool, ID: toolPlaywright, Effect: "restrict"}}})
	if err == nil || !strings.Contains(err.Error(), "include, deny") {
		t.Fatalf("an unknown effect must be refused, naming the known ones; got: %v", err)
	}
}

func TestOneBundleCannotBothIncludeAndDenyATool(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(testContext, &Bundle{Name: "x", Members: []Member{
		{Kind: MemberTool, ID: toolPlaywright},
		{Kind: MemberTool, ID: toolPlaywright, Effect: MemberDeny},
	}})
	if err == nil || !strings.Contains(err.Error(), "both included and denied") {
		t.Fatalf("got: %v", err)
	}
}

func TestADeniedReadPathMustBeAbsoluteOrUnderHome(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{"", ".env", "config/**", "~user/x", "~"} {
		if _, err := s.UpsertBundle(testContext, &Bundle{Name: "x", DeniedReadPaths: []string{bad}}); err == nil {
			t.Fatalf("denied read path %q must be refused", bad)
		}
	}
	if _, err := s.UpsertBundle(testContext, &Bundle{Name: "x", DeniedReadPaths: []string{"/a", "/a"}}); err == nil {
		t.Fatal("a path listed twice must be refused")
	}
}

// The CHECKs in the first migration repeat MemberEffects by hand. This keeps them in
// step: every listed effect is accepted for a tool, deny is refused for a
// skill, and a value outside the list is refused, by Postgres itself (23514).
func TestTheSchemaChecksAgreeWithMemberEffects(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "x"})
	b, _ := s.GetBundleByName(testContext, "x")
	for i, e := range MemberEffects {
		if _, err := s.db.Exec(`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES ($1, 'tool', $2, $3)`, b.ID, i+1, string(e)); err != nil {
			t.Fatalf("the schema refuses effect %q, which MemberEffects lists: %v", e, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES ($1, 'tool', 100, 'restrict')`,
		`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES ($1, 'skill', 100, 'deny')`,
	} {
		if _, err := s.db.Exec(stmt, b.ID); err == nil || !hasPostgresCode(err, "23514") {
			t.Fatalf("%s: want a CHECK refusal, got %v", stmt, err)
		}
	}
}

// denySeed: base denies Edit and ~/.secrets/**; react includes playwright and
// Write; locked-down extends react, denies Write and playwright, and denies
// /proc/** and ~/.secrets/** again.
func denySeed(t *testing.T, s *Store) {
	t.Helper()
	mustUpsert(t, s, &Bundle{Name: "base", Enabled: true,
		DeniedReadPaths: []string{"~/.secrets/**"},
		Members:         []Member{{Kind: MemberTool, ID: toolClaudeCodeEdit, Name: "claude_code.Edit", Effect: MemberDeny}}})
	mustUpsert(t, s, &Bundle{Name: "react", Extends: "base", MatchTags: []string{"react"}, Enabled: true,
		Members: []Member{
			{Kind: MemberTool, ID: toolPlaywright, Name: "playwright"},
			{Kind: MemberTool, ID: toolClaudeCodeWrite, Name: "claude_code.Write"},
		}})
	mustUpsert(t, s, &Bundle{Name: "locked-down", Extends: "react", MatchTags: []string{"locked"}, Enabled: true,
		DeniedReadPaths: []string{"/proc/**", "~/.secrets/**"},
		Members: []Member{
			{Kind: MemberTool, ID: toolClaudeCodeWrite, Name: "claude_code.Write", Effect: MemberDeny},
			{Kind: MemberTool, ID: toolChromeDevTools, Name: "chrome-devtools", Effect: MemberDeny, Condition: "perf"},
		}})
}

func TestResolveWithNothingDeniedReturnsEmptyLists(t *testing.T) {
	s := openTest(t)
	seed(t, s)
	r, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(r)
	for _, want := range []string{`"denied_tools":[]`, `"denied_read_paths":[]`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("resolve must always carry %s; got %s", want, body)
		}
	}
}

func TestResolveCollectsDeniesFromTheWholeChain(t *testing.T) {
	s := openTest(t)
	denySeed(t, s)
	r, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"react"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(r.Tools, toolClaudeCodeWrite) || !hasID(r.Tools, toolPlaywright) {
		t.Fatalf("react alone includes Write and playwright; tools = %+v", r.Tools)
	}
	if !reflect.DeepEqual(r.DeniedTools, []MemberRef{{ID: toolClaudeCodeEdit, Name: "claude_code.Edit"}}) {
		t.Fatalf("base's deny must reach a react session; denied_tools = %+v", r.DeniedTools)
	}
	if !reflect.DeepEqual(r.DeniedReadPaths, []string{"~/.secrets/**"}) {
		t.Fatalf("denied_read_paths = %v", r.DeniedReadPaths)
	}
}

// A deny anywhere wins: react includes Write, locked-down denies it, and the
// order bundles are applied in does not matter.
func TestADenyAnywhereWinsOverAnInclude(t *testing.T) {
	s := openTest(t)
	denySeed(t, s)
	byTags, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"locked"}})
	if err != nil {
		t.Fatal(err)
	}
	if hasID(byTags.Tools, toolClaudeCodeWrite) {
		t.Fatalf("a denied tool must not be in tools; tools = %+v", byTags.Tools)
	}
	if !hasID(byTags.Tools, toolPlaywright) {
		t.Fatalf("an include nobody denies must stay; tools = %+v", byTags.Tools)
	}
	if !hasID(byTags.DeniedTools, toolClaudeCodeWrite) || !hasID(byTags.DeniedTools, toolClaudeCodeEdit) {
		t.Fatalf("denied_tools = %+v", byTags.DeniedTools)
	}
	if hasID(byTags.DeniedTools, toolChromeDevTools) {
		t.Fatal("a conditional deny must not fire without its task tag")
	}
	if !reflect.DeepEqual(byTags.DeniedReadPaths, []string{"~/.secrets/**", "/proc/**"}) {
		t.Fatalf("denied_read_paths must be the union in first-seen order, without repeats; got %v", byTags.DeniedReadPaths)
	}

	locked, _ := s.GetBundleByName(testContext, "locked-down")
	react, _ := s.GetBundleByName(testContext, "react")
	reversed, err := Resolve(testContext, s, ResolveRequest{BundleIDs: []int64{locked.ID, react.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if hasID(reversed.Tools, toolClaudeCodeWrite) {
		t.Fatalf("a bundle applied after the deny must not bring the tool back; tools = %+v", reversed.Tools)
	}

	withPerf, err := Resolve(testContext, s, ResolveRequest{RepoTags: []string{"locked"}, TaskTags: []string{"perf"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(withPerf.DeniedTools, toolChromeDevTools) {
		t.Fatalf("the conditional deny must fire with its task tag; denied_tools = %+v", withPerf.DeniedTools)
	}
}

func TestTheMemberEffectsRouteServesTheList(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := srv.Client().Get(srv.URL + "/member-effects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `["include","deny"]` {
		t.Fatalf("GET /member-effects = %d %s", resp.StatusCode, body)
	}
}

func TestTheBundlesRouteRefusesADeniedSkillWith400(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, body := range []string{
		`{"name":"x","members":[{"kind":"skill","id":5,"effect":"deny"}]}`,
		`{"name":"x","denied_read_paths":["relative/**"]}`,
	} {
		resp, err := srv.Client().Post(srv.URL+"/bundles", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /bundles %s = %d, want 400", body, resp.StatusCode)
		}
	}
}
