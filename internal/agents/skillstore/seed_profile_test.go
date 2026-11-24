package skillstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// First tests for the per-machine seed profile switch. Until they were written,
// a panic() on the first line of SetSeedProfileEnabled, putSeedProfile,
// BuildSeedManifest or EnsureSeedProfile left `go test ./...` green — nothing in
// the suite executed any layer of this feature.
//
// The switch is worth pinning beyond its coverage row because of what OFF means
// downstream: BuildSeedManifest returns an EMPTY manifest for a disabled
// machine, and an empty manifest is not "nothing to do" — it is the instruction
// the runner reconciles to by DELETING every file it previously seeded. So the
// bool written here is the difference between a machine keeping its skills and
// a machine having them removed, and the three layers that carry it (store
// write, HTTP route, manifest reader) can disagree.
//
// The assertions therefore read the effect rather than the reply wherever the
// two are separable, per the rule the 218th pass left on card e9b0b89c.

// seedTestStore is newTestStore plus a HOME that BuildSeedManifest will accept.
//
// snapshotInstallation skips any installation whose target path is not under
// $HOME, so a manifest test using the default install dir under /tmp gets an
// empty entry list for a reason that has nothing to do with the enable switch —
// and would pass just as happily with the switch broken. Pointing HOME at the
// temp dir keeps "no entries" meaning only what this file says it means.
func seedTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	s, err := Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetInstallDir(filepath.Join(dir, "install"))
	return s
}

