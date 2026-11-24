package skillstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// First tests for the install path. Measured against `main` before any of this
// was written: a panic() on the first line of Install, Uninstall,
// ListInstallations, copyTree, installSkill, uninstallSkill or listInstallations
// left `go test ./...` green. Seven functions, one mechanism, executed by
// nothing.
//
// ⚠️ That measurement has to be taken on `main`, not on the branch this file
// sits on. The 222nd pass's seed-profile tests call Install and
// ListInstallations as fixtures, so on that branch a panic() in either one
// reddens the suite while both remain entirely unasserted. The reach guard
// reports "somebody calls this", which is not the same claim as "somebody
// checks this".
//
// The mechanism is worth pinning past its coverage row because of what it does
// to disk. Install begins by REMOVING its target directory and it derives that
// target from a skill's frontmatter name, which arrives from whatever upstream
// repository was ingested. Everything downstream of that RemoveAll is a write;
// nothing upstream of it is a containment check.
//
// Assertions read the effect rather than the reply wherever the two are
// separable, per the rule the 218th pass left on card e9b0b89c, and where a
// call has TWO effects — a directory on disk and a row in the database — both
// are asserted separately, per the 221st's.

// ---- fixtures ----

// ingestOneSkill ingests a single-file skill from a throwaway local source and
// returns its id. extra writes sibling payload files relative to the skill dir.
func ingestOneSkill(t *testing.T, s *Store, sourceName, dirName, skillName string, extra map[string]string) int64 {
	t.Helper()
	root := filepath.Join(t.TempDir(), "authored")
	writeSkill(t, root, dirName, skillName, "body of "+skillName)
	for rel, content := range extra {
		p := filepath.Join(root, dirName, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	src := &Source{Name: sourceName, Enabled: true, Kind: SourceKindLocal, LocalPath: root}
	if err := s.UpsertSource(src); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if _, err := s.Sync(src.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	skills, err := s.ListSkills(ListSkillsFilter{SourceID: src.ID})
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills from %s, want 1", len(skills), sourceName)
	}
	return skills[0].ID
}

// installationRow reads the stored row directly, so a test can compare what was
// written against what was reported without going back through the same code.
func installationRow(t *testing.T, s *Store, skillID int64) (target string, installedAt int64, found bool) {
	t.Helper()
	err := s.DB().QueryRow(`SELECT target_path, installed_at FROM installations WHERE skill_id=?`, skillID).
		Scan(&target, &installedAt)
	if err != nil {
		return "", 0, false
	}
	return target, installedAt, true
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---- the two effects: a directory on disk and a row in the database ----

func TestInstallWritesTheSkillToDiskAndRecordsTheRow(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Effect one: the file.
	want := filepath.Join(s.InstallDir(), "alpha")
	if inst.TargetPath != want {
		t.Fatalf("target = %s, want %s", inst.TargetPath, want)
	}
	sk, err := s.GetSkill(id)
	if err != nil {
		t.Fatalf("get skill: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(want, "SKILL.md")); got != sk.SkillMD {
		t.Fatalf("installed SKILL.md does not match the stored skill:\n on disk: %q\n in db:   %q", got, sk.SkillMD)
	}

	// Effect two: the row. A skill whose files are on disk with no row is a
	// skill Uninstall can never remove and the seed manifest never sees.
	target, at, found := installationRow(t, s, id)
	if !found {
		t.Fatal("files were installed but no installations row was written; " +
			"Uninstall reads that row, so this skill can never be removed through the API")
	}
	if target != want {
		t.Fatalf("row target_path = %s, want %s", target, want)
	}
	if at == 0 {
		t.Fatal("row installed_at is zero")
	}
}

// Install returns an Installation, and every caller — including the HTTP route —
// reports that struct rather than re-reading the row. It is built from a SECOND
// now() call, not from the value the INSERT wrote, so the two agree only because
// now() has one-second resolution. Pin them equal: a reply that disagrees with
// the row it claims to describe is a lie the coarse clock is currently hiding.
func TestTheReplyDescribesTheRowThatWasActuallyWritten(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	target, at, found := installationRow(t, s, id)
	if !found {
		t.Fatal("no installations row")
	}
	if inst.TargetPath != target {
		t.Fatalf("reply target %s != row target %s", inst.TargetPath, target)
	}
	if inst.InstalledAt != at {
		t.Fatalf("reply installed_at %d != row installed_at %d; the reply is built from a "+
			"second clock read rather than from the value the INSERT wrote", inst.InstalledAt, at)
	}
	if inst.SkillID != id {
		t.Fatalf("reply skill_id = %d, want %d", inst.SkillID, id)
	}
}

func TestInstallCopiesSiblingFilesAndKeepsTheirSubdirectories(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", map[string]string{
		"reference/notes.md": "the sibling body",
		"scripts/run.sh":     "#!/bin/sh\necho hi\n",
	})

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(inst.TargetPath, "reference", "notes.md")); got != "the sibling body" {
		t.Fatalf("sibling file = %q, want %q", got, "the sibling body")
	}
	if got := mustReadFile(t, filepath.Join(inst.TargetPath, "scripts", "run.sh")); !strings.Contains(got, "echo hi") {
		t.Fatalf("nested sibling file = %q", got)
	}
}

// A skill with no payload directory at all must still install. The stat/IsNotExist
// branch is the difference between "this skill ships only a SKILL.md" and an error.
func TestASkillWithNoSiblingFilesStillInstalls(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	if err := os.RemoveAll(s.SkillDir(id)); err != nil {
		t.Fatalf("remove payload dir: %v", err)
	}

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("a skill with no payload dir failed to install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inst.TargetPath, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md missing: %v", err)
	}
}

