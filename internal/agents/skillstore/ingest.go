package skillstore

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SyncResult summarizes what happened during a Sync call.
type SyncResult struct {
	SourceID   int64    `json:"source_id"`
	SourceName string   `json:"source_name"`
	Commit     string   `json:"commit"`
	Found      int      `json:"found"`
	Inserted   int      `json:"inserted"`
	Updated    int      `json:"updated"`
	Pruned     int      `json:"pruned"`
	Errors     []string `json:"errors,omitempty"`
}

// Sync ingests every SKILL.md beneath the source's configured subpath. A git
// source is cloned or updated first; a local source is read where it lies. Skill
// files are copied into the store's data dir keyed by the skill's DB id, and
// pruning removes skills that have disappeared from the source.
func (s *Store) Sync(sourceID int64) (*SyncResult, error) {
	src, err := s.GetSource(sourceID)
	if err != nil {
		return nil, err
	}
	if !src.Enabled {
		return nil, fmt.Errorf("source %s is disabled", src.Name)
	}

	rootDir, commit, err := s.sourceRoot(src)
	if err != nil {
		return nil, err
	}

	res := &SyncResult{SourceID: sourceID, SourceName: src.Name, Commit: commit}

	searchRoot := rootDir
	if src.Subpath != "" {
		searchRoot = filepath.Join(rootDir, src.Subpath)
	}
	if _, err := os.Stat(searchRoot); err != nil {
		return nil, fmt.Errorf("subpath %s not found in %s", src.Subpath, src.Name)
	}

	var keepIDs []int64
	err = filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			// Skip .git subtrees
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		sk, inserted, ierr := s.ingestSkillFile(src, rootDir, path, commit)
		if ierr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", path, ierr))
			return nil
		}
		res.Found++
		keepIDs = append(keepIDs, sk.ID)
		if inserted {
			res.Inserted++
		} else {
			res.Updated++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk: %w", err)
	}

	pruned, err := s.DeleteSkillsForSourceExcept(sourceID, keepIDs)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	res.Pruned = pruned

	// Remove orphaned skill file directories
	if err := s.pruneOrphanSkillDirs(keepIDs); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("prune dirs: %v", err))
	}

	if err := s.MarkSourceSynced(sourceID, commit, res.Found); err != nil {
		return nil, fmt.Errorf("mark synced: %w", err)
	}
	return res, nil
}

// sourceRoot returns the directory to walk for a source's SKILL.md files, and
// the commit to stamp on the skills it yields.
//
// A git source is cloned or fetched into the store's own data dir and reports
// the checked-out SHA. A local source is read where it already lives and has no
// commit — its skills are authored here, not pulled from anywhere.
func (s *Store) sourceRoot(src *Source) (rootDir, commit string, err error) {
	switch src.Kind {
	case SourceKindLocal:
		if src.LocalPath == "" {
			return "", "", fmt.Errorf("source %s is local but has no local_path", src.Name)
		}
		root, err := filepath.Abs(src.LocalPath)
		if err != nil {
			return "", "", fmt.Errorf("resolve local_path for %s: %w", src.Name, err)
		}
		if err := s.refuseInstallDir(src.Name, root); err != nil {
			return "", "", err
		}
		info, err := os.Stat(root)
		if err != nil {
			return "", "", fmt.Errorf("local_path for %s: %w", src.Name, err)
		}
		if !info.IsDir() {
			return "", "", fmt.Errorf("local_path for %s is not a directory: %s", src.Name, root)
		}
		return root, "", nil
	case SourceKindGit, "":
		root := filepath.Join(s.ReposDir(), src.Name)
		commit, err := syncRepo(src, root)
		if err != nil {
			return "", "", fmt.Errorf("git sync %s: %w", src.Name, err)
		}
		return root, commit, nil
	default:
		return "", "", fmt.Errorf("source %s has unknown kind %q", src.Name, src.Kind)
	}
}

// refuseInstallDir rejects a local source rooted at, inside, or above the
// install dir.
//
// Install wipes and rewrites its target directory. A source overlapping that
// target would have the store ingesting the files it just wrote and destroying
// the authored originals on the next install — so this is a hard error at sync
// time rather than a caveat in the docs.
func (s *Store) refuseInstallDir(name, root string) error {
	installDir, err := filepath.Abs(s.installDir)
	if err != nil {
		return fmt.Errorf("resolve install dir: %w", err)
	}
	if overlaps(root, installDir) {
		return fmt.Errorf(
			"local source %s is rooted at %s, which overlaps the install dir %s; "+
				"author skills outside the install dir so installing them cannot overwrite the source",
			name, root, installDir)
	}
	return nil
}

// overlaps reports whether either path is the other, or contains it.
func overlaps(a, b string) bool {
	return a == b || isWithin(a, b) || isWithin(b, a)
}

