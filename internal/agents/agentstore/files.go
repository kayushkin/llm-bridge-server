package agentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TrackedFile is a reference to a file on disk that agent-store indexes.
// The DB holds no content — reads and writes hit the filesystem live.
type TrackedFile struct {
	ID            int64  `json:"id"`
	Path          string `json:"path"` // canonical path (no .disabled suffix)
	Scope         string `json:"scope"`
	AgentSlug     string `json:"agent_slug,omitempty"`
	Enabled       bool   `json:"enabled"`
	FSHash        string `json:"fs_hash,omitempty"`
	Size          int64  `json:"size"`
	MTime         int64  `json:"mtime"`
	LastScannedAt int64  `json:"last_scanned_at"`
	Status        string `json:"status"` // present, missing
	// IgnoredByRuleID is the tracked_file_ignore_rules row that says this file
	// is a copy or third-party, 0 when none does. See trackedfileignore.go.
	IgnoredByRuleID int64 `json:"ignored_by_rule_id,omitempty"`
	CreatedAt       int64 `json:"created_at"`
	UpdatedAt       int64 `json:"updated_at"`
}

// Well-known agent-config filenames (matched anywhere outside ~/.claude).
var agentFileNames = map[string]bool{
	"CLAUDE.md":               true,
	"AGENTS.md":               true,
	"GEMINI.md":               true,
	"copilot-instructions.md": true,
	".cursorrules":            true,
	".clinerules":             true,
	".windsurfrules":          true,
	".aider.conf.yml":         true,
	".continuerules":          true,
}

// Directories to skip during the walk.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"build":        true,
	".next":        true,
	"vendor":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".cache":       true,
	"target":       true,
	".pnpm-store":  true,
	".yarn":        true,
}

// classifyFile returns the scope for a file, or ("", false) if it is not tracked.
// Skills are intentionally excluded — skill-store owns those.
func classifyFile(absPath, home string) (string, bool) {
	base := filepath.Base(absPath)
	// Strip .disabled suffix for classification purposes.
	base = strings.TrimSuffix(base, ".disabled")

	claudeRoot := filepath.Join(home, ".claude")
	if strings.HasPrefix(absPath, claudeRoot+string(os.PathSeparator)) {
		if !strings.HasSuffix(base, ".md") {
			return "", false
		}
		rel := strings.TrimPrefix(absPath, claudeRoot+string(os.PathSeparator))
		rel = strings.TrimSuffix(rel, ".disabled")
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) == 1 {
			return "global", true
		}
		switch parts[0] {
		case "commands":
			return "command", true
		case "agents":
			return "subagent", true
		case "projects":
			for i, p := range parts {
				if p == "memory" && i > 1 {
					return "memory", true
				}
			}
		}
		return "", false
	}

	inberRoot := filepath.Join(home, "inber-workspace", "agents")
	if strings.HasPrefix(absPath, inberRoot+string(os.PathSeparator)) {
		rel := strings.TrimPrefix(absPath, inberRoot+string(os.PathSeparator))
		rel = strings.TrimSuffix(rel, ".disabled")
		if !strings.Contains(rel, string(os.PathSeparator)) && strings.HasSuffix(rel, ".md") {
			return "inber", true
		}
		return "", false
	}

	// Well-known agent-config files placed directly in $HOME are treated as
	// global. Anywhere else they're project-scoped.
	if filepath.Dir(absPath) == home && agentFileNames[base] {
		return "global", true
	}

	if agentFileNames[base] {
		return "project", true
	}
	return "", false
}

// canonicalPath strips the .disabled suffix if present.
func canonicalPath(p string) (string, bool) {
	if strings.HasSuffix(p, ".disabled") {
		return strings.TrimSuffix(p, ".disabled"), false
	}
	return p, true
}

func hashFile(path string) (string, int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), info.Size(), info.ModTime().Unix(), nil
}

// agentSlugFromPath returns the filename stem if it matches a known agent slug.
// Returns empty string if no match or scope doesn't link to agents.
func (s *Store) agentSlugFromPath(path, scope string) string {
	if scope != "subagent" && scope != "inber" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(path), ".disabled")
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		return ""
	}
	var slug string
	err := s.db.QueryRow("SELECT slug FROM agents WHERE slug = ?", stem).Scan(&slug)
	if err != nil {
		return ""
	}
	return slug
}

