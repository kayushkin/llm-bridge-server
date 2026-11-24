package bundlestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

// DefaultDataDir is ~/.config/bundle-store.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "bundle-store")
}

type Store struct {
	db      *sql.DB
	dataDir string

	// QuarantinedMembers counts the rows Open moved into LegacyMembersTable
	// because they were keyed by name and carry no id. Zero on every database
	// created or opened after that migration ran.
	QuarantinedMembers int

	// BackfilledExtendsLinks counts the inheritance links Open converted from a
	// parent's name to that parent's id. Zero on every database created or
	// opened after that migration ran.
	BackfilledExtendsLinks int
}

// Open opens or creates the store rooted at dataDir. Pass "" for the default.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "bundle-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	quarantined, err := quarantineNameJoinedMembers(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	backfilled, err := backfillNameJoinedExtends(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addDenyColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{
		db:                     db,
		dataDir:                dataDir,
		QuarantinedMembers:     quarantined,
		BackfilledExtendsLinks: backfilled,
	}, nil
}

// LegacyMembersTable holds the rows of a bundle_members that predates
// member_id. They are kept, not dropped: a name cannot be turned into an id
// without asking the owning store, and this package never asks.
const LegacyMembersTable = "bundle_members_legacy_name_join"

// quarantineNameJoinedMembers moves a pre-member_id bundle_members table aside
// so the current schema can create the real one, and reports how many rows went
// with it. None of those rows carries an id, so none can be resolved;
// anything hand-curated is still readable in LegacyMembersTable and has to be
// re-added with the ids its names never carried.
func quarantineNameJoinedMembers(db *sql.DB) (int, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='bundle_members'`).Scan(&name)
	if err == sql.ErrNoRows {
		return 0, nil // fresh database: schema.sql creates the current shape
	}
	if err != nil {
		return 0, err
	}
	columns, err := tableColumns(db, "bundle_members")
	if err != nil {
		return 0, err
	}
	if columns["member_id"] {
		return 0, nil // already migrated
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM bundle_members`).Scan(&count); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS ` + LegacyMembersTable); err != nil {
		return 0, err
	}
	// The index has to go before the rename: SQLite keeps it attached to the
	// renamed table, and schema.sql's CREATE INDEX IF NOT EXISTS would then
	// find the name taken and leave the real table unindexed.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_bundle_members_bundle`); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`ALTER TABLE bundle_members RENAME TO ` + LegacyMembersTable); err != nil {
		return 0, err
	}
	return count, nil
}

