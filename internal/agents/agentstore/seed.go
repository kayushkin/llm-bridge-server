package agentstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ============================================
// TRACKED FILE VERSIONS
// ============================================

// TrackedFileVersion is one historical content snapshot of a tracked file.
type TrackedFileVersion struct {
	ID            int64  `json:"id"`
	TrackedFileID int64  `json:"tracked_file_id"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	Source        string `json:"source"` // ui-save, scan-import, runner-drift, seed
	MachineID     string `json:"machine_id,omitempty"`
	Note          string `json:"note,omitempty"`
	CreatedAt     int64  `json:"created_at"`
}

// VersionSource constants.
const (
	VersionSourceUISave      = "ui-save"
	VersionSourceScanImport  = "scan-import"
	VersionSourceRunnerDrift = "runner-drift"
	VersionSourceSeed        = "seed"
)

// AppendVersion writes a new version row. content is hashed; if the most
// recent version for this tracked_file already has the same sha, no row is
// inserted and the existing version is returned. This keeps history append-only
// without filling the DB with duplicate rows when a save is a no-op.
func (s *Store) AppendVersion(trackedFileID int64, content []byte, source, machineID, note string) (*TrackedFileVersion, error) {
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	// De-dupe against the most recent version.
	var lastID int64
	var lastSHA string
	err := s.db.QueryRow(`
		SELECT id, sha256 FROM tracked_file_versions
		WHERE tracked_file_id=? ORDER BY created_at DESC, id DESC LIMIT 1
	`, trackedFileID).Scan(&lastID, &lastSHA)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query last version: %w", err)
	}
	if err == nil && lastSHA == sha {
		return s.GetVersion(lastID)
	}

	var machinePtr any
	if machineID != "" {
		machinePtr = machineID
	}
	var notePtr any
	if note != "" {
		notePtr = note
	}
	res, err := s.db.Exec(`
		INSERT INTO tracked_file_versions
			(tracked_file_id, sha256, size, content_bytes, source, machine_id, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, trackedFileID, sha, int64(len(content)), content, source, machinePtr, notePtr, now())
	if err != nil {
		return nil, fmt.Errorf("insert version: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetVersion(id)
}

// GetVersion fetches a single version row (without content for list ops; use
// ReadVersionContent for the bytes).
func (s *Store) GetVersion(id int64) (*TrackedFileVersion, error) {
	v := &TrackedFileVersion{}
	var machine, note sql.NullString
	err := s.db.QueryRow(`
		SELECT id, tracked_file_id, sha256, size, source, machine_id, note, created_at
		FROM tracked_file_versions WHERE id=?
	`, id).Scan(&v.ID, &v.TrackedFileID, &v.SHA256, &v.Size, &v.Source, &machine, &note, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	if machine.Valid {
		v.MachineID = machine.String
	}
	if note.Valid {
		v.Note = note.String
	}
	return v, nil
}

// ReadVersionContent returns the stored bytes for a version.
func (s *Store) ReadVersionContent(id int64) ([]byte, error) {
	var content []byte
	err := s.db.QueryRow(`SELECT content_bytes FROM tracked_file_versions WHERE id=?`, id).Scan(&content)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// ListVersions returns every version of a tracked file, newest first, without
// content bodies.
func (s *Store) ListVersions(trackedFileID int64) ([]TrackedFileVersion, error) {
	rows, err := s.db.Query(`
		SELECT id, tracked_file_id, sha256, size, source, COALESCE(machine_id,''), COALESCE(note,''), created_at
		FROM tracked_file_versions WHERE tracked_file_id=?
		ORDER BY created_at DESC, id DESC
	`, trackedFileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackedFileVersion
	for rows.Next() {
		var v TrackedFileVersion
		if err := rows.Scan(&v.ID, &v.TrackedFileID, &v.SHA256, &v.Size, &v.Source, &v.MachineID, &v.Note, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LatestVersion returns the most recent version for a tracked file, or nil
// (no error) if there is none yet.
//
// ⚠️ Newest is not the same as what is on disk. Scan's history guard searches
// the whole version log, so reverting a file to content it has held before
// appends nothing and leaves the newest row holding the superseded content.
// To ask what a file currently holds, use FindVersionByHash with the row's
// FSHash. This method has no caller today; BuildSeedManifest used to be one
// and shipped the wrong bytes to every runner because of exactly that.
func (s *Store) LatestVersion(trackedFileID int64) (*TrackedFileVersion, error) {
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM tracked_file_versions
		WHERE tracked_file_id=? ORDER BY created_at DESC, id DESC LIMIT 1
	`, trackedFileID).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetVersion(id)
}

// FindVersionByHash returns the version with the given sha256 for a tracked file,
// or nil (no error) if there is no such version.
func (s *Store) FindVersionByHash(trackedFileID int64, sha string) (*TrackedFileVersion, error) {
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM tracked_file_versions
		WHERE tracked_file_id=? AND sha256=? ORDER BY created_at DESC, id DESC LIMIT 1
	`, trackedFileID, sha).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetVersion(id)
}

// ============================================
// MACHINE SEED PROFILES
// ============================================

// MachineSeedProfile is one runner's opt-in scope set.
type MachineSeedProfile struct {
	MachineID string   `json:"machine_id"`
	Scopes    []string `json:"scopes"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// DefaultSeedScopes is the scope set a fresh machine starts with. "project"
// is intentionally excluded — project-scoped paths point to literal bridge
// host directories that don't exist on a runner.
var DefaultSeedScopes = []string{"global", "subagent", "memory", "command", "inber"}

// EnsureSeedProfile creates a default profile row for a machine if none exists,
// otherwise returns the existing one.
func (s *Store) EnsureSeedProfile(machineID string) (*MachineSeedProfile, error) {
	if machineID == "" {
		return nil, fmt.Errorf("machine_id required")
	}
	prof, err := s.GetSeedProfile(machineID)
	if err == nil {
		return prof, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	scopes, _ := json.Marshal(DefaultSeedScopes)
	if _, err := s.db.Exec(`
		INSERT INTO machine_seed_profiles (machine_id, scopes, created_at, updated_at)
		VALUES (?, ?, ?, ?)
	`, machineID, string(scopes), now(), now()); err != nil {
		return nil, fmt.Errorf("insert profile: %w", err)
	}
	return s.GetSeedProfile(machineID)
}

// GetSeedProfile fetches one machine's profile. Returns sql.ErrNoRows if
// the row doesn't exist.
func (s *Store) GetSeedProfile(machineID string) (*MachineSeedProfile, error) {
	var raw string
	prof := &MachineSeedProfile{MachineID: machineID}
	err := s.db.QueryRow(`
		SELECT scopes, created_at, updated_at FROM machine_seed_profiles WHERE machine_id=?
	`, machineID).Scan(&raw, &prof.CreatedAt, &prof.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &prof.Scopes); err != nil {
		return nil, fmt.Errorf("parse scopes: %w", err)
	}
	return prof, nil
}

// SetSeedProfileScopes replaces the scope set for a machine.
func (s *Store) SetSeedProfileScopes(machineID string, scopes []string) (*MachineSeedProfile, error) {
	if _, err := s.EnsureSeedProfile(machineID); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(scopes)
	if _, err := s.db.Exec(`
		UPDATE machine_seed_profiles SET scopes=?, updated_at=? WHERE machine_id=?
	`, string(encoded), now(), machineID); err != nil {
		return nil, err
	}
	return s.GetSeedProfile(machineID)
}

// ListSeedProfiles returns every machine profile.
func (s *Store) ListSeedProfiles() ([]MachineSeedProfile, error) {
	rows, err := s.db.Query(`
		SELECT machine_id, scopes, created_at, updated_at FROM machine_seed_profiles ORDER BY machine_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineSeedProfile
	for rows.Next() {
		var p MachineSeedProfile
		var raw string
		if err := rows.Scan(&p.MachineID, &raw, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &p.Scopes); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ============================================
// MACHINE SEED STATE
// ============================================

// MachineSeedState is the per-machine, per-file snapshot of "what the runner
// has on disk" as last reported, plus what the bridge most recently pushed.
type MachineSeedState struct {
	MachineID           string `json:"machine_id"`
	TrackedFileID       int64  `json:"tracked_file_id"`
	ObservedSHA         string `json:"observed_sha,omitempty"`
	ObservedAt          int64  `json:"observed_at,omitempty"`
	LastPushedVersionID int64  `json:"last_pushed_version_id,omitempty"`
	LastPushedAt        int64  `json:"last_pushed_at,omitempty"`
}

// RecordObservedHash upserts the runner's currently-observed sha for a file.
func (s *Store) RecordObservedHash(machineID string, trackedFileID int64, sha string) error {
	_, err := s.db.Exec(`
		INSERT INTO machine_seed_state (machine_id, tracked_file_id, observed_sha, observed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(machine_id, tracked_file_id) DO UPDATE SET
			observed_sha=excluded.observed_sha, observed_at=excluded.observed_at
	`, machineID, trackedFileID, sha, now())
	return err
}

// RecordPushedVersion upserts the bridge's record of what version it last
// pushed to a runner.
func (s *Store) RecordPushedVersion(machineID string, trackedFileID int64, versionID int64) error {
	_, err := s.db.Exec(`
		INSERT INTO machine_seed_state (machine_id, tracked_file_id, last_pushed_version_id, last_pushed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(machine_id, tracked_file_id) DO UPDATE SET
			last_pushed_version_id=excluded.last_pushed_version_id,
			last_pushed_at=excluded.last_pushed_at
	`, machineID, trackedFileID, versionID, now())
	return err
}

// GetSeedState fetches the seed state row for one (machine, file) pair.
// Returns sql.ErrNoRows if the runner has never reported on this file.
func (s *Store) GetSeedState(machineID string, trackedFileID int64) (*MachineSeedState, error) {
	st := &MachineSeedState{MachineID: machineID, TrackedFileID: trackedFileID}
	var observed, pushedAt sql.NullInt64
	var sha sql.NullString
	var versionID sql.NullInt64
	err := s.db.QueryRow(`
		SELECT observed_sha, observed_at, last_pushed_version_id, last_pushed_at
		FROM machine_seed_state WHERE machine_id=? AND tracked_file_id=?
	`, machineID, trackedFileID).Scan(&sha, &observed, &versionID, &pushedAt)
	if err != nil {
		return nil, err
	}
	if sha.Valid {
		st.ObservedSHA = sha.String
	}
	if observed.Valid {
		st.ObservedAt = observed.Int64
	}
	if versionID.Valid {
		st.LastPushedVersionID = versionID.Int64
	}
	if pushedAt.Valid {
		st.LastPushedAt = pushedAt.Int64
	}
	return st, nil
}

// ListSeedStateForMachine returns every seed-state row for a machine.
func (s *Store) ListSeedStateForMachine(machineID string) ([]MachineSeedState, error) {
	rows, err := s.db.Query(`
		SELECT tracked_file_id, COALESCE(observed_sha,''), COALESCE(observed_at,0),
		       COALESCE(last_pushed_version_id,0), COALESCE(last_pushed_at,0)
		FROM machine_seed_state WHERE machine_id=?
	`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineSeedState
	for rows.Next() {
		st := MachineSeedState{MachineID: machineID}
		if err := rows.Scan(&st.TrackedFileID, &st.ObservedSHA, &st.ObservedAt, &st.LastPushedVersionID, &st.LastPushedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ============================================
// SEED MANIFEST
// ============================================

// SeedManifestEntry is one row of the manifest a runner reconciles against.
// install_path is $HOME-relative — the runner prepends its own $HOME.
// version_id points at the canonical version a fresh runner should land on.
type SeedManifestEntry struct {
	TrackedFileID int64  `json:"tracked_file_id"`
	InstallPath   string `json:"install_path"` // $HOME-relative, leading "/" stripped
	Scope         string `json:"scope"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	VersionID     int64  `json:"version_id"`
}

// SeedManifest is the full manifest for one machine — exactly the set of
// files that machine's profile says it wants.
type SeedManifest struct {
	MachineID string              `json:"machine_id"`
	Scopes    []string            `json:"scopes"`
	Entries   []SeedManifestEntry `json:"entries"`
	BuiltAt   int64               `json:"built_at"`
}

// BuildSeedManifest assembles the manifest for one machine. It walks every
// enabled, present tracked file whose scope is in the machine's profile and
// whose path can be rewritten to a $HOME-relative install path. Files whose
// on-disk content is not in the version log are skipped — the bridge can't
// push what it hasn't recorded.
//
// Each entry names the version holding the bytes currently on disk, found by
// hash. It deliberately does not use LatestVersion: newest and current are the
// same row for every edit except a revert to content the file has held before,
// and on a revert Scan appends nothing (its guard searches the whole history),
// so the newest row is the superseded content. Resolving by hash makes the
// manifest correct without touching how history is recorded.
func (s *Store) BuildSeedManifest(machineID string) (*SeedManifest, error) {
	prof, err := s.EnsureSeedProfile(machineID)
	if err != nil {
		return nil, fmt.Errorf("ensure profile: %w", err)
	}
	allowed := make(map[string]bool, len(prof.Scopes))
	for _, sc := range prof.Scopes {
		allowed[sc] = true
	}

	files, err := s.ListTrackedFiles("", "")
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}

	mf := &SeedManifest{
		MachineID: machineID,
		Scopes:    prof.Scopes,
		BuiltAt:   now(),
	}

	for _, f := range files {
		if !f.Enabled || f.Status != "present" || !allowed[f.Scope] {
			continue
		}
		install, ok := installPathForBridgeFile(f.Path, home)
		if !ok {
			continue
		}
		if f.FSHash == "" {
			// Never hashed, so there is nothing to match a version against.
			continue
		}
		v, err := s.FindVersionByHash(f.ID, f.FSHash)
		if err != nil {
			return nil, fmt.Errorf("version by disk hash for %d: %w", f.ID, err)
		}
		if v == nil {
			// The bytes on disk were never recorded — skip until a save or
			// scan-import lands them, rather than advertise other content
			// under this path.
			continue
		}
		mf.Entries = append(mf.Entries, SeedManifestEntry{
			TrackedFileID: f.ID,
			InstallPath:   install,
			Scope:         f.Scope,
			SHA256:        v.SHA256,
			Size:          v.Size,
			VersionID:     v.ID,
		})
	}
	return mf, nil
}

// installPathForBridgeFile rewrites a bridge-host absolute path into the
// runner-relative install path (relative to that runner's $HOME). Returns
// ("", false) if the path can't be safely seeded.
func installPathForBridgeFile(path, home string) (string, bool) {
	prefix := home + string(os.PathSeparator)
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(path, prefix)
	if rel == "" {
		return "", false
	}
	// Disallow path traversal — should never trigger under classifyFile, but
	// belt-and-braces given this path will be written inside a remote $HOME.
	if strings.Contains(rel, "..") {
		return "", false
	}
	return rel, true
}