// ScanResult is the aggregate of one full scan.
//
// Added, Updated and Unchanged are disjoint and together account for every
// file the walk visited, so Added+Updated+Unchanged == Scanned. Missing is
// counted over rows the walk did NOT reach (their file is gone), so it is
// deliberately outside that identity -- do not "fix" it into the sum.
//
// Updated means the file's content hash differs from the one on the row. It
// used to mean "a row for this path already existed", which made it identical
// to Scanned-Added on every run and left callers no way to tell an edit from a
// no-op. AGENTS.md tells agents to POST /files/scan to confirm an out-of-band
// edit landed; Updated is the number they read to confirm it, so it has to
// mean what its name says.
//
// Errors names everything the scan could not account for, and it is what makes
// the identity above safe to read. That identity holds over survivors: while a
// file that failed to hash or failed to upsert was dropped from every counter,
// Added+Updated+Unchanged == Scanned stayed perfectly true on a scan that
// silently covered fewer files than the box has, so a partial run was
// indistinguishable from a complete one. Errors is deliberately outside the
// sum, like Missing -- a non-empty Errors means Scanned is an undercount.
//
// It is a list rather than a count because of who reads it. AGENTS.md tells
// agents to POST /files/scan to confirm one particular edit landed, and
// "errors: 1" does not tell them whether the dropped file was theirs. len()
// gives the count, so there is no second number to go stale.
//
// A failed file does not abort the sweep: a scan that covers 153 of 154 files
// and says which one it missed is more useful than one that returns an error.
type ScanResult struct {
	Scanned   int         `json:"scanned"`
	Added     int         `json:"added"`
	Updated   int         `json:"updated"`
	Unchanged int         `json:"unchanged"`
	Missing   int         `json:"missing"`
	Ignored   int         `json:"ignored"` // of Scanned, how many an ignore rule claims
	Errors    []ScanError `json:"errors"`
}

// ScanError is one file the scan could not account for, named at the point of
// failure. Stage says which step gave up, because the three mean different
// things: a file that would not hash is on disk and unreadable, one that would
// not upsert was read fine and the DB refused it, and one that would not mark
// missing is a row still claiming to be present.
type ScanError struct {
	Path  string `json:"path"`
	Stage string `json:"stage"`
	Err   string `json:"error"`
}

const (
	scanStageHash        = "hash"
	scanStageUpsert      = "upsert"
	scanStageMarkMissing = "mark_missing"
)

// fail records one unaccounted-for file on the result.
func (r *ScanResult) fail(path, stage string, err error) {
	r.Errors = append(r.Errors, ScanError{Path: path, Stage: stage, Err: err.Error()})
}

// upsertOutcome says what one visited file did to the store. It exists because
// the previous bool ("was this row newly inserted?") could not distinguish an
// edit from a no-op, which is the whole question a scan is asked.
type upsertOutcome int

const (
	upsertAdded upsertOutcome = iota
	upsertUpdated
	upsertUnchanged
)