// addDenyColumns brings a database written before deny existed up to the
// current schema: bundle_members gains effect, and bundles gains
// denied_read_paths. schema.sql's CREATE TABLE IF NOT EXISTS leaves an existing
// table alone, so it cannot add them itself.
//
// Both are plain ADD COLUMNs. SQLite accepts a CHECK on an added column and
// tests it against the rows already there, and every existing row takes the
// default: each member becomes 'include', which is what it always meant, and
// each bundle denies no paths. No row is rewritten or dropped.
func addDenyColumns(db *sql.DB) error {
	members, err := tableColumns(db, "bundle_members")
	if err != nil {
		return err
	}
	if !members["effect"] {
		if _, err := db.Exec(`ALTER TABLE bundle_members ADD COLUMN effect TEXT NOT NULL DEFAULT 'include'
			CHECK (effect IN ('include', 'deny'))
			CHECK (effect = 'include' OR kind = 'tool')`); err != nil {
			return fmt.Errorf("add bundle_members.effect: %w", err)
		}
	}
	bundles, err := tableColumns(db, "bundles")
	if err != nil {
		return err
	}
	if !bundles["denied_read_paths"] {
		if _, err := db.Exec(`ALTER TABLE bundles ADD COLUMN denied_read_paths TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add bundles.denied_read_paths: %w", err)
		}
	}
	return nil
}

// tableColumns reports which columns a table has. An absent table has none.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// backfillNameJoinedExtends converts a pre-extends_id bundles table, whose
// inheritance link was the parent's name, to the id join, and reports how many
// links it converted.
//
// Unlike the member migration this one CAN be done without asking anyone: the
// parent is a row in this very table and `bundles.name` is UNIQUE, so each name
// resolves to exactly one id or to none at all. A name that resolves to none is
// a dangling link, and this fails the open rather than writing NULL — NULL means
// "this bundle is a root", and a child that has silently become a root inherits
// nothing while still looking perfectly healthy. The error names the bundle, the
// parent it wanted, and the two ways out.
func backfillNameJoinedExtends(db *sql.DB) (int, error) {
	columns, err := tableColumns(db, "bundles")
	if err != nil {
		return 0, err
	}
	if len(columns) == 0 {
		return 0, nil // fresh database: schema.sql creates the current shape
	}
	if columns["extends_id"] {
		return 0, nil // already migrated
	}
	if !columns["extends"] {
		return 0, fmt.Errorf("bundles has neither extends_id nor extends — refusing to guess at its inheritance links")
	}

	// SQLite allows adding a column with a REFERENCES clause only when its
	// default is NULL, which is what a root bundle wants anyway.
	if _, err := db.Exec(`ALTER TABLE bundles ADD COLUMN extends_id INTEGER REFERENCES bundles(id) ON DELETE RESTRICT`); err != nil {
		return 0, err
	}
	res, err := db.Exec(`
		UPDATE bundles SET extends_id = (SELECT parent.id FROM bundles parent WHERE parent.name = bundles.extends)
		WHERE extends <> ''
	`)
	if err != nil {
		return 0, err
	}

	dangling, err := danglingExtendsNames(db)
	if err != nil {
		return 0, err
	}
	if len(dangling) > 0 {
		return 0, fmt.Errorf(
			"%d inheritance link(s) name a parent bundle that does not exist: %s. "+
				"They cannot be carried over as ids and will not be silently cleared, because a child "+
				"whose parent went missing must not come back looking like a root. Either recreate the "+
				"parent (INSERT INTO bundles …) or cut the link (UPDATE bundles SET extends='' WHERE name=…), "+
				"then start again",
			len(dangling), strings.Join(dangling, "; "))
	}

	if _, err := db.Exec(`ALTER TABLE bundles DROP COLUMN extends`); err != nil {
		return 0, err
	}
	converted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(converted), nil
}

// danglingExtendsNames lists "child -> missing parent" for every row whose
// extends name found no id, in the shape an error message wants.
func danglingExtendsNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name, extends FROM bundles WHERE extends <> '' AND extends_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("bundle %q extends missing %q", child, parent))
	}
	return out, rows.Err()
}

func (s *Store) Close() error    { return s.db.Close() }
func (s *Store) DataDir() string { return s.dataDir }

func now() int64 { return time.Now().Unix() }

// ErrParentHasChildren is what DeleteBundle returns when other bundles still
// inherit from the one being deleted. The schema's ON DELETE RESTRICT is what
// refuses it; this turns that refusal into something a caller can match on.
var ErrParentHasChildren = errors.New("bundle is extended by other bundles")

func validate(b *Bundle) error {
	if b.Name == "" {
		return errors.New("bundle: name is required")
	}
	if b.ExtendsID < 0 {
		return fmt.Errorf("bundle %s: extends_id %d is not a bundle id", b.Name, b.ExtendsID)
	}
	type memberKey struct {
		kind MemberKind
		id   int64
	}
	seen := map[memberKey]MemberEffect{}
	for i := range b.Members {
		m := &b.Members[i]
		if !m.Kind.Valid() {
			return fmt.Errorf("bundle %s: member kind %q must be skill or tool", b.Name, m.Kind)
		}
		if m.ID <= 0 {
			return fmt.Errorf(
				"bundle %s: %s member %q has no id — a member is identified by the id its owning store assigned (%s), never by its name",
				b.Name, m.Kind, m.Name, m.Kind.OwningStore())
		}
		if m.Effect == "" {
			m.Effect = MemberInclude
		}
		if !m.Effect.Valid() {
			return fmt.Errorf("bundle %s: %s member id %d has effect %q; it must be one of %s (GET /member-effects)",
				b.Name, m.Kind, m.ID, m.Effect, joinEffects(MemberEffects))
		}
		if !m.Effect.AllowedFor(m.Kind) {
			return fmt.Errorf("bundle %s: %s member id %d cannot be %s — only a tool can be denied",
				b.Name, m.Kind, m.ID, m.Effect)
		}
		key := memberKey{kind: m.Kind, id: m.ID}
		if earlier, ok := seen[key]; ok {
			if earlier != m.Effect {
				return fmt.Errorf("bundle %s: %s member id %d is both included and denied — one bundle does one or the other",
					b.Name, m.Kind, m.ID)
			}
			return fmt.Errorf("bundle %s: %s member id %d listed twice", b.Name, m.Kind, m.ID)
		}
		seen[key] = m.Effect
	}
	return validateDeniedReadPaths(b)
}

// validateDeniedReadPaths refuses a path pattern that is not absolute or
// under the home directory. A relative pattern would mean something different
// in every working directory a session starts in, so it cannot say which
// files are off limits. A pattern listed twice is refused as well, so the
// stored list is exactly what the curator meant.
func validateDeniedReadPaths(b *Bundle) error {
	seen := map[string]bool{}
	for _, p := range b.DeniedReadPaths {
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~/") {
			return fmt.Errorf("bundle %s: denied read path %q must start with \"/\" or \"~/\"", b.Name, p)
		}
		if seen[p] {
			return fmt.Errorf("bundle %s: denied read path %q listed twice", b.Name, p)
		}
		seen[p] = true
	}
	return nil
}

func joinEffects(effects []MemberEffect) string {
	parts := make([]string, len(effects))
	for i, e := range effects {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}

// UpsertBundle inserts or updates by name, replacing the full member set in a
// single transaction. Sets ID on insert.
func (s *Store) UpsertBundle(b *Bundle) (bool, error) {
	if err := validate(b); err != nil {
		return false, err
	}
	if b.CreatedAt == 0 {
		b.CreatedAt = now()
	}
	b.UpdatedAt = now()
	matchTags := encodeStrings(b.MatchTags)
	deniedReadPaths := encodeStrings(b.DeniedReadPaths)

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// The one place a parent's name becomes a parent's id. It happens once, on
	// the write, against this store's own table — never again on a read.
	extendsID, err := resolveExtends(tx, b)
	if err != nil {
		return false, err
	}

	res, err := tx.Exec(`
		UPDATE bundles SET display_name=?, description=?, extends_id=?, match_tags=?, denied_read_paths=?, model=?, effort=?, enabled=?, updated_at=?
		WHERE name=?
	`, b.DisplayName, b.Description, extendsID, matchTags, deniedReadPaths, b.Model, b.Effort, boolToInt(b.Enabled), b.UpdatedAt, b.Name)
	if err != nil {
		return false, err
	}
	inserted := false
	if n, _ := res.RowsAffected(); n > 0 {
		if err := tx.QueryRow(`SELECT id FROM bundles WHERE name=?`, b.Name).Scan(&b.ID); err != nil {
			return false, err
		}
		// Only an update can build a cycle. An insert's parent had to exist
		// already, and nothing can extend a bundle that did not exist a moment
		// ago, so a new row is always a leaf.
		if err := refuseInheritanceCycle(tx, b); err != nil {
			return false, err
		}
	} else {
		res, err = tx.Exec(`
			INSERT INTO bundles (name, display_name, description, extends_id, match_tags, denied_read_paths, model, effort, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, b.Name, b.DisplayName, b.Description, extendsID, matchTags, deniedReadPaths, b.Model, b.Effort, boolToInt(b.Enabled), b.CreatedAt, b.UpdatedAt)
		if err != nil {
			return false, err
		}
		b.ID, _ = res.LastInsertId()
		inserted = true
	}

	if _, err := tx.Exec(`DELETE FROM bundle_members WHERE bundle_id=?`, b.ID); err != nil {
		return false, err
	}
	for _, m := range b.Members {
		if _, err := tx.Exec(`
			INSERT INTO bundle_members (bundle_id, kind, member_id, name, condition, effect) VALUES (?, ?, ?, ?, ?, ?)
		`, b.ID, string(m.Kind), m.ID, m.Name, m.Condition, string(m.Effect)); err != nil {
			return false, err
		}
	}
	return inserted, tx.Commit()
}

// resolveExtends settles a bundle's inheritance link to the id that will be
// stored, and writes both halves back onto b so the caller sees what was
// recorded. It accepts an id, or a name, or both when they agree, and refuses
// anything else — a link that cannot be pinned to an existing row is not
// written as NULL, because NULL means "root" and a demoted child inherits
// nothing while still reading as healthy.
func resolveExtends(tx *sql.Tx, b *Bundle) (any, error) {
	if b.ExtendsID == 0 && b.Extends == "" {
		return nil, nil
	}

	if b.ExtendsID > 0 {
		var name string
		err := tx.QueryRow(`SELECT name FROM bundles WHERE id=?`, b.ExtendsID).Scan(&name)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("bundle %s: extends_id %d is not a bundle in this store", b.Name, b.ExtendsID)
		}
		if err != nil {
			return nil, err
		}
		if b.Extends != "" && b.Extends != name {
			return nil, fmt.Errorf(
				"bundle %s: extends_id %d is bundle %q, but extends says %q — the id and the display name disagree, so one of them is wrong",
				b.Name, b.ExtendsID, name, b.Extends)
		}
		b.Extends = name
		return b.ExtendsID, nil
	}

	var id int64
	err := tx.QueryRow(`SELECT id FROM bundles WHERE name=?`, b.Extends).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf(
			"bundle %s: extends %q, which is not a bundle in this store — write the parent first, or pass extends_id if you already have it",
			b.Name, b.Extends)
	}
	if err != nil {
		return nil, err
	}
	b.ExtendsID = id
	return id, nil
}

