package bundles

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

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
	return postgresstorage.Module{Schema: "bundles", Role: "bundles_module", Migrations: migrations}, nil
}

// Store keeps the bundles in the module's schema. The server provisions,
// migrates and opens the module, then hands the pool to New.
type Store struct {
	db *sql.DB
}

// New returns the store over db, the module's pool from postgresstorage.Open.
func New(db *sql.DB) *Store { return &Store{db: db} }

// LegacyMembersTable holds the members of a standalone store written before
// member_id existed: a name and no id, so nothing resolves them. The import
// copies them so an operator can still read them back.
const LegacyMembersTable = "bundle_members_legacy_name_join"

// The Postgres error codes DeleteBundle turns into ErrParentHasChildren: an ON
// DELETE RESTRICT refusal is restrict_violation, and a NO ACTION one would be
// foreign_key_violation.
const (
	restrictViolation   = "23001"
	foreignKeyViolation = "23503"
)

func hasPostgresCode(err error, code string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == code
}

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
func (s *Store) UpsertBundle(ctx context.Context, b *Bundle) (bool, error) {
	if err := validate(b); err != nil {
		return false, err
	}
	if b.CreatedAt == 0 {
		b.CreatedAt = now()
	}
	b.UpdatedAt = now()
	matchTags := encodeStrings(b.MatchTags)
	deniedReadPaths := encodeStrings(b.DeniedReadPaths)

	inserted := false
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		// The one place a parent's name becomes a parent's id. It happens once,
		// on the write, against this store's own table — never again on a read.
		extendsID, err := resolveExtends(ctx, tx, b)
		if err != nil {
			return err
		}

		err = tx.QueryRowContext(ctx, `
			UPDATE bundles SET display_name=$1, description=$2, extends_id=$3, match_tags=$4, denied_read_paths=$5, model=$6, effort=$7, enabled=$8, updated_at=$9
			WHERE name=$10
			RETURNING id
		`, b.DisplayName, b.Description, extendsID, matchTags, deniedReadPaths, b.Model, b.Effort, b.Enabled, b.UpdatedAt, b.Name).Scan(&b.ID)
		switch {
		case err == nil:
			// Only an update can build a cycle. An insert's parent had to
			// exist already, and nothing can extend a bundle that did not
			// exist a moment ago, so a new row is always a leaf.
			if err := refuseInheritanceCycle(ctx, tx, b); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO bundles (name, display_name, description, extends_id, match_tags, denied_read_paths, model, effort, enabled, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				RETURNING id
			`, b.Name, b.DisplayName, b.Description, extendsID, matchTags, deniedReadPaths, b.Model, b.Effort, b.Enabled, b.CreatedAt, b.UpdatedAt).Scan(&b.ID); err != nil {
				return err
			}
			inserted = true
		default:
			return err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM bundle_members WHERE bundle_id=$1`, b.ID); err != nil {
			return err
		}
		for _, m := range b.Members {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO bundle_members (bundle_id, kind, member_id, name, condition, effect) VALUES ($1, $2, $3, $4, $5, $6)
			`, b.ID, string(m.Kind), m.ID, m.Name, m.Condition, string(m.Effect)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// resolveExtends settles a bundle's inheritance link to the id that will be
// stored, and writes both halves back onto b so the caller sees what was
// recorded. It accepts an id, or a name, or both when they agree, and refuses
// anything else — a link that cannot be pinned to an existing row is not
// written as NULL, because NULL means "root" and a demoted child inherits
// nothing while still reading as healthy.
func resolveExtends(ctx context.Context, tx *sql.Tx, b *Bundle) (any, error) {
	if b.ExtendsID == 0 && b.Extends == "" {
		return nil, nil
	}

	if b.ExtendsID > 0 {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM bundles WHERE id=$1`, b.ExtendsID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
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
	err := tx.QueryRowContext(ctx, `SELECT id FROM bundles WHERE name=$1`, b.Extends).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
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
// own guard for the rows this one cannot see: rows imported from a standalone
// store, or written by hand.
func refuseInheritanceCycle(ctx context.Context, tx *sql.Tx, b *Bundle) error {
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
		err := tx.QueryRowContext(ctx, `SELECT extends_id FROM bundles WHERE id=$1`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the foreign key will refuse this write on its own
		}
		if err != nil {
			return err
		}
		cur = parent.Int64
	}
	return nil
}

// Names are ordered byte by byte (COLLATE "C"), as the standalone store's
// SQLite ordered them, so a list comes back in the same order whatever the
// database's own collation is.

func (s *Store) loadMembers(ctx context.Context, bundleID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, member_id, name, condition, effect FROM bundle_members WHERE bundle_id=$1 ORDER BY kind COLLATE "C", name COLLATE "C", member_id`, bundleID)
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

// scanBundleRow reads one row of bundleSelect, without its members.
func scanBundleRow(row interface{ Scan(...any) error }) (*Bundle, error) {
	b := &Bundle{}
	var matchTags, deniedReadPaths string
	var extendsID sql.NullInt64
	err := row.Scan(&b.ID, &b.Name, &b.DisplayName, &b.Description, &extendsID, &b.Extends, &matchTags, &deniedReadPaths, &b.Model, &b.Effort, &b.Enabled, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	return b, nil
}

func (s *Store) getOne(ctx context.Context, where string, argument any) (*Bundle, error) {
	b, err := scanBundleRow(s.db.QueryRowContext(ctx, bundleSelect+` WHERE `+where, argument))
	if err != nil {
		return nil, err
	}
	if b.Members, err = s.loadMembers(ctx, b.ID); err != nil {
		return nil, err
	}
	return b, nil
}

// GetBundle reads one bundle by id, enabled or not. ErrNotFound when there is
// no such bundle.
func (s *Store) GetBundle(ctx context.Context, id int64) (*Bundle, error) {
	return s.getOne(ctx, `b.id=$1`, id)
}

// GetBundleByName answers the lookup a human asked for by name — the
// /bundles/by-name/{name} route.
// It is not a join: nothing inside this package reads a stored name back to
// find the row it points at.
func (s *Store) GetBundleByName(ctx context.Context, name string) (*Bundle, error) {
	return s.getOne(ctx, `b.name=$1`, name)
}

// ListFilter narrows ListBundles.
type ListFilter struct {
	Tag         string // matches a value in match_tags
	EnabledOnly bool
	Limit       int
}

func (s *Store) ListBundles(ctx context.Context, f ListFilter) ([]Bundle, error) {
	q := bundleSelect + ` WHERE true`
	var args []any
	if f.EnabledOnly {
		q += ` AND b.enabled`
	}
	if f.Tag != "" {
		// ILIKE, because SQLite's LIKE, which the standalone store used, is
		// case-insensitive.
		args = append(args, `%"`+f.Tag+`"%`)
		q += fmt.Sprintf(` AND b.match_tags ILIKE $%d`, len(args))
	}
	q += ` ORDER BY b.name COLLATE "C"`
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []Bundle
	for rows.Next() {
		b, err := scanBundleRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Members, err = s.loadMembers(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteBundle removes a bundle. A bundle that other bundles extend is refused
// with ErrParentHasChildren, naming them: the schema says ON DELETE RESTRICT,
// and the point of RESTRICT is that the operator hears about the children
// instead of finding them stranded later.
func (s *Store) DeleteBundle(ctx context.Context, id int64) error {
	children, err := s.childBundleNames(ctx, id)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return fmt.Errorf("%w: %s — reparent or delete them first", ErrParentHasChildren, strings.Join(children, ", "))
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM bundles WHERE id=$1`, id)
	if err != nil {
		// The pre-check above is what produces a useful message, but the
		// foreign key is the actual guarantee: it also holds against a racing
		// write that adds a child between the two statements.
		if hasPostgresCode(err, restrictViolation) || hasPostgresCode(err, foreignKeyViolation) {
			return fmt.Errorf("%w: %v", ErrParentHasChildren, err)
		}
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

func (s *Store) childBundleNames(ctx context.Context, parentID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM bundles WHERE extends_id=$1 ORDER BY name COLLATE "C"`, parentID)
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

func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE bundles SET enabled=$1, updated_at=$2 WHERE id=$3`, enabled, now(), id)
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