// isWithin reports whether child sits under parent.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ingestSkillFile parses a SKILL.md, upserts the skill row, and copies
// sibling files into the store's files dir for that skill.
func (s *Store) ingestSkillFile(src *Source, rootDir, skillMDPath, commit string) (*Skill, bool, error) {
	body, err := os.ReadFile(skillMDPath)
	if err != nil {
		return nil, false, err
	}
	name, displayName, description, tags, err := parseSkillFrontmatter(body)
	if err != nil {
		return nil, false, err
	}
	if name == "" {
		// Fall back to directory name
		name = filepath.Base(filepath.Dir(skillMDPath))
	}
	skillDir := filepath.Dir(skillMDPath)
	relPath, err := filepath.Rel(rootDir, skillDir)
	if err != nil {
		relPath = skillDir
	}

	// Copy sibling files into the store (excluding SKILL.md itself, which is stored in DB).
	sk := &Skill{
		SourceID:     src.ID,
		Name:         name,
		DisplayName:  displayName,
		Description:  description,
		SkillMD:      string(body),
		SourcePath:   relPath,
		SourceCommit: commit,
		Tags:         tags,
		Enabled:      true,
	}
	inserted, err := s.UpsertSkill(sk)
	if err != nil {
		return nil, false, err
	}

	dstDir := s.SkillDir(sk.ID)
	if err := os.RemoveAll(dstDir); err != nil {
		return nil, false, fmt.Errorf("clear skill dir: %w", err)
	}
	fileCount, err := copySkillFiles(skillDir, dstDir)
	if err != nil {
		return nil, false, fmt.Errorf("copy files: %w", err)
	}
	sk.FileCount = fileCount
	if _, err := s.UpsertSkill(sk); err != nil {
		return nil, false, fmt.Errorf("update file_count: %w", err)
	}
	return sk, inserted, nil
}

func copySkillFiles(srcDir, dstDir string) (int, error) {
	count := 0
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "SKILL.md" {
			return nil
		}
		dst := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, dst); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (s *Store) pruneOrphanSkillDirs(keepIDs []int64) error {
	entries, err := os.ReadDir(s.SkillsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keep := make(map[string]struct{}, len(keepIDs))
	for _, id := range keepIDs {
		keep[fmt.Sprintf("%d", id)] = struct{}{}
	}
	// Also keep dirs for skills not owned by this sync source — verify against DB.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := keep[e.Name()]; ok {
			continue
		}
		// Is there any row with this id? If not, remove.
		var exists int
		err := s.db.QueryRow(`SELECT 1 FROM skills WHERE id=?`, e.Name()).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if rerr := os.RemoveAll(filepath.Join(s.SkillsDir(), e.Name())); rerr != nil {
				return fmt.Errorf("remove orphan skill dir %s: %w", e.Name(), rerr)
			}
		case err != nil:
			return fmt.Errorf("look up skill %s: %w", e.Name(), err)
		}
	}
	return nil
}

// ============================================
// SKILL.md frontmatter parsing
// ============================================

func parseSkillFrontmatter(body []byte) (name, displayName, description string, tags []string, err error) {
	// Expect leading `---\n...\n---\n` YAML block.
	if !bytes.HasPrefix(body, []byte("---\n")) && !bytes.HasPrefix(body, []byte("---\r\n")) {
		return "", "", "", nil, fmt.Errorf("missing YAML frontmatter")
	}
	after := body[4:]
	end := bytes.Index(after, []byte("\n---"))
	if end < 0 {
		return "", "", "", nil, fmt.Errorf("unterminated frontmatter")
	}
	fm := after[:end]
	// Unmarshal loosely — some upstream skills ship tags as a comma-separated
	// string like `tags: [foo, bar]` that isn't valid YAML sequence syntax.
	// Pull known fields out by type.
	var raw map[string]any
	if err := yaml.Unmarshal(fm, &raw); err != nil {
		return "", "", "", nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	name, _ = raw["name"].(string)
	displayName, _ = raw["display_name"].(string)
	description, _ = raw["description"].(string)
	tags = coerceTags(raw["tags"])
	return name, displayName, description, tags, nil
}

func coerceTags(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		s := strings.TrimSpace(t)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// ============================================
// git sync
// ============================================

// syncRepo clones the source if missing, else fetches+hard-resets to the
// configured branch. Returns the current commit SHA.
func syncRepo(src *Source, repoDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
			return "", err
		}
		cmd := exec.Command("git", "clone", "--depth=1", "--branch", src.Branch, src.RepoURL, repoDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone: %w\n%s", err, strings.TrimSpace(string(out)))
		}
	} else {
		cmd := exec.Command("git", "-C", repoDir, "fetch", "--depth=1", "origin", src.Branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		cmd = exec.Command("git", "-C", repoDir, "reset", "--hard", "FETCH_HEAD")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git reset: %w\n%s", err, strings.TrimSpace(string(out)))
		}
	}
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
