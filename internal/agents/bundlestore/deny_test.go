package bundlestore

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
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
	got, err := s.GetBundleByName("x")
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
	got, err := s.GetBundleByName("x")
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
	_, err := s.UpsertBundle(&Bundle{Name: "x", Members: []Member{{Kind: MemberSkill, ID: skillBackendPatterns, Effect: MemberDeny}}})
	if err == nil || !strings.Contains(err.Error(), "only a tool can be denied") {
		t.Fatalf("a denied skill must be refused, got: %v", err)
	}
}

func TestAnUnknownEffectIsRefused(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(&Bundle{Name: "x", Members: []Member{{Kind: MemberTool, ID: toolPlaywright, Effect: "restrict"}}})
	if err == nil || !strings.Contains(err.Error(), "include, deny") {
		t.Fatalf("an unknown effect must be refused, naming the known ones; got: %v", err)
	}
}

func TestOneBundleCannotBothIncludeAndDenyATool(t *testing.T) {
	s := openTest(t)
	_, err := s.UpsertBundle(&Bundle{Name: "x", Members: []Member{
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
		if _, err := s.UpsertBundle(&Bundle{Name: "x", DeniedReadPaths: []string{bad}}); err == nil {
			t.Fatalf("denied read path %q must be refused", bad)
		}
	}
	if _, err := s.UpsertBundle(&Bundle{Name: "x", DeniedReadPaths: []string{"/a", "/a"}}); err == nil {
		t.Fatal("a path listed twice must be refused")
	}
}

// The CHECKs in schema.sql repeat MemberEffects by hand. This keeps them in
// step: every listed effect is accepted for a tool, deny is refused for a
// skill, and a value outside the list is refused, by SQLite itself.
func TestTheSchemaChecksAgreeWithMemberEffects(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, &Bundle{Name: "x"})
	b, _ := s.GetBundleByName("x")
	for i, e := range MemberEffects {
		if _, err := s.db.Exec(`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES (?, 'tool', ?, ?)`, b.ID, i+1, string(e)); err != nil {
			t.Fatalf("the schema refuses effect %q, which MemberEffects lists: %v", e, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES (?, 'tool', 100, 'restrict')`,
		`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES (?, 'skill', 100, 'deny')`,
	} {
		if _, err := s.db.Exec(stmt, b.ID); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: want a CHECK refusal, got %v", stmt, err)
		}
	}
}

// A database written before deny existed keeps every row, and every member
// comes back as include.
func TestAPreDenyDatabaseIsMigratedWithEveryRowKept(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "bundle-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	preDeny := `
		CREATE TABLE bundles (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			extends_id INTEGER REFERENCES bundles(id) ON DELETE RESTRICT,
			match_tags TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE bundle_members (
			bundle_id INTEGER NOT NULL REFERENCES bundles(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, member_id INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '',
			condition TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (bundle_id, kind, member_id), CHECK (kind IN ('skill','tool')), CHECK (member_id > 0));
		INSERT INTO bundles (id, name, match_tags, created_at, updated_at) VALUES (1, 'react', '["react"]', 1, 1);
		INSERT INTO bundle_members (bundle_id, kind, member_id, name) VALUES (1, 'skill', 9002, 'browser-automation');
		INSERT INTO bundle_members (bundle_id, kind, member_id, name, condition) VALUES (1, 'tool', 14, 'chrome-devtools', 'perf');
	`
	if _, err := db.Exec(preDeny); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open a pre-deny database: %v", err)
	}
	defer s.Close()
	got, err := s.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 || got.DeniedReadPaths != nil {
		t.Fatalf("migrated bundle = %+v", got)
	}
	for _, m := range got.Members {
		if m.Effect != MemberInclude {
			t.Fatalf("existing member %+v must become include", m)
		}
	}
	// The added columns carry their CHECKs.
	if _, err := s.db.Exec(`INSERT INTO bundle_members (bundle_id, kind, member_id, effect) VALUES (1, 'skill', 5, 'deny')`); err == nil {
		t.Fatal("the migrated table accepted a denied skill")
	}
	s.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("a second open of the migrated database: %v", err)
	}
	again.Close()
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
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"react"}})
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
	r, err := Resolve(s, ResolveRequest{RepoTags: []string{"react"}})
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
	byTags, err := Resolve(s, ResolveRequest{RepoTags: []string{"locked"}})
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

	locked, _ := s.GetBundleByName("locked-down")
	react, _ := s.GetBundleByName("react")
	reversed, err := Resolve(s, ResolveRequest{BundleIDs: []int64{locked.ID, react.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if hasID(reversed.Tools, toolClaudeCodeWrite) {
		t.Fatalf("a bundle applied after the deny must not bring the tool back; tools = %+v", reversed.Tools)
	}

	withPerf, err := Resolve(s, ResolveRequest{RepoTags: []string{"locked"}, TaskTags: []string{"perf"}})
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