// Install clears its target before writing. Without that, a file dropped by an
// earlier version of the skill outlives the upgrade and the installed directory
// is a union of every version ever installed.
func TestInstallingOverAnEarlierVersionRemovesTheFilesItDropped(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	stale := filepath.Join(inst.TargetPath, "dropped-in-v1.txt")
	if err := os.WriteFile(stale, []byte("from an older version"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	if _, err := s.Install(id, false); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a file from the previous install survived the reinstall (stat err = %v); "+
			"the installed directory is a union of every version, not the current one", err)
	}
}

// ---- the conflict gate ----

func TestASecondSkillWantingTheSameDirectoryIsRefused(t *testing.T) {
	s := newTestStore(t)
	first := ingestOneSkill(t, s, "srcA", "alpha", "alpha", nil)
	if _, err := s.Install(first, false); err != nil {
		t.Fatalf("install first: %v", err)
	}
	second := ingestOneSkill(t, s, "srcB", "alpha", "alpha", nil)

	_, err := s.Install(second, false)
	if !errors.Is(err, ErrInstallConflict) {
		t.Fatalf("installing a same-named skill from another source returned %v, want ErrInstallConflict; "+
			"the target path is derived from the name alone, so two sources shipping one name collide", err)
	}
}

// The refusal is the reply. The effect is that the incumbent is still there —
// and it is the effect that matters, because everything after the conflict check
// in Install is a write and the first of those writes is os.RemoveAll(target).
func TestARefusedInstallLeavesTheIncumbentUntouched(t *testing.T) {
	s := newTestStore(t)
	first := ingestOneSkill(t, s, "srcA", "alpha", "alpha", nil)
	inst, err := s.Install(first, false)
	if err != nil {
		t.Fatalf("install first: %v", err)
	}
	before := mustReadFile(t, filepath.Join(inst.TargetPath, "SKILL.md"))

	second := ingestOneSkill(t, s, "srcB", "alpha", "alpha", nil)
	if _, err := s.Install(second, false); !errors.Is(err, ErrInstallConflict) {
		t.Fatalf("want ErrInstallConflict, got %v", err)
	}

	after, err := os.ReadFile(filepath.Join(inst.TargetPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("the refused install deleted the incumbent's files: %v", err)
	}
	if string(after) != before {
		t.Fatalf("the refused install overwrote the incumbent:\n before: %q\n after:  %q", before, after)
	}
	target, _, found := installationRow(t, s, first)
	if !found || target != inst.TargetPath {
		t.Fatalf("the refused install disturbed the incumbent's row: found=%v target=%s", found, target)
	}
	if _, _, found := installationRow(t, s, second); found {
		t.Fatal("a refused install still wrote an installations row")
	}
}

func TestForceReplacesTheIncumbentAndLeavesExactlyOneOwner(t *testing.T) {
	s := newTestStore(t)
	first := ingestOneSkill(t, s, "srcA", "alpha", "alpha", nil)
	if _, err := s.Install(first, false); err != nil {
		t.Fatalf("install first: %v", err)
	}
	second := ingestOneSkill(t, s, "srcB", "alpha", "alpha", nil)

	inst, err := s.Install(second, true)
	if err != nil {
		t.Fatalf("forced install: %v", err)
	}

	if _, _, found := installationRow(t, s, first); found {
		t.Fatal("the displaced skill still owns an installations row; target_path is UNIQUE, " +
			"so the next write to it fails on a constraint rather than on the conflict check")
	}
	target, _, found := installationRow(t, s, second)
	if !found || target != inst.TargetPath {
		t.Fatalf("the forcing skill did not take ownership: found=%v target=%s", found, target)
	}
	sk, err := s.GetSkill(second)
	if err != nil {
		t.Fatalf("get skill: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(inst.TargetPath, "SKILL.md")); got != sk.SkillMD {
		t.Fatalf("the directory still holds the displaced skill's SKILL.md: %q", got)
	}
}

// The conflict check compares ids, not paths. Reinstalling the skill that
// already owns the target must not be read as a collision with itself —
// otherwise no skill can ever be upgraded without force.
func TestReinstallingTheSkillThatAlreadyOwnsTheTargetIsNotAConflict(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	if _, err := s.Install(id, false); err != nil {
		t.Fatalf("first install: %v", err)
	}

	if _, err := s.Install(id, false); err != nil {
		t.Fatalf("reinstalling the owning skill was refused: %v; no skill can be upgraded without force", err)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM installations`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("installations rows = %d after reinstall, want 1", rows)
	}
}

// The upsert conflicts on skill_id and updates target_path from the excluded
// row. That UPDATE is the only thing that moves an installation's recorded
// location, and the install dir is configurable — so a skill reinstalled after
// the dir moves must end up owning its new directory. If the row keeps pointing
// at the old one, Uninstall deletes the abandoned copy and leaves the live one.
func TestReinstallingAfterTheInstallDirMovesUpdatesTheRecordedTarget(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	first, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	moved := filepath.Join(filepath.Dir(s.InstallDir()), "install-moved")
	s.SetInstallDir(moved)
	second, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("reinstall after move: %v", err)
	}
	if second.TargetPath == first.TargetPath {
		t.Fatalf("the target did not move: still %s", first.TargetPath)
	}

	target, _, found := installationRow(t, s, id)
	if !found {
		t.Fatal("no installations row after the reinstall")
	}
	if target != second.TargetPath {
		t.Fatalf("row still records %s, but the skill was installed to %s; "+
			"Uninstall would remove the abandoned copy and leave the live one", target, second.TargetPath)
	}
	if _, err := s.Uninstall(id); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(second.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the live directory behind (stat err = %v)", err)
	}
}

func TestInstallingASkillThatDoesNotExistWritesNothing(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Install(4242, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("install of a missing skill = %v, want ErrNotFound", err)
	}
	if entries, err := os.ReadDir(s.InstallDir()); err == nil && len(entries) != 0 {
		t.Fatalf("a failed install created %d entries under the install dir", len(entries))
	}
}

// ---- uninstall ----

func TestUninstallRemovesBothTheDirectoryAndTheRow(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	got, err := s.Uninstall(id)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got == nil || got.TargetPath != inst.TargetPath {
		t.Fatalf("uninstall reported %+v, want target %s", got, inst.TargetPath)
	}
	if _, err := os.Stat(inst.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("the directory survived uninstall (stat err = %v)", err)
	}
	if _, _, found := installationRow(t, s, id); found {
		t.Fatal("the installations row survived uninstall, so the skill reads as installed " +
			"while its files are gone")
	}
}

func TestUninstallingSomethingThatWasNeverInstalledIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	inst, err := s.Uninstall(id)
	if err != nil {
		t.Fatalf("uninstalling a skill that is not installed returned an error: %v", err)
	}
	if inst != nil {
		t.Fatalf("uninstalling a skill that is not installed reported %+v, want nil", inst)
	}
}

// ---- the listing ----

func TestListInstallationsJoinsTheSkillAndSourceNamesAndSortsBySkillName(t *testing.T) {
	s := newTestStore(t)
	zulu := ingestOneSkill(t, s, "src-zulu", "zulu", "zulu", nil)
	alpha := ingestOneSkill(t, s, "src-alpha", "alpha", "alpha", nil)
	if _, err := s.Install(zulu, false); err != nil {
		t.Fatalf("install zulu: %v", err)
	}
	if _, err := s.Install(alpha, false); err != nil {
		t.Fatalf("install alpha: %v", err)
	}

	got, err := s.ListInstallations()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d installations, want 2", len(got))
	}
	if got[0].SkillName != "alpha" || got[1].SkillName != "zulu" {
		t.Fatalf("order = %s, %s; want alpha, zulu", got[0].SkillName, got[1].SkillName)
	}
	if got[0].SourceName != "src-alpha" {
		t.Fatalf("source name = %q, want src-alpha; the join carries which source a skill came from, "+
			"and two sources can ship one name", got[0].SourceName)
	}
	if got[0].TargetPath == "" || got[0].InstalledAt == 0 {
		t.Fatalf("listing dropped a column: %+v", got[0])
	}
}

// ---- the routes ----

func installTestServer(t *testing.T, s *Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTheInstallRouteWritesTheInstallationItReports(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	resp, err := http.Post(srv.URL+"/skills/1/install", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got Installation
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	target, _, found := installationRow(t, s, id)
	if !found {
		t.Fatal("the route answered 200 and no installations row exists")
	}
	if got.TargetPath != target {
		t.Fatalf("route reported %s, row says %s", got.TargetPath, target)
	}
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatalf("the route answered 200 and nothing reached disk: %v", err)
	}
}

func TestTheInstallRouteReportsAConflictAs409(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	first := ingestOneSkill(t, s, "srcA", "alpha", "alpha", nil)
	if _, err := s.Install(first, false); err != nil {
		t.Fatalf("install first: %v", err)
	}
	ingestOneSkill(t, s, "srcB", "alpha", "alpha", nil)

	resp, err := http.Post(srv.URL+"/skills/2/install", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409; a conflict reported as 500 reads as a broken server "+
			"rather than a name collision the caller can resolve with force", resp.StatusCode)
	}
}

func TestTheInstallRouteReportsAMissingSkillAs404(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)

	resp, err := http.Post(srv.URL+"/skills/4242/install", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// force is read as the literal string "true". Any other truthy spelling is a
// silent no-force, so a caller sending force=1 gets a 409 it did not expect —
// which is the safe direction, and pinning it stops the comparison drifting to
// one where force=false turns forcing ON.
func TestTheForceParameterIsTheLiteralStringTrue(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	first := ingestOneSkill(t, s, "srcA", "alpha", "alpha", nil)
	if _, err := s.Install(first, false); err != nil {
		t.Fatalf("install first: %v", err)
	}
	ingestOneSkill(t, s, "srcB", "alpha", "alpha", nil)

	resp, err := http.Post(srv.URL+"/skills/2/install?force=1", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("force=1 was honored as forcing (status %d); only the literal \"true\" forces, "+
			"and a wider reading of it would let force=false displace an installed skill", resp.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/skills/2/install?force=true", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("force=true was not honored (status %d)", resp2.StatusCode)
	}
	if _, _, found := installationRow(t, s, first); found {
		t.Fatal("force=true answered 200 without displacing the incumbent's row")
	}
}

func TestTheUninstallRouteSaysNotInstalledRatherThanFailing(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	ingestOneSkill(t, s, "src", "alpha", "alpha", nil)

	resp, err := http.Post(srv.URL+"/skills/1/uninstall", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "not-installed" {
		t.Fatalf("body = %v, want status not-installed", body)
	}
}

func TestTheUninstallRouteRemovesTheDirectoryAndTheRow(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	resp, err := http.Post(srv.URL+"/skills/1/uninstall", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The status code is 200 whether the route removed something or found
	// nothing to remove, so the body is the only thing that distinguishes them.
	// A route that always answers not-installed tells an operator their
	// uninstall was a no-op while it deletes the directory.
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] == "not-installed" {
		t.Fatalf("the route reported not-installed for a skill it did uninstall: %v", body)
	}
	if body["target_path"] != inst.TargetPath {
		t.Fatalf("route reported target_path %v, want %s", body["target_path"], inst.TargetPath)
	}
	if _, err := os.Stat(inst.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("the route answered 200 and the directory is still there (stat err = %v)", err)
	}
	if _, _, found := installationRow(t, s, id); found {
		t.Fatal("the route answered 200 and the row is still there")
	}
}

// An empty listing must serialize as [] and not null. A client that iterates the
// response sees "no skills installed" either way in Go, but null is not an array
// in JSON and the dash page that reads this cannot map over it.
func TestTheInstallationsRouteAnswersAnEmptyArrayNotNull(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)

	resp, err := http.Get(srv.URL + "/installations")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty listing serialized as %s, want []", raw)
	}
}

func TestTheInstallationsRouteReportsWhatIsInstalled(t *testing.T) {
	s := newTestStore(t)
	srv := installTestServer(t, s)
	id := ingestOneSkill(t, s, "src", "alpha", "alpha", nil)
	if _, err := s.Install(id, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	resp, err := http.Get(srv.URL + "/installations")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got []Installation
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].SkillName != "alpha" || got[0].SourceName != "src" {
		t.Fatalf("listing = %+v, want one alpha from src", got)
	}
}

// ---- a characterisation: behavior that is filed, not fixed ----
//
// Its sibling, the path-traversal characterisation, was deleted when the
// containment guard landed; install_containment_test.go replaces it.

// ⛔ CHARACTERISATION, not an endorsement. Filed on noteboard card
// e5ed5958-ce79-4f89-b4e1-2f64967bbaeb.
//
// installations.skill_id carries ON DELETE CASCADE, so pruning a skill deletes
// the row that records where it was installed — but nothing removes the
// directory. Renaming a skill upstream prunes the old row and inserts a new one,
// and the previous install is left on disk owned by nobody: ListInstallations
// cannot see it, Uninstall cannot reach it, and the seed manifest never mentions
// it. skill-store prunes orphaned PAYLOAD directories (pruneOrphanSkillDirs) and
// has no equivalent for installed ones.
func TestARenamedSkillLeavesItsInstalledDirectoryBehindOwnedByNobody(t *testing.T) {
	s := newTestStore(t)
	root := filepath.Join(t.TempDir(), "authored")
	writeSkill(t, root, "thing", "oldname", "v1")
	src := &Source{Name: "renaming", Enabled: true, Kind: SourceKindLocal, LocalPath: root}
	if err := s.UpsertSource(src); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.Sync(src.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	before, err := s.ListSkills(ListSkillsFilter{SourceID: src.ID})
	if err != nil || len(before) != 1 {
		t.Fatalf("list: %v %d", err, len(before))
	}
	if _, err := s.Install(before[0].ID, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	writeSkill(t, root, "thing", "newname", "v2")
	if _, err := s.Sync(src.ID); err != nil {
		t.Fatalf("resync: %v", err)
	}

	if _, _, found := installationRow(t, s, before[0].ID); found {
		t.Fatal("the pruned skill still has an installations row — the CASCADE stopped firing, " +
			"which changes what this characterisation is about")
	}
	if _, err := os.Stat(filepath.Join(s.InstallDir(), "oldname")); err != nil {
		t.Fatalf("the orphaned directory is gone (stat err = %v) — something now prunes installed "+
			"directories, so delete this characterisation and close its card", err)
	}
	insts, err := s.ListInstallations()
	if err != nil {
		t.Fatalf("list installations: %v", err)
	}
	for _, inst := range insts {
		if strings.HasSuffix(inst.TargetPath, string(os.PathSeparator)+"oldname") {
			t.Fatalf("the orphan is visible after all: %+v", inst)
		}
	}
}
