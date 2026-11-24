package agentstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// SetTrackedFileEnabled is the only store method whose effect is a rename on
// disk as well as a row update, and until this file nothing executed it: an
// unconditional panic() on its first line left `go test ./...` green. Measured
// by the 202nd nightly pass, re-measured by the 221st before writing these.
//
// The two effects are what makes it worth its own file. Every other enable
// switch on this box writes one row, so reading the row back proves the write
// happened. Here the row and the file can disagree, and the row is the half the
// HTTP reply reports — so a handler test that asserts `{"enabled":false}` passes
// against a function that never touched the disk. Each test below therefore
// asserts the FILE, and separately asserts the row, rather than trusting either
// to stand for the other.

// enabledFixture builds a throwaway $HOME holding the given files, scans it into
// a fresh store, and returns both. Rows are created the way production creates
// them — by Scan() over a real tree — because tracked_files has no exported
// upsert and a hand-inserted row would not prove the scan and the switch agree
// about what a path means.
func enabledFixture(t *testing.T, files map[string]string) (string, *Store) {
	t.Helper()
	home := scanFixture(t, files)
	s := testStore(t)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	return home, s
}

// rowFor returns the tracked_files row whose canonical path is home/rel.
func rowFor(t *testing.T, s *Store, home, rel string) *TrackedFile {
	t.Helper()
	want := filepath.Join(home, rel)
	files, err := s.ListTrackedFiles("", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if files[i].Path == want {
			return &files[i]
		}
	}
	t.Fatalf("no tracked_files row for %s", want)
	return nil
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s: %s still exists", why, path)
	}
}

func mustHoldContent(t *testing.T, path, want, why string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: reading %s: %v", why, path, err)
	}
	if string(got) != want {
		t.Fatalf("%s: %s holds %q, want %q", why, path, got, want)
	}
}

// Disabling has to move the file. A row flipped to enabled=false over a file
// still sitting at its canonical path is the failure this whole file exists to
// catch: every reader of a tracked file resolves it through DiskPath(), which
// answers from the ROW, so the row and the disk disagreeing means every read
// afterwards looks for the file at a path nothing put it at.
func TestDisablingATrackedFileMovesItOnDisk(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}

	mustNotExist(t, filepath.Join(home, "CLAUDE.md"), "after disabling, the canonical path")
	mustHoldContent(t, filepath.Join(home, "CLAUDE.md.disabled"), "ORIGINAL", "after disabling")
}

// ...and it has to write the row. Separated from the test above on purpose: a
// rename with no UPDATE and an UPDATE with no rename are different bugs that
// one combined assertion would report as the same failure, and only one of them
// is visible from the HTTP reply.
func TestDisablingATrackedFileWritesTheRow(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	returned, err := s.SetTrackedFileEnabled(f.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if returned.Enabled {
		t.Error("the returned row still reports enabled")
	}

	// Read the row back through a fresh query rather than trusting the value
	// the call handed us: a function that set the field on its in-memory copy
	// and never ran the UPDATE returns exactly the same struct.
	fresh, err := s.GetTrackedFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Enabled {
		t.Error("the stored row still reports enabled; the UPDATE did not happen")
	}
	if fresh.DiskPath() != filepath.Join(home, "CLAUDE.md.disabled") {
		t.Errorf("DiskPath() = %s, want the .disabled variant", fresh.DiskPath())
	}
}

func TestEnablingATrackedFileMovesItBackAndWritesTheRow(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrackedFileEnabled(f.ID, true); err != nil {
		t.Fatal(err)
	}

	mustHoldContent(t, filepath.Join(home, "CLAUDE.md"), "ORIGINAL", "after re-enabling")
	mustNotExist(t, filepath.Join(home, "CLAUDE.md.disabled"), "after re-enabling, the disabled path")

	fresh, err := s.GetTrackedFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Enabled {
		t.Error("the stored row still reports disabled")
	}
}

// The switch must not be a content edit. It renames, so the bytes are the same
// bytes — including a trailing newline and any content the version log has no
// entry for.
func TestARoundTripThroughDisabledChangesNoBytes(t *testing.T) {
	const body = "line one\nline two\n\n# heading\n"
	home, s := enabledFixture(t, map[string]string{"AGENTS.md": body})
	f := rowFor(t, s, home, "AGENTS.md")

	for i := 0; i < 3; i++ {
		if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
			t.Fatalf("round %d disable: %v", i, err)
		}
		if _, err := s.SetTrackedFileEnabled(f.ID, true); err != nil {
			t.Fatalf("round %d enable: %v", i, err)
		}
	}

	mustHoldContent(t, filepath.Join(home, "AGENTS.md"), body, "after three round trips")
}

