// Package skillstore is the registry of Claude Code / Agent SKILL.md skills,
// ingested from upstream git repositories or from directories on this host,
// with their installations into the install directory and the per-machine
// seeding of installed skill files to runners.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage: each SKILL.md's text and the bytes of every seed file
// version are rows there. A skill's sibling files are not. They stay on disk
// under <data directory>/skills/<id>/, cloned sources under
// <data directory>/repos/<name>/, and an install copies a skill into the
// install directory. The server provisions, migrates and opens the module, then
// hands the pool and both directories to New.
package skillstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// ErrNotFound is returned when a row lookup misses.
var ErrNotFound = errors.New("not found")

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "skills", Role: "skills_module", Migrations: migrations}, nil
}

// Store is the skill registry: its rows in Postgres and its files on disk.
type Store struct {
	db         *sql.DB
	dataDir    string
	installDir string // where installed skills are copied (e.g. ~/.claude/skills)
}

// New returns the store on db, the module's pool from postgresstorage.Open.
// dataDirectory holds the skill files (skills/<id>/) and cloned sources
// (repos/<name>/), and is made with both subfolders if missing; an install
// copies a skill into installDirectory. Neither may be empty: the server's
// settings name both, and the package guesses neither.
func New(db *sql.DB, dataDirectory, installDirectory string) (*Store, error) {
	if db == nil {
		return nil, errors.New("skillstore: no database pool")
	}
	if strings.TrimSpace(dataDirectory) == "" {
		return nil, errors.New("skillstore: the data directory is empty; it must name where skill files and cloned sources are kept")
	}
	if strings.TrimSpace(installDirectory) == "" {
		return nil, errors.New("skillstore: the install directory is empty; it must name where installed skills are copied")
	}
	for _, sub := range []string{"", "skills", "repos"} {
		if err := os.MkdirAll(filepath.Join(dataDirectory, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", filepath.Join(dataDirectory, sub), err)
		}
	}
	return &Store{db: db, dataDir: dataDirectory, installDir: installDirectory}, nil
}

func (s *Store) DB() *sql.DB        { return s.db }
func (s *Store) DataDir() string    { return s.dataDir }
func (s *Store) InstallDir() string { return s.installDir }

// SkillsDir is where ingested skill files live.
func (s *Store) SkillsDir() string { return filepath.Join(s.dataDir, "skills") }

// ReposDir is where cloned source repos live.
func (s *Store) ReposDir() string { return filepath.Join(s.dataDir, "repos") }

// SkillDir is the directory where a given skill's files live.
func (s *Store) SkillDir(skillID int64) string {
	return filepath.Join(s.SkillsDir(), fmt.Sprintf("%d", skillID))
}

func now() int64 { return time.Now().Unix() }

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ============================================
// SOURCES
// ============================================

// SourceKind says where a source's skills come from, and therefore how Sync
// obtains them.
type SourceKind string

const (
	// SourceKindGit clones or fetches a remote repo into the store's data dir.
	SourceKindGit SourceKind = "git"
	// SourceKindLocal reads a directory that already exists on this host, for
	// skills authored here rather than pulled from upstream. Nothing is cloned
	// and there is no commit to record.
	//
	// LocalPath must not be, or sit inside, the install dir: Install wipes and
	// rewrites its target, so a source rooted there would have the store
	// ingesting its own output and destroying the original in the process.
	SourceKindLocal SourceKind = "local"
)

type Source struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Kind selects the sync strategy; empty is treated as git for the sake of
	// rows written before local sources existed.
	Kind SourceKind `json:"kind"`
	// LocalPath is the directory to read for a local source, ignored for git.
	LocalPath string `json:"local_path,omitempty"`
	// RepoURL is the clone URL for a git source, empty for a local one.
	RepoURL        string `json:"repo_url"`
	Branch         string `json:"branch"`
	Subpath        string `json:"subpath"`
	TrustTier      string `json:"trust_tier"`
	License        string `json:"license,omitempty"`
	LastSyncedAt   int64  `json:"last_synced_at,omitempty"`
	LastCommit     string `json:"last_commit,omitempty"`
	LastSkillCount int    `json:"last_skill_count"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

func (s *Store) UpsertSource(ctx context.Context, src *Source) error {
	if src.Kind == "" {
		src.Kind = SourceKindGit
	}
	if src.Branch == "" {
		src.Branch = "main"
	}
	if src.TrustTier == "" {
		src.TrustTier = "community"
	}
	if src.CreatedAt == 0 {
		src.CreatedAt = now()
	}
	src.UpdatedAt = now()
	return s.db.QueryRowContext(ctx, `
		INSERT INTO sources (name, kind, local_path, repo_url, branch, subpath, trust_tier, license, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (name) DO UPDATE SET
			kind=excluded.kind, local_path=excluded.local_path,
			repo_url=excluded.repo_url, branch=excluded.branch, subpath=excluded.subpath,
			trust_tier=excluded.trust_tier, license=excluded.license, enabled=excluded.enabled,
			updated_at=excluded.updated_at
		RETURNING id
	`, src.Name, string(src.Kind), src.LocalPath, src.RepoURL, src.Branch, src.Subpath, src.TrustTier, src.License,
		src.Enabled, src.CreatedAt, src.UpdatedAt).Scan(&src.ID)
}

func (s *Store) GetSource(ctx context.Context, id int64) (*Source, error) {
	return scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id=$1`, id))
}