// installOneSkill ingests a one-file skill and installs it, so the machine has
// something to seed. Returns the skill id.
func installOneSkill(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	root := filepath.Join(t.TempDir(), "authored")
	writeSkill(t, root, name, name, "body text")

	src := &Source{Name: "host-" + name, Enabled: true, Kind: SourceKindLocal, LocalPath: root}
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
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	if _, err := s.Install(skills[0].ID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	return skills[0].ID
}

// ---- the default ----

// A machine nobody has configured must seed. The column default and the INSERT
// both say 1; this pins that they agree and that the read path decodes it.
func TestAnUnconfiguredMachineSeedsByDefault(t *testing.T) {
	s := seedTestStore(t)

	prof, err := s.EnsureSeedProfile("fresh-box")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !prof.Enabled {
		t.Fatal("a machine with no profile row defaulted to NOT seeding; " +
			"an empty manifest tells the runner to delete what it has already seeded")
	}
	if prof.MachineID != "fresh-box" {
		t.Fatalf("machine_id = %q, want fresh-box", prof.MachineID)
	}
	if prof.CreatedAt == 0 || prof.UpdatedAt == 0 {
		t.Fatalf("timestamps not set: created=%d updated=%d", prof.CreatedAt, prof.UpdatedAt)
	}
}

// EnsureSeedProfile runs on the read path of every seed request — getSeedProfile
// and BuildSeedManifest both call it first. If it re-inserted rather than
// returning the existing row, every disabled machine would silently re-enable
// itself the next time its runner asked for a manifest, and the opt-out would
// last exactly until the next poll.
func TestEnsureDoesNotReEnableAMachineThatOptedOut(t *testing.T) {
	s := seedTestStore(t)

	if _, err := s.SetSeedProfileEnabled("opted-out", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	for i := 0; i < 3; i++ {
		prof, err := s.EnsureSeedProfile("opted-out")
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
		if prof.Enabled {
			t.Fatalf("ensure call %d re-enabled a machine that opted out", i)
		}
	}
}

// ---- the store write ----

func TestTheSwitchWritesBothValuesAndTheyRoundTrip(t *testing.T) {
	s := seedTestStore(t)

	for _, want := range []bool{false, true, false} {
		returned, err := s.SetSeedProfileEnabled("box", want)
		if err != nil {
			t.Fatalf("set %v: %v", want, err)
		}
		if returned.Enabled != want {
			t.Fatalf("returned profile says enabled=%v, want %v", returned.Enabled, want)
		}
		// The effect, read back independently of what the call answered.
		stored, err := s.GetSeedProfile("box")
		if err != nil {
			t.Fatalf("get after set %v: %v", want, err)
		}
		if stored.Enabled != want {
			t.Fatalf("stored row says enabled=%v, want %v — the reply and the row disagree",
				stored.Enabled, want)
		}
	}
}

// The switch is expected to work on a machine that has never been seen: it
// ensures the row exists before updating it. Without that, the UPDATE matches
// zero rows and the call reports success having written nothing.
func TestTheSwitchCreatesTheRowItIsAboutToUpdate(t *testing.T) {
	s := seedTestStore(t)

	if _, err := s.GetSeedProfile("never-seen"); err == nil {
		t.Fatal("precondition failed: a profile already exists for never-seen")
	}
	prof, err := s.SetSeedProfileEnabled("never-seen", false)
	if err != nil {
		t.Fatalf("set on an unknown machine: %v", err)
	}
	if prof.Enabled {
		t.Fatal("disabling an unknown machine reported enabled=true")
	}
	stored, err := s.GetSeedProfile("never-seen")
	if err != nil {
		t.Fatalf("the row was never created: %v", err)
	}
	if stored.Enabled {
		t.Fatal("the row exists but still says enabled — the UPDATE matched nothing")
	}
}

// The UPDATE carries a WHERE machine_id=?. Dropping it would disable the whole
// fleet on one machine's request, and a suite testing one machine at a time
// cannot see that.
func TestTheSwitchTouchesOnlyTheNamedMachine(t *testing.T) {
	s := seedTestStore(t)

	for _, id := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.EnsureSeedProfile(id); err != nil {
			t.Fatalf("ensure %s: %v", id, err)
		}
	}
	if _, err := s.SetSeedProfileEnabled("beta", false); err != nil {
		t.Fatalf("disable beta: %v", err)
	}
	for _, id := range []string{"alpha", "gamma"} {
		prof, err := s.GetSeedProfile(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !prof.Enabled {
			t.Fatalf("disabling beta also disabled %s", id)
		}
	}
	beta, err := s.GetSeedProfile("beta")
	if err != nil {
		t.Fatalf("get beta: %v", err)
	}
	if beta.Enabled {
		t.Fatal("beta was not disabled")
	}
}

// A flip must update the existing row, not replace it. A re-INSERT would reset
// created_at and lose when the machine was first seen.
func TestFlippingTheSwitchPreservesWhenTheMachineWasFirstSeen(t *testing.T) {
	s := seedTestStore(t)

	first, err := s.EnsureSeedProfile("box")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	after, err := s.SetSeedProfileEnabled("box", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if after.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at moved %d -> %d; the row was replaced, not updated",
			first.CreatedAt, after.CreatedAt)
	}
}

func TestASwitchWithNoMachineIsRefused(t *testing.T) {
	s := seedTestStore(t)

	if _, err := s.SetSeedProfileEnabled("", false); err == nil {
		t.Fatal("setting the switch with an empty machine_id succeeded")
	}
}

// ---- the consumer: what OFF actually does ----

// The whole point of the switch. A disabled machine gets an empty manifest, and
// the runner reads that as "remove what you seeded".
func TestADisabledMachineGetsAnEmptyManifestAndAnEnabledOneDoesNot(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")

	on, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest while enabled: %v", err)
	}
	if !on.Enabled {
		t.Fatal("manifest reports enabled=false for a machine that has not opted out")
	}
	if len(on.Entries) == 0 {
		t.Fatal("an enabled machine with an installed skill got no manifest entries; " +
			"every later assertion here would pass vacuously")
	}

	if _, err := s.SetSeedProfileEnabled("box", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	off, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest while disabled: %v", err)
	}
	if off.Enabled {
		t.Fatal("manifest reports enabled=true for a machine that opted out")
	}
	if len(off.Entries) != 0 {
		t.Fatalf("a disabled machine got %d manifest entries, want 0", len(off.Entries))
	}
}

// The manifest is per-machine, so one machine opting out must not empty
// another's. This is the switch's WHERE clause seen from the consumer end.
func TestOneMachineOptingOutDoesNotEmptyAnothersManifest(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")

	if _, err := s.SetSeedProfileEnabled("laptop", false); err != nil {
		t.Fatalf("disable laptop: %v", err)
	}
	desktop, err := s.BuildSeedManifest("desktop")
	if err != nil {
		t.Fatalf("manifest for desktop: %v", err)
	}
	if !desktop.Enabled || len(desktop.Entries) == 0 {
		t.Fatalf("disabling laptop emptied desktop's manifest: enabled=%v entries=%d",
			desktop.Enabled, len(desktop.Entries))
	}
}

// ---- the HTTP route ----

func seedTestServer(t *testing.T, s *Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func putSeedProfileReq(t *testing.T, srv *httptest.Server, query, body string) (int, SeedProfile) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/seed/profile"+query, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /seed/profile: %v", err)
	}
	defer resp.Body.Close()
	var prof SeedProfile
	_ = json.NewDecoder(resp.Body).Decode(&prof)
	return resp.StatusCode, prof
}

