package skillstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SeedFileVersion is one historical content snapshot of an installed skill
// file. Mirrors agent-store's TrackedFileVersion shape so the runner can
// treat the two sources uniformly.
type SeedFileVersion struct {
	ID        int64  `json:"id"`
	SkillID   int64  `json:"skill_id"`
	Relpath   string `json:"relpath"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Source    string `json:"source"`
	MachineID string `json:"machine_id,omitempty"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

const (
	SeedSourceIngest      = "ingest"
	SeedSourceRunnerDrift = "runner-drift"
	SeedSourceScanImport  = "scan-import"
)

// AppendSeedVersion records bytes for one (skill, relpath) pair. If the most
// recent version for that pair already has the same sha, nothing is inserted
// and the existing row is returned — keeps history append-only without
// inserting duplicates on every reconcile pass.
func (s *Store) AppendSeedVersion(ctx context.Context, skillID int64, relpath string, content []byte, source, machineID, note string) (*SeedFileVersion, error) {
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	var lastID int64
	var lastSHA string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sha256 FROM seed_file_versions
		WHERE skill_id=$1 AND relpath=$2 ORDER BY created_at DESC, id DESC LIMIT 1
	`, skillID, relpath).Scan(&lastID, &lastSHA)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query last version: %w", err)
	}
	if err == nil && lastSHA == sha {
		return s.GetSeedVersion(ctx, lastID)
	}

	var machinePtr any
	if machineID != "" {
		machinePtr = machineID
	}
	var notePtr any
	if note != "" {
		notePtr = note
	}
	if content == nil {
		// An empty file is zero bytes, never NULL: the column is NOT NULL.
		content = []byte{}
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO seed_file_versions
			(skill_id, relpath, sha256, size, content_bytes, source, machine_id, note, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`, skillID, relpath, sha, int64(len(content)), content, source, machinePtr, notePtr, now()).Scan(&id)
	if err != nil {
		if wrapped := wrapForeignKeyViolation(err, "skill", skillID); wrapped != err {
			return nil, wrapped
		}
		return nil, fmt.Errorf("insert version: %w", err)
	}
	return s.GetSeedVersion(ctx, id)
}