// Setting the state a row already has must touch nothing at all. The guard is a
// row comparison, so the cheap way to get this wrong is to drop it and rename
// unconditionally — which, for an already-disabled file, means renaming
// CLAUDE.md.disabled to CLAUDE.md.disabled.disabled and stranding it under a
// name no scan classifies.
func TestSettingTheStateTheRowAlreadyHasMovesNothing(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	// A sentinel at the destination the switch WOULD rename over. The no-op
	// path must leave it alone; an unconditional rename destroys it.
	sentinel := filepath.Join(home, "CLAUDE.md.disabled")
	if err := os.WriteFile(sentinel, []byte("SENTINEL"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetTrackedFileEnabled(f.ID, true); err != nil {
		t.Fatal(err)
	}
	mustHoldContent(t, filepath.Join(home, "CLAUDE.md"), "ORIGINAL", "enabling an enabled row")
	mustHoldContent(t, sentinel, "SENTINEL", "enabling an enabled row")

	// Now the other direction, from a genuinely disabled row.
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	mustHoldContent(t, sentinel, "ORIGINAL", "disabling an already-disabled row")
	mustNotExist(t, sentinel+".disabled", "disabling an already-disabled row")
}

// The no-op guard's ONLY observable effect, and it is not the obvious one.
//
// When f.Enabled already equals enable, `from` and `to` compute to the same
// path by construction — DiskPath() and the `to` expression are the same
// function of the same bool — so deleting the guard turns the body into
// os.Rename(x, x), which succeeds and moves nothing. Every happy-path assertion
// about a redundant call therefore passes with the guard gone, which is exactly
// what the scorer reported: the row for "the no-op guard is dropped" came back
// UNNOTICED against the six tests above.
//
// The one input that separates them is a file that is not on disk. With the
// guard, a redundant disable is a success because there is nothing to do; with
// it gone, the stat fails and the caller is told the file is missing. That is
// the right contract — asking for the state a row already has must not depend
// on the filesystem — and this is the only test that can see it.
func TestARedundantCallSucceedsEvenWhenTheFileIsNotOnDisk(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "CLAUDE.md.disabled")); err != nil {
		t.Fatal(err)
	}

	got, err := s.SetTrackedFileEnabled(f.ID, false)
	if err != nil {
		t.Fatalf("a redundant disable consulted the filesystem and failed: %v", err)
	}
	if got == nil {
		t.Fatal("a redundant disable returned no row")
	}
	if got.Enabled {
		t.Error("the returned row reports enabled")
	}
}

// A source file that is not where the row says it is must be an error, and the
// row must survive it unchanged. Writing the row anyway would record a move
// that did not happen and point every later read at nothing.
func TestAMissingSourceFileIsAnErrorAndLeavesTheRowAlone(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if err := os.Remove(filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}

	got, err := s.SetTrackedFileEnabled(f.ID, false)
	if err == nil {
		t.Fatal("disabling a file that is not on disk succeeded")
	}
	if got != nil {
		t.Errorf("an error result also returned a row: %+v", got)
	}

	fresh, ferr := s.GetTrackedFile(f.ID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if !fresh.Enabled {
		t.Error("the row was flipped to disabled even though the rename never happened")
	}
	mustNotExist(t, filepath.Join(home, "CLAUDE.md.disabled"), "a failed disable")
}

// The suffix is a contract between this function and canonicalPath(): the
// switch writes ".disabled" and the scan is what has to recognize it. Drift the
// suffix in one of them and a disabled file stops being tracked entirely, which
// no test confined to either side can see.
func TestAScanAgreesWithTheSuffixTheSwitchWrites(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}

	files, err := s.ListTrackedFiles("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("scan produced %d rows for one file: %+v", len(files), files)
	}
	if files[0].ID != f.ID {
		t.Errorf("scan replaced row %d with %d; the disabled file was not recognized as the same file", f.ID, files[0].ID)
	}
	if files[0].Enabled {
		t.Error("scan re-enabled a file the switch disabled")
	}
	if files[0].Status == "missing" {
		t.Error("scan marked the disabled file missing; it is on disk under its .disabled name")
	}
}

// ---------------------------------------------------------------------------
// HTTP layer
// ---------------------------------------------------------------------------

