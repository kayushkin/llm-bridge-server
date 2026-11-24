package repostore

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// ErrNotFound is a repo id, name or path no row has.
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
	return postgresstorage.Module{Schema: "repos", Role: "repos_module", Migrations: migrations}, nil
}

// Store is the registry, the deploy ledger and the pull-request copy, in the
// module's schema.
type Store struct {
	db *sql.DB
}

// New wraps a pool opened on the module (postgresstorage.Open), migrated.
func New(db *sql.DB) *Store { return &Store{db: db} }

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

	// One statement, so two upserts of one name at once cannot both insert:
	// the second finds the first's row through the name's unique index. xmax
	// is 0 only on a row this statement inserted.
	var inserted bool
	err := s.db.QueryRow(`
		INSERT INTO repos (name, path, git_remote, description, tags, tag_overrides, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE SET path=excluded.path, git_remote=excluded.git_remote, description=excluded.description,
			tags=excluded.tags, tag_overrides=excluded.tag_overrides, updated_at=excluded.updated_at
		RETURNING id, provenance, (xmax = 0)
	`, r.Name, r.Path, r.GitRemote, r.Description, tags, overrides, r.CreatedAt, r.UpdatedAt).Scan(&r.ID, &r.Provenance, &inserted)
	if err != nil {
		return false, err
	}
	if !inserted {
		r.EffectiveTags = unionSorted(r.Tags, r.TagOverrides)
		r.GitHubURL = GitHubURLOfRemote(r.GitRemote)
		return false, nil
	}
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
	// No rows-affected check: an UPDATE that matches nothing is not an error,
	// and the read below already answers ErrNotFound for exactly that id. A
	// second check here would be a branch no input can reach, reading to a
	// later maintainer as a case somebody handled.
	if _, err := s.db.Exec(`UPDATE repos SET provenance=$1, updated_at=$2 WHERE id=$3`, string(p), now(), id); err != nil {
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

	// The row is locked for the rest of the transaction, so two renames of
	// one repo take turns.
	var exists int64
	if err := tx.QueryRow(`SELECT id FROM repos WHERE id=$1 FOR UPDATE`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var other int64
	switch err := tx.QueryRow(`SELECT id FROM repos WHERE name=$1 AND id<>$2`, ident.Name, id).Scan(&other); {
	case err == nil:
		return nil, fmt.Errorf("%w: %q is repo %d", ErrNameTaken, ident.Name, other)
	case err != sql.ErrNoRows:
		return nil, err
	}
	switch err := tx.QueryRow(`SELECT id FROM repos WHERE path=$1 AND id<>$2`, ident.Path, id).Scan(&other); {
	case err == nil:
		return nil, fmt.Errorf("%w: %q is repo %d", ErrPathTaken, ident.Path, other)
	case err != sql.ErrNoRows:
		return nil, err
	}

	if ident.Tags != nil {
		_, err = tx.Exec(`UPDATE repos SET name=$1, path=$2, git_remote=$3, tags=$4, updated_at=$5 WHERE id=$6`,
			ident.Name, ident.Path, ident.GitRemote, encodeStrings(ident.Tags), now(), id)
	} else {
		_, err = tx.Exec(`UPDATE repos SET name=$1, path=$2, git_remote=$3, updated_at=$4 WHERE id=$5`,
			ident.Name, ident.Path, ident.GitRemote, now(), id)
	}
	if err != nil {
		return nil, err
	}
	repo, err := scanRepo(tx.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id=$1`, id))
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
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id=$1`, id))
}

func (s *Store) GetRepoByName(name string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE name=$1`, name))
}

func (s *Store) GetRepoByPath(path string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE path=$1 ORDER BY id LIMIT 1`, path))
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
	q := `SELECT ` + repoCols + ` FROM repos WHERE true`
	var args []any
	placeholder := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	// ILIKE: the filters have always matched without regard to case, as
	// SQLite's LIKE does for ASCII.
	if f.Tag != "" {
		// Match the tag in either detected tags or manual overrides.
		tag := placeholder(`%"` + f.Tag + `"%`)
		q += ` AND (tags ILIKE ` + tag + ` OR tag_overrides ILIKE ` + tag + `)`
	}
	if f.Provenance != nil {
		q += ` AND provenance=` + placeholder(string(*f.Provenance))
	}
	if f.Query != "" {
		like := placeholder("%" + f.Query + "%")
		q += ` AND (name ILIKE ` + like + ` OR description ILIKE ` + like + ` OR path ILIKE ` + like + `)`
	}
	// Byte order, as SQLite sorted names, not the database's language order.
	q += ` ORDER BY name COLLATE "C"`
	if f.Limit > 0 {
		q += ` LIMIT ` + placeholder(f.Limit)
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
	res, err := s.db.Exec(`DELETE FROM repos WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
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