// GetSeedVersion fetches one version row (without content body).
func (s *Store) GetSeedVersion(ctx context.Context, id int64) (*SeedFileVersion, error) {
	v := &SeedFileVersion{}
	var machine, note sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, skill_id, relpath, sha256, size, source, machine_id, note, created_at
		FROM seed_file_versions WHERE id=$1
	`, id).Scan(&v.ID, &v.SkillID, &v.Relpath, &v.SHA256, &v.Size, &v.Source, &machine, &note, &v.CreatedAt)
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

// ReadSeedVersionContent returns the bytes for one version.
func (s *Store) ReadSeedVersionContent(ctx context.Context, id int64) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT content_bytes FROM seed_file_versions WHERE id=$1`, id).Scan(&data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// LatestSeedVersion returns the most recent version for (skillID, relpath),
// or nil (no error) if none exist.
func (s *Store) LatestSeedVersion(ctx context.Context, skillID int64, relpath string) (*SeedFileVersion, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM seed_file_versions
		WHERE skill_id=$1 AND relpath=$2 ORDER BY created_at DESC, id DESC LIMIT 1
	`, skillID, relpath).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetSeedVersion(ctx, id)
}

// ============================================
// MACHINE SEED PROFILE
// ============================================

type SeedProfile struct {
	MachineID string `json:"machine_id"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// EnsureSeedProfile creates an enabled-by-default profile if none exists.
func (s *Store) EnsureSeedProfile(ctx context.Context, machineID string) (*SeedProfile, error) {
	if machineID == "" {
		return nil, fmt.Errorf("machine_id required")
	}
	prof, err := s.GetSeedProfile(ctx, machineID)
	if err == nil {
		return prof, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_seed_profiles (machine_id, enabled, created_at, updated_at)
		VALUES ($1, true, $2, $3)
	`, machineID, now(), now()); err != nil {
		return nil, err
	}
	return s.GetSeedProfile(ctx, machineID)
}

func (s *Store) GetSeedProfile(ctx context.Context, machineID string) (*SeedProfile, error) {
	prof := &SeedProfile{MachineID: machineID}
	err := s.db.QueryRowContext(ctx, `
		SELECT enabled, created_at, updated_at FROM machine_seed_profiles WHERE machine_id=$1
	`, machineID).Scan(&prof.Enabled, &prof.CreatedAt, &prof.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return prof, nil
}

func (s *Store) SetSeedProfileEnabled(ctx context.Context, machineID string, enabled bool) (*SeedProfile, error) {
	if _, err := s.EnsureSeedProfile(ctx, machineID); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE machine_seed_profiles SET enabled=$1, updated_at=$2 WHERE machine_id=$3`,
		enabled, now(), machineID); err != nil {
		return nil, err
	}
	return s.GetSeedProfile(ctx, machineID)
}

// ============================================
// SEED STATE
// ============================================

type SeedState struct {
	MachineID           string `json:"machine_id"`
	SkillID             int64  `json:"skill_id"`
	Relpath             string `json:"relpath"`
	ObservedSHA         string `json:"observed_sha,omitempty"`
	ObservedAt          int64  `json:"observed_at,omitempty"`
	LastPushedVersionID int64  `json:"last_pushed_version_id,omitempty"`
	LastPushedAt        int64  `json:"last_pushed_at,omitempty"`
}

func (s *Store) RecordObservedHash(ctx context.Context, machineID string, skillID int64, relpath, sha string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO machine_seed_state (machine_id, skill_id, relpath, observed_sha, observed_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (machine_id, skill_id, relpath) DO UPDATE SET
			observed_sha=excluded.observed_sha, observed_at=excluded.observed_at
	`, machineID, skillID, relpath, sha, now())
	return wrapForeignKeyViolation(err, "skill", skillID)
}

// ============================================
// MANIFEST
// ============================================

// SeedManifestEntry is one entry the runner reconciles against.
type SeedManifestEntry struct {
	SkillID     int64  `json:"skill_id"`
	Relpath     string `json:"relpath"`
	InstallPath string `json:"install_path"` // $HOME-relative
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	VersionID   int64  `json:"version_id"`
}

// SeedManifest is the per-machine bundle.
type SeedManifest struct {
	MachineID string              `json:"machine_id"`
	Enabled   bool                `json:"enabled"`
	Entries   []SeedManifestEntry `json:"entries"`
	BuiltAt   int64               `json:"built_at"`
}

// BuildSeedManifest enumerates every file in every installed skill and ensures
// each (skillID, relpath) has at least one version row recording its current
// canonical bytes. Returns the manifest the named runner should reconcile to.
//
// If the machine's profile has enabled=false, the manifest is empty (the
// runner removes any previously-seeded files).
func (s *Store) BuildSeedManifest(ctx context.Context, machineID string) (*SeedManifest, error) {
	prof, err := s.EnsureSeedProfile(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("ensure profile: %w", err)
	}
	mf := &SeedManifest{
		MachineID: machineID,
		Enabled:   prof.Enabled,
		BuiltAt:   now(),
	}
	if !prof.Enabled {
		return mf, nil
	}

	insts, err := s.ListInstallations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home: %w", err)
	}

	for _, inst := range insts {
		entries, err := s.snapshotInstallation(ctx, inst, home)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", inst.SkillName, err)
		}
		mf.Entries = append(mf.Entries, entries...)
	}
	return mf, nil
}

// snapshotInstallation walks one installed skill's directory, ensures every
// file has a current version row, and returns manifest entries pointing at
// the latest version per file. Files outside $HOME are skipped (refusing to
// produce an unsafe install_path is better than reporting one we can't
// rewrite).
func (s *Store) snapshotInstallation(ctx context.Context, inst Installation, home string) ([]SeedManifestEntry, error) {
	if !strings.HasPrefix(inst.TargetPath, home+string(os.PathSeparator)) {
		return nil, nil
	}
	var out []SeedManifestEntry
	err := filepath.WalkDir(inst.TargetPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(inst.TargetPath, path)
		if err != nil {
			return err
		}
		v, err := s.AppendSeedVersion(ctx, inst.SkillID, rel, data, SeedSourceIngest, "", "")
		if err != nil {
			return err
		}
		install, ok := installPathRelative(path, home)
		if !ok {
			return nil
		}
		out = append(out, SeedManifestEntry{
			SkillID:     inst.SkillID,
			Relpath:     rel,
			InstallPath: install,
			SHA256:      v.SHA256,
			Size:        v.Size,
			VersionID:   v.ID,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// installPathRelative rewrites a bridge-host absolute path into a path
// relative to $HOME. Refuses paths outside $HOME or containing "..".
func installPathRelative(path, home string) (string, bool) {
	prefix := home + string(os.PathSeparator)
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(path, prefix)
	if rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	return rel, true
}
