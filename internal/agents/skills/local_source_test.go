package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates <root>/<name>/SKILL.md with the given frontmatter name.
func writeSkill(t *testing.T, root, dirName, skillName, body string) string {
	t.Helper()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "---\nname: " + skillName + "\ndescription: test skill\n---\n\n" + body
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestSourceDefaultsToGit(t *testing.T) {
	s := newTestStore(t)
	src := &Source{Name: "upstream", Enabled: true, RepoURL: "https://example.invalid/skills.git"}
	if err := s.UpsertSource(testContext, src); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetSourceByName(testContext, "upstream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != SourceKindGit {
		t.Fatalf("kind = %q, want %q", got.Kind, SourceKindGit)
	}
}

func TestLocalSourceIngestsSkills(t *testing.T) {
	s := newTestStore(t)
	root := filepath.Join(t.TempDir(), "authored")
	writeSkill(t, root, "browser-automation", "browser-automation", "body text")

	src := &Source{Name: "host", Enabled: true, Kind: SourceKindLocal, LocalPath: root, TrustTier: "personal"}
	if err := s.UpsertSource(testContext, src); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	res, err := s.Sync(testContext, src.ID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Found != 1 || res.Inserted != 1 {
		t.Fatalf("found=%d inserted=%d, want 1/1", res.Found, res.Inserted)
	}
	// A local source has no commit to report.
	if res.Commit != "" {
		t.Fatalf("commit = %q, want empty for a local source", res.Commit)
	}

	skills, err := s.ListSkills(testContext, ListSkillsFilter{SourceID: src.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	if skills[0].Name != "browser-automation" {
		t.Fatalf("name = %q", skills[0].Name)
	}
	if skills[0].ID == 0 {
		t.Fatal("skill has no id — the whole point is that it becomes joinable by id")
	}
}

// The guard that stops the store ingesting its own install output. Without it,
// Install would wipe and rewrite the directory the source was read from.
func TestLocalSourceRefusesInstallDirOverlap(t *testing.T) {
	s := newTestStore(t)
	installDir := s.InstallDir()
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("mkdir install: %v", err)
	}

	cases := []struct {
		name string
		root string
	}{
		{"exactly the install dir", installDir},
		{"inside the install dir", filepath.Join(installDir, "nested")},
		{"a parent of the install dir", filepath.Dir(installDir)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.root, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			writeSkill(t, tc.root, "some-skill", "some-skill", "body")

			src := &Source{Name: "overlapping-" + tc.name, Enabled: true, Kind: SourceKindLocal, LocalPath: tc.root}
			if err := s.UpsertSource(testContext, src); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			_, err := s.Sync(testContext, src.ID)
			if err == nil {
				t.Fatal("sync succeeded; want refusal for a source overlapping the install dir")
			}
			if !strings.Contains(err.Error(), "install dir") {
				t.Fatalf("error %q does not explain the overlap", err)
			}
		})
	}
}

func TestLocalSourceRequiresExistingDirectory(t *testing.T) {
	s := newTestStore(t)

	missing := &Source{Name: "missing", Enabled: true, Kind: SourceKindLocal, LocalPath: filepath.Join(t.TempDir(), "nope")}
	if err := s.UpsertSource(testContext, missing); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.Sync(testContext, missing.ID); err == nil {
		t.Fatal("sync succeeded for a local_path that does not exist")
	}

	filePath := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	notDir := &Source{Name: "not-a-dir", Enabled: true, Kind: SourceKindLocal, LocalPath: filePath}
	if err := s.UpsertSource(testContext, notDir); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.Sync(testContext, notDir.ID); err == nil {
		t.Fatal("sync succeeded for a local_path that is a file")
	}
}

func TestLocalSourceWithoutPathIsRejected(t *testing.T) {
	s := newTestStore(t)
	src := &Source{Name: "pathless", Enabled: true, Kind: SourceKindLocal}
	if err := s.UpsertSource(testContext, src); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.Sync(testContext, src.ID); err == nil {
		t.Fatal("sync succeeded for a local source with no local_path")
	}
}

func TestUnknownSourceKindIsRejected(t *testing.T) {
	s := newTestStore(t)
	src := &Source{Name: "weird", Enabled: true, Kind: SourceKind("carrier-pigeon"), RepoURL: "x"}
	if err := s.UpsertSource(testContext, src); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, err := s.Sync(testContext, src.ID)
	if err == nil {
		t.Fatal("sync succeeded for an unknown source kind")
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("error %q does not name the offending kind", err)
	}
}