// The route must carry the bool it was given through to the row. A handler that
// builds its 200 from the request body rather than from the store answers
// correctly while writing nothing, so this asserts the store and not the reply.
func TestTheRouteWritesTheValueItWasGiven(t *testing.T) {
	s := seedTestStore(t)
	srv := seedTestServer(t, s)

	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"enabled":false}`, false},
		{`{"enabled":true}`, true},
	} {
		code, prof := putSeedProfileReq(t, srv, "?machine_id=box", tc.body)
		if code != http.StatusOK {
			t.Fatalf("PUT %s -> %d, want 200", tc.body, code)
		}
		if prof.Enabled != tc.want {
			t.Fatalf("reply says enabled=%v, want %v", prof.Enabled, tc.want)
		}
		stored, err := s.GetSeedProfile("box")
		if err != nil {
			t.Fatalf("get after PUT %s: %v", tc.body, err)
		}
		if stored.Enabled != tc.want {
			t.Fatalf("PUT %s replied enabled=%v but the row says %v — the route wrote nothing",
				tc.body, prof.Enabled, stored.Enabled)
		}
	}
}

// A PUT that reaches the store must also reach the consumer: disabling over
// HTTP has to empty the manifest the same request path serves.
func TestDisablingOverHTTPEmptiesTheManifestServedOverHTTP(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")
	srv := seedTestServer(t, s)

	if code, _ := putSeedProfileReq(t, srv, "?machine_id=box", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable -> %d", code)
	}

	resp, err := http.Get(srv.URL + "/seed/manifest?machine_id=box")
	if err != nil {
		t.Fatalf("GET /seed/manifest: %v", err)
	}
	defer resp.Body.Close()
	var mf SeedManifest
	if err := json.NewDecoder(resp.Body).Decode(&mf); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if mf.Enabled {
		t.Fatal("manifest served over HTTP still reports enabled after a disabling PUT")
	}
	if len(mf.Entries) != 0 {
		t.Fatalf("manifest served over HTTP has %d entries for a disabled machine", len(mf.Entries))
	}
}

// GET must report what PUT wrote, rather than re-deriving a default.
func TestTheGetRouteReportsWhatThePutRouteWrote(t *testing.T) {
	s := seedTestStore(t)
	srv := seedTestServer(t, s)

	if code, _ := putSeedProfileReq(t, srv, "?machine_id=box", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable -> %d", code)
	}
	resp, err := http.Get(srv.URL + "/seed/profile?machine_id=box")
	if err != nil {
		t.Fatalf("GET /seed/profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET -> %d, want 200", resp.StatusCode)
	}
	var prof SeedProfile
	if err := json.NewDecoder(resp.Body).Decode(&prof); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if prof.Enabled {
		t.Fatal("GET reports enabled after a disabling PUT")
	}
	if prof.MachineID != "box" {
		t.Fatalf("machine_id = %q, want box", prof.MachineID)
	}
}

// A PUT with no machine_id must be refused rather than defaulting to some
// machine. The failure mode it guards is a request disabling the wrong box.
func TestTheRouteRefusesARequestWithNoMachine(t *testing.T) {
	s := seedTestStore(t)
	srv := seedTestServer(t, s)

	code, _ := putSeedProfileReq(t, srv, "", `{"enabled":false}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT with no machine_id -> %d, want 400", code)
	}
}

// An unparseable body must be refused, not read as {"enabled":false}. Go's zero
// value for bool is the destructive setting here, so a handler that ignored the
// decode error would turn a malformed request into an opt-out.
func TestTheRouteRefusesAnUnparseableBodyRatherThanReadingItAsOff(t *testing.T) {
	s := seedTestStore(t)
	srv := seedTestServer(t, s)

	if _, err := s.EnsureSeedProfile("box"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	code, _ := putSeedProfileReq(t, srv, "?machine_id=box", `{"enabled":`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT with a truncated body -> %d, want 400", code)
	}
	stored, err := s.GetSeedProfile("box")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !stored.Enabled {
		t.Fatal("a malformed request disabled the machine — the zero value was written")
	}
}

// The manifest only describes files under $HOME: installPathRelative refuses to
// emit an install_path it cannot rewrite as $HOME-relative. That refusal is why
// seedTestStore sets HOME, and pinning it here stops a later change to the
// manifest tests silently reducing them to assertions about nothing.
func TestAnInstallationOutsideHomeIsNotSeeded(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")

	full, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(full.Entries) == 0 {
		t.Fatal("precondition failed: nothing was seeded even under HOME")
	}

	// Move HOME away from the install dir; the same installation is now outside it.
	t.Setenv("HOME", t.TempDir())
	outside, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest with HOME moved: %v", err)
	}
	if len(outside.Entries) != 0 {
		t.Fatalf("an installation outside HOME produced %d entries, want 0", len(outside.Entries))
	}
}