func postRoute(t *testing.T, mux *http.ServeMux, path string) (int, TrackedFile) {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewBufferString(""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var f TrackedFile
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	return w.Code, f
}

// enableFile and disableFile are one-line siblings differing only by the bool
// they forward. Drive them separately and both still pass when both forward the
// same value — the service silently loses a verb. So both routes are driven
// against ONE row here, and each is checked against the disk.
func TestBothEnableAndDisableRoutesDriveTheSameRow(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	s := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	f := rowFor(t, s, home, "CLAUDE.md")

	canonical := filepath.Join(home, "CLAUDE.md")
	disabled := canonical + ".disabled"

	code, body := postRoute(t, mux, fmt.Sprintf("/files/%d/disable", f.ID))
	if code != 200 {
		t.Fatalf("disable route: %d", code)
	}
	if body.Enabled {
		t.Error("disable route replied enabled=true")
	}
	mustNotExist(t, canonical, "after POST disable")
	mustHoldContent(t, disabled, "ORIGINAL", "after POST disable")

	code, body = postRoute(t, mux, fmt.Sprintf("/files/%d/enable", f.ID))
	if code != 200 {
		t.Fatalf("enable route: %d", code)
	}
	if !body.Enabled {
		t.Error("enable route replied enabled=false")
	}
	mustHoldContent(t, canonical, "ORIGINAL", "after POST enable")
	mustNotExist(t, disabled, "after POST enable")
}

// The reply has to report what was written, not what was asked for. A handler
// that builds its 200 from the bool it was called with answers correctly for a
// store call that did nothing.
func TestTheDisableRouteReportsTheStateItActuallyWrote(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	s := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	f := rowFor(t, s, home, "CLAUDE.md")

	_, body := postRoute(t, mux, fmt.Sprintf("/files/%d/disable", f.ID))

	fresh, err := s.GetTrackedFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body.Enabled != fresh.Enabled {
		t.Errorf("reply said enabled=%v, the stored row says %v", body.Enabled, fresh.Enabled)
	}
	if _, err := os.Stat(fresh.DiskPath()); err != nil {
		t.Errorf("the row points at %s, which is not on disk: %v", fresh.DiskPath(), err)
	}
}

// A file id that is not a number must be refused at the edge. The store takes
// an int64 and would otherwise be handed a zero that matches no row.
func TestTheEnableRouteRefusesAnUnparseableID(t *testing.T) {
	_, mux := NewTestServer(t)
	code, _ := postRoute(t, mux, "/files/not-a-number/enable")
	if code != 400 {
		t.Errorf("expected 400 for a non-numeric id, got %d", code)
	}
}

// A rename that cannot happen must reach the caller as a failure. Reporting 200
// for it would tell the UI a file had been disabled while it sits enabled on
// disk — the one state the whole switch exists to keep straight.
func TestTheDisableRouteReportsAFailedRename(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	s := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	f := rowFor(t, s, home, "CLAUDE.md")
	if err := os.Remove(filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}

	code, _ := postRoute(t, mux, fmt.Sprintf("/files/%d/disable", f.ID))
	if code == 200 {
		t.Fatal("a disable whose rename could not happen answered 200")
	}

	fresh, err := s.GetTrackedFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Enabled {
		t.Error("the row was written even though the route reported a failure")
	}
}

// ---------------------------------------------------------------------------
// Characterisation — this pins a DEFECT, deliberately
// ---------------------------------------------------------------------------

// ⚠️ This test asserts behavior that is WRONG, so that changing it has to be
// deliberate. Do not read a passing run as an endorsement.
//
// os.Rename replaces its destination silently. Enabling a file whose canonical
// path has since been re-created — by hand, by a git checkout, or by an agent
// following the instruction in ~/AGENTS.md to edit these files with its own
// tools — destroys those bytes and captures no version of them.
//
// That is a direct contradiction of the invariant stated forty lines above
// SetTrackedFileEnabled in this same file, where captureExistingIfNew is
// described as "the safety net that makes every overwrite non-destructive: even
// if the bridge missed a manual edit between our last knowledge and now, that
// content lands in history before we clobber it." Every other write path calls
// it. This one does not, and the drift is reachable in both directions, because
// Scan() rewrites `enabled` from whichever variant its walk sees first.
//
// Filed for a decision rather than fixed here: the repair has at least three
// shapes (capture the destination as a version, refuse the rename, or move the
// loser aside) and picking one unattended would be a guess.
func TestEnablingOverAnExistingCanonicalFileDestroysItWithoutCapturingAVersion(t *testing.T) {
	home, s := enabledFixture(t, map[string]string{"CLAUDE.md": "ORIGINAL"})
	f := rowFor(t, s, home, "CLAUDE.md")

	if _, err := s.SetTrackedFileEnabled(f.ID, false); err != nil {
		t.Fatal(err)
	}
	// Something re-creates the canonical file while the row says disabled.
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte("FRESH USER CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListVersions(f.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetTrackedFileEnabled(f.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("current behavior changed: canonical path holds %q, not the archived %q. "+
			"If the rename now preserves the destination, this characterisation is obsolete — "+
			"delete it and assert the new contract.", got, "ORIGINAL")
	}

	after, err := s.ListVersions(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("current behavior changed: %d versions before the clobber, %d after. "+
			"Capturing the destroyed bytes is the fix this test was written to notice — "+
			"replace it with an assertion that the captured version holds %q.",
			len(before), len(after), "FRESH USER CONTENT")
	}
}
