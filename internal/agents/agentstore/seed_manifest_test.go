package agentstore

import "testing"

// manifestEntryFor returns the single manifest entry for the fixture's one
// tracked file, and the bytes that entry actually points at. Failing here
// rather than returning a zero value keeps every caller below a two-line test.
func manifestEntryFor(t *testing.T, s *Store, machineID string) (SeedManifestEntry, string) {
	t.Helper()
	mf, err := s.BuildSeedManifest(machineID)
	if err != nil {
		t.Fatalf("BuildSeedManifest: %v", err)
	}
	if len(mf.Entries) != 1 {
		t.Fatalf("want 1 manifest entry, got %d", len(mf.Entries))
	}
	e := mf.Entries[0]
	body, err := s.ReadVersionContent(e.VersionID)
	if err != nil {
		t.Fatalf("ReadVersionContent(%d): %v", e.VersionID, err)
	}
	return e, string(body)
}

func onlyTrackedFile(t *testing.T, s *Store) TrackedFile {
	t.Helper()
	files, err := s.ListTrackedFiles("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 tracked file, got %d", len(files))
	}
	return files[0]
}

// The manifest is what a connected runner installs, so an entry naming content
// the file does not hold ships the wrong bytes to every machine.
//
// A revert is the only edit that separates "newest version" from "what is on
// disk": Scan's history guard searches the whole version log, so returning a
// file to content it has held before appends no row and leaves the newest one
// holding the superseded content. The tracked_files row is right throughout —
// only the manifest was wrong.
//
// This is the case the whole repair exists for. It fails against a manifest
// built from LatestVersion, reporting "BBB" where disk holds "AAA".
func TestSeedManifestShipsWhatIsOnDiskAfterARevert(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "AAA"})
	s := testStore(t)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, home, "CLAUDE.md", "BBB")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, home, "CLAUDE.md", "AAA")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}

	f := onlyTrackedFile(t, s)
	e, body := manifestEntryFor(t, s, "runner-1")

	if body != "AAA" {
		t.Errorf("manifest ships %q, disk holds \"AAA\" — every runner installs the superseded content", body)
	}
	if e.SHA256 != f.FSHash {
		t.Errorf("manifest sha %s != row fs_hash %s", e.SHA256, f.FSHash)
	}
}

// Control. A fix that made the revert case pass by breaking the ordinary
// forward edit would be worse than the defect, and this is the case that
// catches it — the revert test alone cannot, because a manifest hardwired to
// the oldest version would satisfy it.
func TestSeedManifestShipsWhatIsOnDiskAfterAPlainEdit(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "AAA"})
	s := testStore(t)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, home, "CLAUDE.md", "BBB")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}

	f := onlyTrackedFile(t, s)
	e, body := manifestEntryFor(t, s, "runner-1")

	if body != "BBB" {
		t.Errorf("manifest ships %q after a forward edit to \"BBB\"", body)
	}
	if e.SHA256 != f.FSHash {
		t.Errorf("manifest sha %s != row fs_hash %s", e.SHA256, f.FSHash)
	}
}

// A file scanned twice with no edit must still appear. Guards the case where
// disk, newest version and hash lookup all agree — the overwhelmingly common
// one, and the one a hash-resolving manifest would drop if FSHash were not
// kept current by the upsert.
func TestSeedManifestShipsAnUneditedFile(t *testing.T) {
	scanFixture(t, map[string]string{"CLAUDE.md": "AAA"})
	s := testStore(t)
	for i := 0; i < 2; i++ {
		if _, err := s.Scan(); err != nil {
			t.Fatal(err)
		}
	}

	f := onlyTrackedFile(t, s)
	e, body := manifestEntryFor(t, s, "runner-1")

	if body != "AAA" {
		t.Errorf("manifest ships %q for an unedited file holding \"AAA\"", body)
	}
	if e.SHA256 != f.FSHash {
		t.Errorf("manifest sha %s != row fs_hash %s", e.SHA256, f.FSHash)
	}
}

// The row's hash can move without the content landing in the log: Scan sets
// fs_hash from the upsert, then swallows the error if it cannot read the file
// back to append a version. The manifest must then advertise nothing for that
// path rather than fall back to whatever else is in the log, which is the
// same wrong-bytes failure the revert case describes.
//
// Driven by writing the row's hash directly, because the swallowed read error
// it stands for cannot be provoked through Scan without racing the walk.
func TestSeedManifestSkipsAFileWhoseDiskContentWasNeverRecorded(t *testing.T) {
	scanFixture(t, map[string]string{"CLAUDE.md": "AAA"})
	s := testStore(t)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	f := onlyTrackedFile(t, s)

	const unrecorded = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := s.db.Exec("UPDATE tracked_files SET fs_hash=? WHERE id=?", unrecorded, f.ID); err != nil {
		t.Fatal(err)
	}

	mf, err := s.BuildSeedManifest("runner-1")
	if err != nil {
		t.Fatalf("BuildSeedManifest: %v", err)
	}
	for _, e := range mf.Entries {
		if e.TrackedFileID == f.ID {
			body, _ := s.ReadVersionContent(e.VersionID)
			t.Fatalf("manifest advertises %q for a file whose disk content was never recorded", string(body))
		}
	}
}

// LatestVersion is the trap this repair removed from BuildSeedManifest, and
// it is still exported. Pinning the divergence keeps its doc-comment warning
// honest: if a later change made newest and current agree on a revert, this
// goes red and the warning can come off.
func TestLatestVersionDivergesFromDiskAfterARevert(t *testing.T) {
	home := scanFixture(t, map[string]string{"CLAUDE.md": "AAA"})
	s := testStore(t)
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, home, "CLAUDE.md", "BBB")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, home, "CLAUDE.md", "AAA")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}

	f := onlyTrackedFile(t, s)
	v, err := s.LatestVersion(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		t.Fatal("no version recorded")
	}
	if v.SHA256 == f.FSHash {
		t.Fatal("LatestVersion now agrees with disk after a revert — the warning on it is stale, and so is the reason BuildSeedManifest resolves by hash")
	}
}