func (s *Store) GetSourceByName(ctx context.Context, name string) (*Source, error) {
	return scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE name=$1`, name))
}

// ListSources lists every source by name, in byte order as the standalone
// SQLite store sorted them (COLLATE "C"), not the database's locale order.
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY name COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE id=$1`, id)
	return err
}

func (s *Store) MarkSourceSynced(ctx context.Context, id int64, commit string, skillCount int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sources SET last_synced_at=$1, last_commit=$2, last_skill_count=$3, updated_at=$4 WHERE id=$5`,
		now(), commit, skillCount, now(), id)
	return err
}

// sourceCols and scanSource must list the same columns in the same order —
// the compiler cannot check that, so change them together.
const sourceCols = `id, name, COALESCE(kind,'git'), COALESCE(local_path,''), repo_url, branch, subpath, trust_tier, COALESCE(license,''), COALESCE(last_synced_at,0), COALESCE(last_commit,''), last_skill_count, enabled, created_at, updated_at`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	src := &Source{}
	var kind string
	err := row.Scan(&src.ID, &src.Name, &kind, &src.LocalPath, &src.RepoURL, &src.Branch, &src.Subpath, &src.TrustTier,
		&src.License, &src.LastSyncedAt, &src.LastCommit, &src.LastSkillCount, &src.Enabled, &src.CreatedAt, &src.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.Kind = SourceKind(kind)
	return src, nil
}

// ============================================
// SKILLS
// ============================================

type Skill struct {
	ID           int64    `json:"id"`
	SourceID     int64    `json:"source_id"`
	SourceName   string   `json:"source_name,omitempty"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name,omitempty"`
	Description  string   `json:"description,omitempty"`
	SkillMD      string   `json:"skill_md,omitempty"`
	SourcePath   string   `json:"source_path"`
	SourceCommit string   `json:"source_commit,omitempty"`
	ContentHash  string   `json:"content_hash"`
	FileCount    int      `json:"file_count"`
	Tags         []string `json:"tags,omitempty"`
	Enabled      bool     `json:"enabled"`
	Installed    bool     `json:"installed"`
	InstalledAt  int64    `json:"installed_at,omitempty"`
	InstallPath  string   `json:"install_path,omitempty"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

// UpsertSkill inserts or updates by (source_id, name). On insert it sets the
// skill's ID. Returns (inserted bool, err).
func (s *Store) UpsertSkill(ctx context.Context, sk *Skill) (bool, error) {
	if sk.CreatedAt == 0 {
		sk.CreatedAt = now()
	}
	sk.UpdatedAt = now()
	sk.ContentHash = hashBytes([]byte(sk.SkillMD))
	tagsJSON := encodeTags(sk.Tags)

	// Try update first
	err := s.db.QueryRowContext(ctx, `
		UPDATE skills SET display_name=$1, description=$2, skill_md=$3, source_path=$4, source_commit=$5,
			content_hash=$6, file_count=$7, tags=$8, enabled=$9, updated_at=$10
		WHERE source_id=$11 AND name=$12
		RETURNING id
	`, sk.DisplayName, sk.Description, sk.SkillMD, sk.SourcePath, sk.SourceCommit,
		sk.ContentHash, sk.FileCount, tagsJSON, sk.Enabled, sk.UpdatedAt,
		sk.SourceID, sk.Name).Scan(&sk.ID)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO skills (source_id, name, display_name, description, skill_md, source_path, source_commit,
			content_hash, file_count, tags, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`, sk.SourceID, sk.Name, sk.DisplayName, sk.Description, sk.SkillMD, sk.SourcePath, sk.SourceCommit,
		sk.ContentHash, sk.FileCount, tagsJSON, sk.Enabled, sk.CreatedAt, sk.UpdatedAt).Scan(&sk.ID)
	if err != nil {
		return false, wrapForeignKeyViolation(err, "source", sk.SourceID)
	}
	return true, nil
}