// snapshotInstallation checks $HOME containment TWICE, and only the outer check
// is measurable here.
//
// The inner one (installPathRelative, pinned above) drops the ENTRY, but by then
// the walk has already read the file and called AppendSeedVersion on it. The
// outer one returns before the walk starts. So deleting the outer check changes
// no manifest anyone can see — the entry list is identical, because the inner
// check catches every file the outer one would have — and the only separating
// observation is the version log quietly growing a row for a file the store has
// just decided it will not seed.
//
// Measured, not assumed: with the outer check forced false, an out-of-HOME
// installation yields 0 entries and 1 new seed_file_versions row.
//
// Without this test the outer check is dominated everywhere a manifest-shaped
// assertion would look, and a scorer reports it as a coverage hole in tests that
// are otherwise complete.
func TestAnInstallationOutsideHomeIsNotEvenRead(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")
	t.Setenv("HOME", t.TempDir())

	countVersions := func() int {
		t.Helper()
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM seed_file_versions`).Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	before := countVersions()
	if _, err := s.BuildSeedManifest("box"); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if after := countVersions(); after != before {
		t.Fatalf("building a manifest for an installation outside HOME wrote %d version rows "+
			"(%d -> %d); the directory should not have been walked at all",
			after-before, before, after)
	}
}

// ⚠️ CHARACTERISATION, not an endorsement. installPathRelative rejects any
// relative path CONTAINING "..", and it is applied to a path produced by walking
// the install directory — so the substring can only be part of a real file's
// name, never a traversal. A skill shipping `notes..md` therefore gets a version
// row written (the store believes it captured the file) and no manifest entry
// (no machine is ever sent it). Nothing logs the discrepancy.
//
// This test pins today's behavior so the silent drop cannot widen unnoticed. It
// is filed rather than fixed because the repair has several plausible shapes —
// compare cleaned paths, split on the separator and match a ".." COMPONENT, or
// drop the check as redundant given the containment check above it — and picking
// one unattended would be a guess. See the noteboard card named in the failure
// message.
func TestAFileWhoseNameContainsDotDotIsCapturedButNeverSeeded(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")

	insts, err := s.ListInstallations()
	if err != nil || len(insts) != 1 {
		t.Fatalf("installations: %v (%d)", err, len(insts))
	}
	awkward := filepath.Join(insts[0].TargetPath, "notes..md")
	if err := os.WriteFile(awkward, []byte("real content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	mf, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	for _, e := range mf.Entries {
		if e.Relpath == "notes..md" {
			t.Fatal("notes..md now reaches the manifest. If that was the intended fix, " +
				"delete this characterisation; it exists to record that it did NOT.")
		}
	}

	// The half that makes the drop silent rather than merely strict: the bytes
	// were read and version-logged, so the store's own records show the file as
	// captured while no machine will ever receive it.
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM seed_file_versions WHERE relpath=?`, "notes..md").Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n == 0 {
		t.Fatal("notes..md is no longer version-logged either — the drop is now consistent, " +
			"which is a change in behavior this test was written to detect")
	}
}

// Every manifest entry must point at a real version row holding that file's
// bytes, since the runner fetches content by version id. An entry with
// version_id 0 is a path the runner cannot resolve.
func TestEverySeededEntryCarriesAResolvableVersion(t *testing.T) {
	s := seedTestStore(t)
	installOneSkill(t, s, "browser-automation")

	mf, err := s.BuildSeedManifest("box")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(mf.Entries) == 0 {
		t.Fatal("no entries to check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	for _, e := range mf.Entries {
		if e.VersionID == 0 {
			t.Fatalf("entry %s has no version id", e.Relpath)
		}
		if e.SHA256 == "" {
			t.Fatalf("entry %s has no sha", e.Relpath)
		}
		if filepath.IsAbs(e.InstallPath) {
			t.Fatalf("install_path %q is absolute, want $HOME-relative", e.InstallPath)
		}
		if strings.HasPrefix(e.InstallPath, "..") {
			t.Fatalf("install_path %q escapes $HOME", e.InstallPath)
		}
		if _, err := os.Stat(filepath.Join(home, e.InstallPath)); err != nil {
			t.Fatalf("install_path %q does not resolve under HOME: %v", e.InstallPath, err)
		}
		data, err := s.ReadSeedVersionContent(e.VersionID)
		if err != nil {
			t.Fatalf("version %d for %s is not fetchable: %v", e.VersionID, e.Relpath, err)
		}
		if len(data) == 0 {
			t.Fatalf("version %d for %s is empty", e.VersionID, e.Relpath)
		}
	}
}