// refuseInheritanceCycle rejects a write that would put a bundle in its own
// ancestor chain, so the cycle is reported at the write that caused it rather
// than at some later resolve of an unrelated repository. expandChain keeps its
// own guard for the rows this one cannot see: a database written before this
// check existed, or by hand.
func refuseInheritanceCycle(tx *sql.Tx, b *Bundle) error {
	seen := map[int64]bool{}
	for cur := b.ExtendsID; cur != 0; {
		if cur == b.ID {
			return fmt.Errorf("bundle %s: extending %q puts the bundle in its own inheritance chain", b.Name, b.Extends)
		}
		if seen[cur] {
			// A cycle that does not pass through b. It was already in the
			// database before this write; expandChain reports it when someone
			// resolves through it. Refusing an unrelated write over it would
			// only hide which bundle is actually broken.
			return nil
		}
		seen[cur] = true
		var parent sql.NullInt64
		err := tx.QueryRow(`SELECT extends_id FROM bundles WHERE id=?`, cur).Scan(&parent)
		if err == sql.ErrNoRows {
			return nil // the foreign key will refuse this write on its own
		}
		if err != nil {
			return err
		}
		cur = parent.Int64
	}
	return nil
}

func (s *Store) loadMembers(bundleID int64) ([]Member, error) {
	rows, err := s.db.Query(`SELECT kind, member_id, name, condition, effect FROM bundle_members WHERE bundle_id=? ORDER BY kind, name, member_id`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var kind, effect string
		if err := rows.Scan(&kind, &m.ID, &m.Name, &m.Condition, &effect); err != nil {
			return nil, err
		}
		m.Kind = MemberKind(kind)
		m.Effect = MemberEffect(effect)
		out = append(out, m)
	}
	return out, rows.Err()
}

