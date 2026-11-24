package repostore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

// DefaultDataDir is ~/.config/repo-store.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "repo-store")
}

type Store struct {
	db      *sql.DB
	dataDir string
}

// Open opens or creates the store rooted at dataDir. Pass "" for the default.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "repo-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	// Columns first, then schema.sql. schema.sql creates indexes over columns
	// it also declares, and an index over a column an older database lacks
	// cannot be created until the column is there.
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, dataDir: dataDir}, nil
}

// addMissingColumns brings a database created by an older schema up to the
// current one. schema.sql is all CREATE ... IF NOT EXISTS, which creates a
// missing table and does nothing at all to a table that already exists — so a
// column added to schema.sql reaches a fresh database and never reaches the
// live one, which is where every existing row is. Every column here must be
// nullable or defaulted, because ALTER TABLE ADD COLUMN cannot fill existing
// rows.
//
// A database with no repos table at all is a fresh one: there is nothing to
// alter, and schema.sql creates the table with every column already on it.
func addMissingColumns(db *sql.DB) error {
	want := []struct{ name, ddl string }{
		{"provenance", `ALTER TABLE repos ADD COLUMN provenance TEXT NOT NULL DEFAULT ''`},
	}
	have, err := columnsOf(db, "repos")
	if err != nil {
		return err
	}
	if len(have) == 0 {
		return nil
	}
	for _, c := range want {
		if _, ok := have[c.name]; ok {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

func columnsOf(db *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

func (s *Store) Close() error    { return s.db.Close() }
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

func validate(r *Repo) error {
	if r.Name == "" {
		return errors.New("repo: name is required")
	}
	return nil
}

// UpsertRepo inserts or updates by name. Sets ID on insert. The caller is
// responsible for populating Tags (e.g. via Detect); this method only persists.
//
// It deliberately does NOT write Provenance. This is the repository refresh
// path — re-POSTing a repo to re-detect its tags is routine — and a refresh
// whose body omitted the provenance would otherwise blank a field nobody meant
// to touch. Provenance is written only by SetProvenance. On return, r.Provenance
// carries whatever is stored, so a caller never reads back the value it sent.
func (s *Store) UpsertRepo(r *Repo) (bool, error) {
	if err := validate(r); err != nil {
		return false, err
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = now()
	}
	r.UpdatedAt = now()
	tags := encodeStrings(r.Tags)
	overrides := encodeStrings(r.TagOverrides)

	res, err := s.db.Exec(`
		UPDATE repos SET path=?, git_remote=?, description=?, tags=?, tag_overrides=?, updated_at=?
		WHERE name=?
	`, r.Path, r.GitRemote, r.Description, tags, overrides, r.UpdatedAt, r.Name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := s.db.QueryRow(`SELECT id, provenance FROM repos WHERE name=?`, r.Name).Scan(&r.ID, &r.Provenance); err != nil {
			return false, err
		}
		r.EffectiveTags = unionSorted(r.Tags, r.TagOverrides)
		r.GitHubURL = GitHubURLOfRemote(r.GitRemote)
		return false, nil
	}

	res, err = s.db.Exec(`
		INSERT INTO repos (name, path, git_remote, description, tags, tag_overrides, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, r.Name, r.Path, r.GitRemote, r.Description, tags, overrides, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return false, err
	}
	r.ID, _ = res.LastInsertId()
	r.Provenance = ProvenanceUnstated
	r.EffectiveTags = unionSorted(r.Tags, r.TagOverrides)
	r.GitHubURL = GitHubURLOfRemote(r.GitRemote)
	return true, nil
}

// SetProvenance records who authors the repository. It is the only write path
// for the column, so the routine tag refresh cannot disturb it. An unknown
// value is refused rather than stored: a fleet-wide count reads this field to
// say what population it counted, and a value the reader does not recognize
// leaves that count unanswerable.
func (s *Store) SetProvenance(id int64, p Provenance) (*Repo, error) {
	if !p.IsKnown() {
		return nil, fmt.Errorf("repo: unknown provenance %q", string(p))
	}
	// No rows-affected check: an UPDATE that matches nothing is not an error in
	// SQLite, and the read below already answers ErrNotFound for exactly that
	// id. A second check here would be a branch no input can reach, reading to
	// a later maintainer as a case somebody handled.
	if _, err := s.db.Exec(`UPDATE repos SET provenance=?, updated_at=? WHERE id=?`, string(p), now(), id); err != nil {
		return nil, err
	}
	return s.GetRepo(id)
}

// ErrNameTaken and ErrPathTaken refuse an identity change that would leave
// two rows answering to one name or one path. Lookups by name (chat mentions)
// and by path (the deploy gate) each return one row, so a second claimant
// would make one of them silently answer for the other.
var (
	ErrNameTaken = errors.New("name is held by another repo")
	ErrPathTaken = errors.New("path is held by another repo")
)

// RepoIdentity is what SetIdentity rewrites: the fields other services look a
// repo up by. Tags is nil to keep the stored tags, or the freshly detected
// set when the caller re-detected from the new path.
type RepoIdentity struct {
	Name      string
	Path      string
	GitRemote string
	Tags      []string
}

// SetIdentity renames a repo and moves its path and git remote while keeping
// its id, so every row keyed by repo_id (the deploy ledger) stays attached.
// UpsertRepo cannot do this: it matches by name, so a new name would make a
// new row with a new id. It touches nothing on disk.
//
// The deploy ledger keeps the source_path each deploy was built from; those
// rows are history and are not rewritten.
func (s *Store) SetIdentity(id int64, ident RepoIdentity) (*Repo, error) {
	if ident.Name == "" {
		return nil, errors.New("repo: name is required")
	}
	if !filepath.IsAbs(ident.Path) {
		return nil, fmt.Errorf("repo: path %q is not absolute", ident.Path)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int64
	if err := tx.QueryRow(`SELECT id FROM repos WHERE id=?`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var other int64
	switch err := tx.QueryRow(`SELECT id FROM repos WHERE name=? AND id<>?`, ident.Name, id).Scan(&other); {
	case err == nil:
		return nil, fmt.Errorf("%w: %q is repo %d", ErrNameTaken, ident.Name, other)
	case err != sql.ErrNoRows:
		return nil, err
	}
	switch err := tx.QueryRow(`SELECT id FROM repos WHERE path=? AND id<>?`, ident.Path, id).Scan(&other); {
	case err == nil:
		return nil, fmt.Errorf("%w: %q is repo %d", ErrPathTaken, ident.Path, other)
	case err != sql.ErrNoRows:
		return nil, err
	}

	if ident.Tags != nil {
		_, err = tx.Exec(`UPDATE repos SET name=?, path=?, git_remote=?, tags=?, updated_at=? WHERE id=?`,
			ident.Name, ident.Path, ident.GitRemote, encodeStrings(ident.Tags), now(), id)
	} else {
		_, err = tx.Exec(`UPDATE repos SET name=?, path=?, git_remote=?, updated_at=? WHERE id=?`,
			ident.Name, ident.Path, ident.GitRemote, now(), id)
	}
	if err != nil {
		return nil, err
	}
	repo, err := scanRepo(tx.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return repo, nil
}

const repoCols = `id, name, path, git_remote, description, provenance, tags, tag_overrides, created_at, updated_at`

func scanRepo(row interface{ Scan(...any) error }) (*Repo, error) {
	r := &Repo{}
	var tags, overrides string
	err := row.Scan(&r.ID, &r.Name, &r.Path, &r.GitRemote, &r.Description, &r.Provenance, &tags, &overrides, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Tags = decodeStrings(tags)
	r.TagOverrides = decodeStrings(overrides)
	r.EffectiveTags = unionSorted(r.Tags, r.TagOverrides)
	r.GitHubURL = GitHubURLOfRemote(r.GitRemote)
	return r, nil
}

func (s *Store) GetRepo(id int64) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id=?`, id))
}

func (s *Store) GetRepoByName(name string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE name=?`, name))
}

func (s *Store) GetRepoByPath(path string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE path=?`, path))
}

// ListFilter narrows ListRepos. Tag matches against the effective tag set
// (detected ∪ overrides), so a manual override surfaces a repo in tag queries.
type ListFilter struct {
	Tag   string
	Query string
	Limit int
	// Provenance narrows to one authorship class. It is a *Provenance rather
	// than a Provenance because the unstated class is a real answer a caller
	// asks for, and the zero value already means it — so nil is "do not
	// filter" and a pointer to "" is "only the unclassified ones".
	Provenance *Provenance
}

func (s *Store) ListRepos(f ListFilter) ([]Repo, error) {
	q := `SELECT ` + repoCols + ` FROM repos WHERE 1=1`
	var args []any
	if f.Tag != "" {
		// Match the tag in either detected tags or manual overrides.
		q += ` AND (tags LIKE ? OR tag_overrides LIKE ?)`
		args = append(args, `%"`+f.Tag+`"%`, `%"`+f.Tag+`"%`)
	}
	if f.Provenance != nil {
		q += ` AND provenance=?`
		args = append(args, string(*f.Provenance))
	}
	if f.Query != "" {
		like := "%" + f.Query + "%"
		q += ` AND (name LIKE ? OR description LIKE ? OR path LIKE ?)`
		args = append(args, like, like, like)
	}
	q += ` ORDER BY name`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRepo(id int64) error {
	res, err := s.db.Exec(`DELETE FROM repos WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func encodeStrings(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

func decodeStrings(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
