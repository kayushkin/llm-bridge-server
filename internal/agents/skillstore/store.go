// Package skillstore is the canonical registry for Claude Code / Agent SKILL.md
// skills, ingested from upstream GitHub repos and served on localhost:8301.
package skillstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrNotFound is returned when a row lookup misses.
var ErrNotFound = errors.New("not found")

// DefaultDataDir is ~/.config/skill-store.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "skill-store")
}

// DefaultDBPath is ~/.config/skill-store/skill-store.db.
func DefaultDBPath() string { return filepath.Join(DefaultDataDir(), "skill-store.db") }

// Store is the SQLite-backed skill registry.
type Store struct {
	db         *sql.DB
	dataDir    string
	installDir string // where installed skills are copied (e.g. ~/.claude/skills)
}

// DefaultInstallDir is ~/.claude/skills — where Claude Code picks up
// user-installed skills.
func DefaultInstallDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "skills")
}

// Open opens or creates the store rooted at dataDir. The SQLite DB lives at
// <dataDir>/skill-store.db; skill file payloads live at <dataDir>/skills/<id>/
// and cloned source repos at <dataDir>/repos/<source-name>/.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	for _, sub := range []string{"", "skills", "repos"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	dbPath := filepath.Join(dataDir, "skill-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir, installDir: DefaultInstallDir()}, nil
}

// columnMigration adds one column to an existing table.
type columnMigration struct {
	table      string
	column     string
	definition string
}