// Scan walks $HOME (with a curated set of skip paths), classifies every
// matching file, and upserts tracked_files rows. Rows whose underlying file
// has disappeared (in both enabled and .disabled variants) are marked missing.
//
// We skip third-party caches (~/.config, ~/.codex) and ephemeral forge envs
// so imported plugin/skill bundles don't pollute the index.
func (s *Store) Scan() (*ScanResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}

	// Absolute paths to skip during the walk.
	skipPaths := map[string]bool{
		filepath.Join(home, ".config"):                         true,
		filepath.Join(home, ".codex"):                          true,
		filepath.Join(home, ".cache"):                          true,
		filepath.Join(home, "forge", "envs"):                   true,
		filepath.Join(home, ".claude", "plugins"):              true,
		filepath.Join(home, ".claude", "skills"):               true,
		filepath.Join(home, ".claude", "cache"):                true,
		filepath.Join(home, ".claude", "sessions"):             true,
		filepath.Join(home, ".claude", "file-history"):         true,
		filepath.Join(home, ".claude", "backups"):              true,
		filepath.Join(home, ".claude", "debug"):                true,
		filepath.Join(home, ".claude", "downloads"):            true,
		filepath.Join(home, ".claude", "shell-snapshots"):      true,
		filepath.Join(home, ".claude", "telemetry"):            true,
		filepath.Join(home, ".claude", "session-env"):          true,
		filepath.Join(home, ".claude", "mcp-needs-auth-cache"): true,
	}

	res := &ScanResult{}
	seen := make(map[string]bool)
	ignoreRules, err := s.ListTrackedFileIgnoreRules()
	if err != nil {
		return nil, fmt.Errorf("list ignore rules: %w", err)
	}

	visitFile := func(path string) {
		scope, ok := classifyFile(path, home)
		if !ok {
			return
		}
		canon, enabled := canonicalPath(path)
		if seen[canon] {
			return
		}
		seen[canon] = true
		hash, size, mtime, err := hashFile(path)
		if err != nil {
			// A file we classified as tracked but cannot read (permissions, or
			// a delete racing the walk) is a hole in the scan, not a file that
			// was never there. Name it.
			res.fail(path, scanStageHash, err)
			return
		}
		slug := s.agentSlugFromPath(canon, scope)
		fileID, outcome, err := s.upsertTrackedFile(canon, scope, slug, enabled, hash, size, mtime)
		if err != nil {
			res.fail(canon, scanStageUpsert, err)
			return
		}
		ignoredByRuleID, err := s.stampTrackedFileIgnore(fileID, canon, home, ignoreRules)
		if err != nil {
			res.fail(canon, scanStageUpsert, err)
			return
		}
		if ignoredByRuleID != 0 {
			res.Ignored++
		}
		// History capture: if the on-disk hash isn't already in our version log
		// for this file, ingest it as scan-import. Catches first-discovery and
		// out-of-band edits between scans without losing anything. An ignored
		// file gets no history: it is a copy, and its history is the original's.
		if fileID > 0 && ignoredByRuleID == 0 {
			existing, err := s.FindVersionByHash(fileID, hash)
			if err == nil && existing == nil {
				if data, err := os.ReadFile(path); err == nil {
					_, _ = s.AppendVersion(fileID, data, VersionSourceScanImport, "", "")
				}
			}
		}
		res.Scanned++
		switch outcome {
		case upsertAdded:
			res.Added++
		case upsertUpdated:
			res.Updated++
		case upsertUnchanged:
			res.Unchanged++
		}
	}

	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			if skipPaths[path] {
				return fs.SkipDir
			}
			return nil
		}
		visitFile(path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Mark rows whose underlying file (in either variant) no longer exists.
	rows, err := s.db.Query("SELECT id, path, enabled FROM tracked_files WHERE status != 'missing'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type stale struct {
		id   int64
		path string
		en   bool
	}
	var candidates []stale
	for rows.Next() {
		var st stale
		var en int
		if err := rows.Scan(&st.id, &st.path, &en); err != nil {
			return nil, err
		}
		st.en = en == 1
		candidates = append(candidates, st)
	}
	for _, c := range candidates {
		if seen[c.path] {
			continue
		}
		if _, err := os.Stat(c.path); err == nil {
			continue
		}
		if _, err := os.Stat(c.path + ".disabled"); err == nil {
			continue
		}
		if _, err := s.db.Exec("UPDATE tracked_files SET status='missing', updated_at=? WHERE id=?", now(), c.id); err != nil {
			// The row still says 'present'. Reporting it as Missing would
			// claim a write that did not happen -- the same lie as dropping a
			// failed file, in the other direction.
			res.fail(c.path, scanStageMarkMissing, err)
			continue
		}
		res.Missing++
	}

	return res, nil
}

