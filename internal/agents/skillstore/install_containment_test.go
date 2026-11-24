package skillstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ingestSkillNamed syncs a one-skill local source whose frontmatter name is
// skillName, and returns the skill's id.
//
// The name is what these tests are about, so it is deliberately not derived
// from the directory: a traversing name has to reach the database exactly as an
// upstream repository would have written it.
func ingestSkillNamed(t *testing.T, s *Store, sourceName, dirName, skillName string) int64 {
	t.Helper()
	root := filepath.Join(t.TempDir(), "authored")
	writeSkill(t, root, dirName, skillName, "body")
	src := &Source{Name: sourceName, Enabled: true, Kind: SourceKindLocal, LocalPath: root}
	if err := s.UpsertSource(src); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if _, err := s.Sync(src.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	skills, err := s.ListSkills(ListSkillsFilter{})
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	for _, sk := range skills {
		if sk.Name == skillName {
			return sk.ID
		}
	}
	t.Fatalf("skill %q was not ingested; got %d skills", skillName, len(skills))
	return 0
}

// The refusing side. A traversing name must be refused BEFORE the RemoveAll,
// which is the only thing that makes the refusal worth anything: an error
// returned after the directory is gone is a report, not a guard.
func TestAnEscapingSkillNameIsRefusedBeforeAnythingIsErased(t *testing.T) {
	for _, tc := range []struct {
		label string
		name  string
	}{
		{"parent directory", "../not-the-install-dir"},
		{"the install dir itself", "."},
		{"two levels up", "../../repos"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s := newTestStore(t)

			// A bystander at the escaping target, so "was it erased" is
			// observable rather than inferred from the error alone.
			target := filepath.Clean(filepath.Join(s.InstallDir(), tc.name))
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir bystander dir: %v", err)
			}
			bystander := filepath.Join(target, "bystander.txt")
			if err := os.WriteFile(bystander, []byte("owned by something else"), 0o644); err != nil {
				t.Fatalf("write bystander: %v", err)
			}

			id := ingestSkillNamed(t, s, "upstream", "escaper", tc.name)
			inst, err := s.Install(id, false)

			if err == nil {
				t.Fatalf("Install accepted the name %q and wrote %s", tc.name, inst.TargetPath)
			}
			if !errors.Is(err, ErrInstallTargetEscapesInstallDir) {
				t.Fatalf("Install refused %q with the wrong error: %v", tc.name, err)
			}
			if _, err := os.Stat(bystander); err != nil {
				t.Fatalf("the bystander at %s did not survive the refusal: %v", bystander, err)
			}
		})
	}
}

// An ABSOLUTE skill name is not a traversal and must not be refused as one.
// filepath.Join folds a leading separator into a relative segment, so
// "/etc/cron.d" resolves to <installDir>/etc/cron.d — inside, and therefore
// allowed. Recorded as a test because the name reads like the worst case and is
// not, and a guard credited with catching it would be credited wrongly.
func TestAnAbsoluteSkillNameResolvesInsideTheInstallDirAndIsAllowed(t *testing.T) {
	s := newTestStore(t)
	target, err := s.installTargetForName("/etc/cron.d")
	if err != nil {
		t.Fatalf("an absolute name was refused: %v", err)
	}
	want := filepath.Join(s.InstallDir(), "etc", "cron.d")
	if target != want {
		t.Fatalf("target = %s, want %s", target, want)
	}
}

// The permitting side. Without this the refusing test above passes for a guard
// that refuses everything, which is a guard that has been shown nothing.
func TestAnOrdinarySkillNameStillInstalls(t *testing.T) {
	s := newTestStore(t)
	id := ingestSkillNamed(t, s, "upstream", "ordinary", "ordinary-skill")

	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("Install refused an ordinary name: %v", err)
	}
	want := filepath.Join(s.InstallDir(), "ordinary-skill")
	if inst.TargetPath != want {
		t.Fatalf("target = %s, want %s", inst.TargetPath, want)
	}
	if _, err := os.Stat(filepath.Join(want, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md was not written to %s: %v", want, err)
	}
}

// Uninstall reads its path back from the installations table, so a row written
// before Install started checking can still name a directory outside the
// install dir. The check on the way out is what covers those rows.
func TestUninstallRefusesARecordedTargetOutsideTheInstallDir(t *testing.T) {
	s := newTestStore(t)
	id := ingestSkillNamed(t, s, "upstream", "ordinary", "ordinary-skill")
	if _, err := s.Install(id, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Forge the pre-guard row this test exists for.
	outside := filepath.Join(filepath.Dir(s.InstallDir()), "escaped-install")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bystander := filepath.Join(outside, "bystander.txt")
	if err := os.WriteFile(bystander, []byte("owned by something else"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE installations SET target_path=? WHERE skill_id=?`, outside, id); err != nil {
		t.Fatalf("forge row: %v", err)
	}

	_, err := s.Uninstall(id)
	if err == nil {
		t.Fatalf("Uninstall erased a recorded path outside the install dir")
	}
	if !errors.Is(err, ErrInstallTargetEscapesInstallDir) {
		t.Fatalf("Uninstall refused with the wrong error: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("the bystander at %s did not survive the refusal: %v", bystander, err)
	}
}

// The permitting side of the same guard.
func TestUninstallStillRemovesAnOrdinaryInstallation(t *testing.T) {
	s := newTestStore(t)
	id := ingestSkillNamed(t, s, "upstream", "ordinary", "ordinary-skill")
	inst, err := s.Install(id, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if _, err := s.Uninstall(id); err != nil {
		t.Fatalf("Uninstall refused an ordinary installation: %v", err)
	}
	if _, err := os.Stat(inst.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("%s survived an accepted uninstall (stat err = %v)", inst.TargetPath, err)
	}
}

// The refusal has to name the skill and both paths, or an operator reading it
// cannot tell which upstream repository to go and look at.
func TestTheRefusalNamesTheSkillAndBothPaths(t *testing.T) {
	s := newTestStore(t)
	_, err := s.installTargetForName("../not-the-install-dir")
	if err == nil {
		t.Fatal("installTargetForName accepted a traversing name")
	}
	msg := err.Error()
	for _, want := range []string{"../not-the-install-dir", s.InstallDir()} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not name %q", msg, want)
		}
	}
}