// bundleSelect reads a bundle with its parent's name joined on the parent's id.
// The join is what keeps the display name honest: it is read from the parent
// row every time rather than copied into the child, so renaming a parent cannot
// leave a child advertising the old name.
const bundleSelect = `
	SELECT b.id, b.name, b.display_name, b.description,
	       b.extends_id, COALESCE(parent.name, ''),
	       b.match_tags, b.denied_read_paths, b.model, b.effort, b.enabled, b.created_at, b.updated_at
	FROM bundles b LEFT JOIN bundles parent ON parent.id = b.extends_id`

func (s *Store) scanBundle(row interface{ Scan(...any) error }) (*Bundle, error) {
	b := &Bundle{}
	var matchTags, deniedReadPaths string
	var enabled int
	var extendsID sql.NullInt64
	err := row.Scan(&b.ID, &b.Name, &b.DisplayName, &b.Description, &extendsID, &b.Extends, &matchTags, &deniedReadPaths, &b.Model, &b.Effort, &enabled, &b.CreatedAt, &b.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.ExtendsID = extendsID.Int64
	b.MatchTags = decodeStrings(matchTags)
	b.DeniedReadPaths, err = decodeStringsStrictly(deniedReadPaths)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: denied_read_paths: %w", b.Name, err)
	}
	b.Enabled = enabled == 1
	members, err := s.loadMembers(b.ID)
	if err != nil {
		return nil, err
	}
	b.Members = members
	return b, nil
}