// migrate applies the base schema, then every additive column migration.
//
// schema.sql carries only CREATE TABLE IF NOT EXISTS, so a column added there
// alone reaches a fresh database and no other: an existing table is left
// exactly as it was, silently. Any column added after a database first shipped
// must therefore appear BOTH in schema.sql (for new installs) and here (for
// existing ones).
//
// Each migration is checked against the live table rather than applied and
// ignored on error, so a genuine failure still surfaces instead of hiding
// behind an expected "duplicate column name".
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	migrations := []columnMigration{
		{"sources", "kind", "TEXT NOT NULL DEFAULT 'git'"},
		{"sources", "local_path", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, m := range migrations {
		present, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", m.table, m.column, err)
		}
		if present {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m.table, m.column, m.definition)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

// hasColumn reports whether table already has the named column.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notNull, primaryKey int
			name, colType            string
			defaultValue             any
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// SetInstallDir overrides the default install dir (~/.claude/skills). Used in
// tests or to relocate where Claude Code looks for user skills.
func (s *Store) SetInstallDir(dir string) { s.installDir = dir }

func (s *Store) Close() error       { return s.db.Close() }
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

func (s *Store) UpsertSource(src *Source) error {
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
	_, err := s.db.Exec(`
		INSERT INTO sources (name, kind, local_path, repo_url, branch, subpath, trust_tier, license, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind=excluded.kind, local_path=excluded.local_path,
			repo_url=excluded.repo_url, branch=excluded.branch, subpath=excluded.subpath,
			trust_tier=excluded.trust_tier, license=excluded.license, enabled=excluded.enabled,
			updated_at=excluded.updated_at
	`, src.Name, string(src.Kind), src.LocalPath, src.RepoURL, src.Branch, src.Subpath, src.TrustTier, src.License,
		boolToInt(src.Enabled), src.CreatedAt, src.UpdatedAt)
	if err != nil {
		return err
	}
	return s.db.QueryRow(`SELECT id FROM sources WHERE name=?`, src.Name).Scan(&src.ID)
}

func (s *Store) GetSource(id int64) (*Source, error) {
	return scanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

func (s *Store) GetSourceByName(name string) (*Source, error) {
	return scanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE name=?`, name))
}

func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT ` + sourceCols + ` FROM sources ORDER BY name`)
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

func (s *Store) DeleteSource(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) MarkSourceSynced(id int64, commit string, skillCount int) error {
	_, err := s.db.Exec(`UPDATE sources SET last_synced_at=?, last_commit=?, last_skill_count=?, updated_at=? WHERE id=?`,
		now(), commit, skillCount, now(), id)
	return err
}

// sourceCols and scanSource must list the same columns in the same order —
// the compiler cannot check that, so change them together.
const sourceCols = `id, name, COALESCE(kind,'git'), COALESCE(local_path,''), repo_url, branch, subpath, trust_tier, COALESCE(license,''), COALESCE(last_synced_at,0), COALESCE(last_commit,''), last_skill_count, enabled, created_at, updated_at`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	src := &Source{}
	var (
		enabled int
		kind    string
	)
	err := row.Scan(&src.ID, &src.Name, &kind, &src.LocalPath, &src.RepoURL, &src.Branch, &src.Subpath, &src.TrustTier,
		&src.License, &src.LastSyncedAt, &src.LastCommit, &src.LastSkillCount, &enabled, &src.CreatedAt, &src.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.Kind = SourceKind(kind)
	src.Enabled = enabled == 1
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
func (s *Store) UpsertSkill(sk *Skill) (bool, error) {
	if sk.CreatedAt == 0 {
		sk.CreatedAt = now()
	}
	sk.UpdatedAt = now()
	sk.ContentHash = hashBytes([]byte(sk.SkillMD))
	tagsJSON := encodeTags(sk.Tags)

	// Try update first
	res, err := s.db.Exec(`
		UPDATE skills SET display_name=?, description=?, skill_md=?, source_path=?, source_commit=?,
			content_hash=?, file_count=?, tags=?, enabled=?, updated_at=?
		WHERE source_id=? AND name=?
	`, sk.DisplayName, sk.Description, sk.SkillMD, sk.SourcePath, sk.SourceCommit,
		sk.ContentHash, sk.FileCount, tagsJSON, boolToInt(sk.Enabled), sk.UpdatedAt,
		sk.SourceID, sk.Name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return false, s.db.QueryRow(`SELECT id FROM skills WHERE source_id=? AND name=?`, sk.SourceID, sk.Name).Scan(&sk.ID)
	}

	res, err = s.db.Exec(`
		INSERT INTO skills (source_id, name, display_name, description, skill_md, source_path, source_commit,
			content_hash, file_count, tags, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sk.SourceID, sk.Name, sk.DisplayName, sk.Description, sk.SkillMD, sk.SourcePath, sk.SourceCommit,
		sk.ContentHash, sk.FileCount, tagsJSON, boolToInt(sk.Enabled), sk.CreatedAt, sk.UpdatedAt)
	if err != nil {
		return false, wrapForeignKeyViolation(err, "source", sk.SourceID)
	}
	sk.ID, _ = res.LastInsertId()
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
	var enabled int
	var tags string
	err := row.Scan(&sk.ID, &sk.SourceID, &sk.SourceName, &sk.Name, &sk.DisplayName, &sk.Description,
		&sk.SkillMD, &sk.SourcePath, &sk.SourceCommit, &sk.ContentHash, &sk.FileCount,
		&tags, &enabled, &sk.CreatedAt, &sk.UpdatedAt, &sk.InstalledAt, &sk.InstallPath)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sk.Enabled = enabled == 1
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

// ListSkillsFilter narrows listSkills — extended from top of file. If
// InstalledOnly is true, only rows that are currently installed are returned.
func (s *Store) ListSkills(f ListSkillsFilter) ([]Skill, error) {
	q := `SELECT ` + skillCols + skillFrom + ` WHERE 1=1`
	var args []any
	if f.SourceID > 0 {
		q += ` AND s.source_id = ?`
		args = append(args, f.SourceID)
	}
	if f.SourceName != "" {
		q += ` AND src.name = ?`
		args = append(args, f.SourceName)
	}
	if f.Query != "" {
		like := "%" + f.Query + "%"
		q += ` AND (s.name LIKE ? OR s.description LIKE ?)`
		args = append(args, like, like)
	}
	if f.Tag != "" {
		// Cheap LIKE match on the JSON array; entries are quoted strings.
		q += ` AND s.tags LIKE ?`
		args = append(args, `%"`+f.Tag+`"%`)
	}
	if f.InstalledOnly {
		q += ` AND i.skill_id IS NOT NULL`
	}
	q += ` ORDER BY src.name, s.name`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
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

func (s *Store) GetSkill(id int64) (*Skill, error) {
	return scanSkill(s.db.QueryRow(`SELECT `+skillCols+skillFrom+` WHERE s.id=?`, id), true)
}

func (s *Store) DeleteSkill(id int64) error {
	_, err := s.db.Exec(`DELETE FROM skills WHERE id=?`, id)
	return err
}

// DeleteSkillsForSourceExcept removes skills in source_id that are NOT in the keep list.
// Used after a sync to prune skills that disappeared upstream.
func (s *Store) DeleteSkillsForSourceExcept(sourceID int64, keepIDs []int64) (int, error) {
	if len(keepIDs) == 0 {
		res, err := s.db.Exec(`DELETE FROM skills WHERE source_id=?`, sourceID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// Build IN-clause
	q := `DELETE FROM skills WHERE source_id=? AND id NOT IN (`
	args := []any{sourceID}
	for i, id := range keepIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `)`
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ============================================
// helpers
// ============================================

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