const skillCols = `s.id, s.source_id, src.name, s.name, COALESCE(s.display_name,''), COALESCE(s.description,''),
	s.skill_md, s.source_path, COALESCE(s.source_commit,''), s.content_hash, s.file_count,
	COALESCE(s.tags,''), s.enabled, s.created_at, s.updated_at,
	COALESCE(i.installed_at, 0), COALESCE(i.target_path, '')`

const skillFrom = ` FROM skills s
	JOIN sources src ON src.id = s.source_id
	LEFT JOIN installations i ON i.skill_id = s.id`

func scanSkill(row interface{ Scan(...any) error }, includeBody bool) (*Skill, error) {
	sk := &Skill{}
	var tags string
	err := row.Scan(&sk.ID, &sk.SourceID, &sk.SourceName, &sk.Name, &sk.DisplayName, &sk.Description,
		&sk.SkillMD, &sk.SourcePath, &sk.SourceCommit, &sk.ContentHash, &sk.FileCount,
		&tags, &sk.Enabled, &sk.CreatedAt, &sk.UpdatedAt, &sk.InstalledAt, &sk.InstallPath)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sk.Installed = sk.InstalledAt > 0
	sk.Tags = decodeTags(tags)
	if !includeBody {
		sk.SkillMD = ""
	}
	return sk, nil
}

// ListSkillsFilter narrows listSkills.
type ListSkillsFilter struct {
	SourceID      int64
	SourceName    string
	Query         string // substring match on name/description
	Tag           string // matches if the tags JSON array contains this string
	InstalledOnly bool
	Limit         int
}

// ListSkills lists the skills f selects, by source name and skill name in byte
// order as the standalone SQLite store sorted them (COLLATE "C").
//
// Query and Tag match as SQLite's LIKE did: case-insensitive, with % and _ in
// the caller's text acting as wildcards and no escape character (ESCAPE ”).
// One difference remains: ILIKE folds the case of every letter, where SQLite's
// LIKE folded ASCII letters only.
func (s *Store) ListSkills(ctx context.Context, f ListSkillsFilter) ([]Skill, error) {
	q := `SELECT ` + skillCols + skillFrom + ` WHERE 1=1`
	var args []any
	next := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.SourceID > 0 {
		q += ` AND s.source_id = ` + next(f.SourceID)
	}
	if f.SourceName != "" {
		q += ` AND src.name = ` + next(f.SourceName)
	}
	if f.Query != "" {
		like := next("%" + f.Query + "%")
		q += ` AND (s.name ILIKE ` + like + ` ESCAPE '' OR s.description ILIKE ` + like + ` ESCAPE '')`
	}
	if f.Tag != "" {
		// Cheap match on the JSON array; entries are quoted strings.
		q += ` AND s.tags ILIKE ` + next(`%"`+f.Tag+`"%`) + ` ESCAPE ''`
	}
	if f.InstalledOnly {
		q += ` AND i.skill_id IS NOT NULL`
	}
	q += ` ORDER BY src.name COLLATE "C", s.name COLLATE "C"`
	if f.Limit > 0 {
		q += ` LIMIT ` + next(f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkill(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *sk)
	}
	return out, rows.Err()
}

func (s *Store) GetSkill(ctx context.Context, id int64) (*Skill, error) {
	return scanSkill(s.db.QueryRowContext(ctx, `SELECT `+skillCols+skillFrom+` WHERE s.id=$1`, id), true)
}

func (s *Store) DeleteSkill(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id=$1`, id)
	return err
}

// DeleteSkillsForSourceExcept removes skills in source_id that are NOT in the keep list.
// Used after a sync to prune skills that disappeared upstream.
func (s *Store) DeleteSkillsForSourceExcept(ctx context.Context, sourceID int64, keepIDs []int64) (int, error) {
	q := `DELETE FROM skills WHERE source_id=$1`
	args := []any{sourceID}
	if len(keepIDs) > 0 {
		placeholders := make([]string, 0, len(keepIDs))
		for _, id := range keepIDs {
			args = append(args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		q += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