// upsertTrackedFile inserts or updates the row for `path` and returns
// (id, added, err). `added` is true when the row was newly inserted.
// upsertTrackedFile writes the row for one visited file and reports which of
// the three outcomes it was. The stored fs_hash is read back in the same query
// as the id so the comparison costs no extra round trip: it is the only thing
// that distinguishes an edit from a re-scan of an untouched file.
func (s *Store) upsertTrackedFile(path, scope, agentSlug string, enabled bool, hash string, size, mtime int64) (int64, upsertOutcome, error) {
	var id int64
	var storedHash string
	err := s.db.QueryRow("SELECT id, COALESCE(fs_hash,'') FROM tracked_files WHERE path = ?", path).Scan(&id, &storedHash)
	if err == nil {
		// The row is rewritten either way: scope, agent_slug and enabled can
		// change without the content changing, and last_scanned_at has to
		// advance on every pass. Only the reported outcome turns on the hash.
		_, err := s.db.Exec(`
			UPDATE tracked_files
			SET scope=?, agent_slug=?, enabled=?, fs_hash=?, size=?, mtime=?, last_scanned_at=?, status='present', updated_at=?
			WHERE id=?`,
			scope, nullIfEmpty(agentSlug), boolToInt(enabled), hash, size, mtime, now(), now(), id)
		if err != nil {
			return id, upsertUnchanged, err
		}
		if storedHash == hash {
			return id, upsertUnchanged, nil
		}
		return id, upsertUpdated, nil
	}
	res, err := s.db.Exec(`
		INSERT INTO tracked_files (path, scope, agent_slug, enabled, fs_hash, size, mtime, last_scanned_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'present', ?, ?)`,
		path, scope, nullIfEmpty(agentSlug), boolToInt(enabled), hash, size, mtime, now(), now(), now())
	if err != nil {
		return 0, upsertUnchanged, err
	}
	id, _ = res.LastInsertId()
	return id, upsertAdded, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListTrackedFiles returns the rows no ignore rule has claimed, optionally
// filtered. Empty filters = all. It is what context resolution, runner seeding
// and prompt import read, none of which want a worktree's copy of a file.
func (s *Store) ListTrackedFiles(scope, agentSlug string) ([]TrackedFile, error) {
	return s.listTrackedFiles(scope, agentSlug, false)
}

// ListTrackedFilesIncludingIgnored is ListTrackedFiles plus the ignored rows.
func (s *Store) ListTrackedFilesIncludingIgnored(scope, agentSlug string) ([]TrackedFile, error) {
	return s.listTrackedFiles(scope, agentSlug, true)
}

func (s *Store) listTrackedFiles(scope, agentSlug string, includeIgnored bool) ([]TrackedFile, error) {
	q := "SELECT id, path, scope, COALESCE(agent_slug,''), enabled, COALESCE(fs_hash,''), COALESCE(size,0), COALESCE(mtime,0), COALESCE(last_scanned_at,0), status, COALESCE(ignored_by_rule_id,0), created_at, updated_at FROM tracked_files WHERE 1=1"
	var args []any
	if !includeIgnored {
		q += " AND ignored_by_rule_id IS NULL"
	}
	if scope != "" {
		q += " AND scope = ?"
		args = append(args, scope)
	}
	if agentSlug != "" {
		q += " AND agent_slug = ?"
		args = append(args, agentSlug)
	}
	q += " ORDER BY scope, path"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackedFile
	for rows.Next() {
		var f TrackedFile
		var en int
		if err := rows.Scan(&f.ID, &f.Path, &f.Scope, &f.AgentSlug, &en, &f.FSHash, &f.Size, &f.MTime, &f.LastScannedAt, &f.Status, &f.IgnoredByRuleID, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Enabled = en == 1
		out = append(out, f)
	}
	return out, nil
}

// GetTrackedFile fetches a single row by ID.
func (s *Store) GetTrackedFile(id int64) (*TrackedFile, error) {
	var f TrackedFile
	var en int
	err := s.db.QueryRow("SELECT id, path, scope, COALESCE(agent_slug,''), enabled, COALESCE(fs_hash,''), COALESCE(size,0), COALESCE(mtime,0), COALESCE(last_scanned_at,0), status, COALESCE(ignored_by_rule_id,0), created_at, updated_at FROM tracked_files WHERE id=?", id).
		Scan(&f.ID, &f.Path, &f.Scope, &f.AgentSlug, &en, &f.FSHash, &f.Size, &f.MTime, &f.LastScannedAt, &f.Status, &f.IgnoredByRuleID, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	f.Enabled = en == 1
	return &f, nil
}

// DiskPath returns the on-disk path (canonical or .disabled variant) for a row.
func (f *TrackedFile) DiskPath() string {
	if f.Enabled {
		return f.Path
	}
	return f.Path + ".disabled"
}

// ReadTrackedFile returns the on-disk content for a tracked file.
func (s *Store) ReadTrackedFile(id int64) (*TrackedFile, []byte, error) {
	f, err := s.GetTrackedFile(id)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(f.DiskPath())
	if err != nil {
		return f, nil, err
	}
	return f, data, nil
}

// WriteTrackedFile writes new content to disk and updates the row's hash/size/mtime.
// Use WriteTrackedFileVersioned for UI-attributed writes that should appear in history.
func (s *Store) WriteTrackedFile(id int64, content []byte) (*TrackedFile, error) {
	f, _, err := s.WriteTrackedFileVersioned(id, content, VersionSourceUISave, "", "")
	return f, err
}

// WriteTrackedFileVersioned writes new content to disk and ensures full history
// is captured. Before overwriting, if the on-disk content's hash differs from
// the most recent recorded version, that pre-existing content is appended as a
// scan-import version — so a user's out-of-band edit is never silently lost.
// After writing, the new content is appended as a version tagged with `source`.
//
// machineID and note are passed straight through to the version row (use ""
// for UI saves).
func (s *Store) WriteTrackedFileVersioned(id int64, content []byte, source, machineID, note string) (*TrackedFile, *TrackedFileVersion, error) {
	f, err := s.GetTrackedFile(id)
	if err != nil {
		return nil, nil, err
	}

	if err := s.captureExistingIfNew(f); err != nil {
		return nil, nil, fmt.Errorf("capture pre-existing: %w", err)
	}

	diskPath := f.DiskPath()
	tmp := diskPath + ".tmp"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		return nil, nil, err
	}
	if err := os.Rename(tmp, diskPath); err != nil {
		os.Remove(tmp)
		return nil, nil, err
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	info, _ := os.Stat(diskPath)
	var size, mtime int64
	if info != nil {
		size = info.Size()
		mtime = info.ModTime().Unix()
	}
	if _, err := s.db.Exec("UPDATE tracked_files SET fs_hash=?, size=?, mtime=?, last_scanned_at=?, status='present', updated_at=? WHERE id=?",
		hash, size, mtime, now(), now(), id); err != nil {
		return nil, nil, err
	}

	v, err := s.AppendVersion(id, content, source, machineID, note)
	if err != nil {
		return nil, nil, fmt.Errorf("append version: %w", err)
	}
	updated, err := s.GetTrackedFile(id)
	if err != nil {
		return nil, nil, err
	}
	return updated, v, nil
}

// captureExistingIfNew reads the current on-disk bytes for f and, if their
// hash isn't already represented in tracked_file_versions for this file,
// appends them as a scan-import version. This is the safety net that makes
// every overwrite non-destructive: even if the bridge missed a manual edit
// between our last knowledge and now, that content lands in history before
// we clobber it.
//
// Missing files and read errors are swallowed: there's nothing to capture.
func (s *Store) captureExistingIfNew(f *TrackedFile) error {
	data, err := os.ReadFile(f.DiskPath())
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	existing, err := s.FindVersionByHash(f.ID, sha)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	_, err = s.AppendVersion(f.ID, data, VersionSourceScanImport, "", "captured before overwrite")
	return err
}

// SetTrackedFileEnabled renames on disk (canonical ↔ .disabled) and updates the row.
func (s *Store) SetTrackedFileEnabled(id int64, enable bool) (*TrackedFile, error) {
	f, err := s.GetTrackedFile(id)
	if err != nil {
		return nil, err
	}
	if f.Enabled == enable {
		return f, nil
	}
	from := f.DiskPath()
	to := f.Path
	if !enable {
		to = f.Path + ".disabled"
	}
	if _, err := os.Stat(from); err != nil {
		return nil, fmt.Errorf("source file missing: %s", from)
	}
	if err := os.Rename(from, to); err != nil {
		return nil, err
	}
	_, err = s.db.Exec("UPDATE tracked_files SET enabled=?, updated_at=? WHERE id=?", boolToInt(enable), now(), id)
	if err != nil {
		return nil, err
	}
	return s.GetTrackedFile(id)
}