func (s *Store) GetBundle(id int64) (*Bundle, error) {
	return s.scanBundle(s.db.QueryRow(bundleSelect+` WHERE b.id=?`, id))
}

// GetBundleByName answers the lookup a human asked for by name — the
// /bundles/by-name/{name} route.
// It is not a join: nothing inside this package reads a stored name back to
// find the row it points at.
func (s *Store) GetBundleByName(name string) (*Bundle, error) {
	return s.scanBundle(s.db.QueryRow(bundleSelect+` WHERE b.name=?`, name))
}

// ListFilter narrows ListBundles.
type ListFilter struct {
	Tag         string // matches a value in match_tags
	EnabledOnly bool
	Limit       int
}

func (s *Store) ListBundles(f ListFilter) ([]Bundle, error) {
	q := bundleSelect + ` WHERE 1=1`
	var args []any
	if f.EnabledOnly {
		q += ` AND b.enabled = 1`
	}
	if f.Tag != "" {
		q += ` AND b.match_tags LIKE ?`
		args = append(args, `%"`+f.Tag+`"%`)
	}
	q += ` ORDER BY b.name`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bundle
	for rows.Next() {
		b, err := s.scanBundle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// DeleteBundle removes a bundle. A bundle that other bundles extend is refused
// with ErrParentHasChildren, naming them: the schema says ON DELETE RESTRICT,
// and the point of RESTRICT is that the operator hears about the children
// instead of finding them stranded later.
func (s *Store) DeleteBundle(id int64) error {
	children, err := s.childBundleNames(id)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: %s — reparent or delete them first", ErrParentHasChildren, strings.Join(children, ", "))
	}
	res, err := s.db.Exec(`DELETE FROM bundles WHERE id=?`, id)
	if err != nil {
		// The pre-check above is what produces a useful message, but the
		// foreign key is the actual guarantee: it also holds against a racing
		// write that adds a child between the two statements.
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return fmt.Errorf("%w: %v", ErrParentHasChildren, err)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) childBundleNames(parentID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM bundles WHERE extends_id=? ORDER BY name`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) SetEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE bundles SET enabled=?, updated_at=? WHERE id=?`, boolToInt(enabled), now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodeStrings(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// decodeStringsStrictly is decodeStrings for a column whose loss would widen
// what a session may do: a denied path that fails to decode must stop the
// read, not come back as "nothing denied".
func decodeStringsStrictly(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
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
