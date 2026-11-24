package principalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const principalColumns = `
	p.id, p.seq, p.kind, p.display_name, p.email, p.disabled_at, p.created_at, p.updated_at, p.availability, p.is_administrator`

func scanPrincipal(scan func(...any) error) (*Principal, error) {
	var p Principal
	var availability string
	err := scan(&p.ID, &p.Seq, &p.Kind, &p.DisplayName, &p.Email, &p.DisabledAt, &p.CreatedAt, &p.UpdatedAt, &availability, &p.IsAdministrator)
	if err != nil {
		return nil, err
	}
	if p.Availability, err = decodeAvailability(availability); err != nil {
		return nil, fmt.Errorf("principal %s: %w", p.ID, err)
	}
	p.fillKindAnswers()
	return &p, nil
}

// validateForWrite checks the fields every principal must carry, whatever the
// write path. A blank display name is refused rather than stored: the name is
// the only thing a card or a permission can render, and a row that renders as
// nothing is indistinguishable from a broken join.
func validateForWrite(p *Principal) error {
	kind, ok := NormalizeKind(p.Kind)
	if !ok {
		return ErrUnknownKind(p.Kind)
	}
	p.Kind = kind
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if p.DisplayName == "" {
		return fmt.Errorf("%w: display_name is required", ErrInvalidPrincipal)
	}
	p.Email = strings.TrimSpace(p.Email)
	return nil
}

// Create records a new principal and hands back its id.
func (s *Store) Create(input *Principal) (*Principal, error) {
	if err := validateForWrite(input); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM principals`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	seq := maxSeq.Int64 + 1
	id := formatID(seq)
	ts := now()

	_, err = tx.Exec(`
		INSERT INTO principals (id, seq, kind, display_name, email, disabled_at, created_at, updated_at)
		VALUES (?,?,?,?,?,0,?,?)`,
		id, seq, input.Kind, input.DisplayName, input.Email, ts, ts)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// getRow reads one principal without expanding its memberships. Disabled rows
// are returned like any other: a card assigned to a departed human still has
// to render their name.
func (s *Store) getRow(id string) (*Principal, error) {
	row := s.db.QueryRow(`SELECT `+principalColumns+` FROM principals p WHERE p.id = ?`, id)
	p, err := scanPrincipal(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: principal %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Get returns one principal with its memberships expanded: the groups a human
// belongs to, or the members a group holds. Disabled principals are always
// returned; includeDisabledMemberships says whether disabled rows appear in
// the expanded list as well.
func (s *Store) Get(id string, includeDisabledMemberships bool) (*Principal, error) {
	p, err := s.getRow(id)
	if err != nil {
		return nil, err
	}
	switch {
	case p.ActsAsCaller:
		if p.Groups, err = s.ListGroups(id, includeDisabledMemberships); err != nil {
			return nil, err
		}
	case p.Kind == KindGroup:
		if p.Members, err = s.ListMembers(id, includeDisabledMemberships); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// buildQuery assembles the shared WHERE for List and Count.
func (s *Store) buildQuery(f Filter, selectClause string) (string, []any, error) {
	var args []any
	where := ` FROM principals p WHERE 1=1`
	if !f.IncludeDisabled {
		where += ` AND p.disabled_at = 0`
	}
	if f.Kind != "" {
		kind, ok := NormalizeKind(f.Kind)
		if !ok {
			return "", nil, ErrUnknownKind(f.Kind)
		}
		if f.AvailableAt != nil && kind != KindHuman {
			return "", nil, fmt.Errorf("%w: available_at applies to humans only — a group has no hours of its own; drop kind or set kind=human", ErrInvalidAvailability)
		}
		where += ` AND p.kind = ?`
		args = append(args, kind)
	} else if f.AvailableAt != nil {
		where += ` AND p.kind = ?`
		args = append(args, KindHuman)
	}
	if strings.TrimSpace(f.Query) != "" {
		expression := searchExpression(f.Query)
		if err := s.validateSearchQuery(expression); err != nil {
			return "", nil, err
		}
		where += ` AND p.seq IN (SELECT rowid FROM principals_fts WHERE principals_fts MATCH ?)`
		args = append(args, expression)
	}
	return selectClause + where, args, nil
}

// searchExpression turns what a caller typed into the FTS5 MATCH expression.
//
// Bare words become prefix terms — "pri" finds Priya Raman — because the main
// caller is an assignee picker reading keystrokes, and FTS5's default is a
// whole-token match that would find her only once the whole name was typed.
// Anything that already carries FTS5 syntax (a quote, a star, a colon,
// parentheses, or an uppercase AND / OR / NOT / NEAR) is passed through
// untouched, so an exact-phrase lookup like "Vlad Kayushkin" still means
// exactly that, which is what the seed script relies on.
func searchExpression(q string) string {
	q = strings.TrimSpace(q)
	if strings.ContainsAny(q, `"*:()`) {
		return q
	}
	words := strings.Fields(q)
	for _, w := range words {
		switch w {
		case "AND", "OR", "NOT", "NEAR":
			return q
		}
	}
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}

// validateSearchQuery probes the FTS index so a malformed query is the caller's
// 400 rather than a 500 from deep inside the list handler.
func (s *Store) validateSearchQuery(expression string) error {
	var discard int
	err := s.db.QueryRow(
		`SELECT 1 FROM principals_fts WHERE principals_fts MATCH ? LIMIT 1`, expression).Scan(&discard)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("%w: bad search query %q: %v", ErrInvalidPrincipal, expression, err)
}

// List returns principals in display-name order. Memberships are not expanded
// here; Get does that for one row.
//
// With AvailableAt set the week is evaluated in Go, so the page is cut after
// the filter rather than before — otherwise a page of ten could come back as
// three with seven more hiding behind the next offset.
func (s *Store) List(f Filter) ([]*Principal, error) {
	query, args, err := s.buildQuery(f, `SELECT `+principalColumns)
	if err != nil {
		return nil, err
	}
	query += ` ORDER BY p.display_name COLLATE NOCASE ASC, p.seq ASC`
	if f.AvailableAt == nil {
		query += pageClause(f.Limit, f.Offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Principal{}
	for rows.Next() {
		p, err := scanPrincipal(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if f.AvailableAt != nil {
		if out, err = s.filterAvailable(out, *f.AvailableAt); err != nil {
			return nil, err
		}
		out = page(out, f.Limit, f.Offset)
	}
	return out, nil
}

func pageClause(limit, offset int) string {
	clause := ""
	if limit > 0 {
		clause += fmt.Sprintf(` LIMIT %d`, limit)
	}
	if offset > 0 {
		if limit <= 0 {
			clause += ` LIMIT -1`
		}
		clause += fmt.Sprintf(` OFFSET %d`, offset)
	}
	return clause
}

func page(principals []*Principal, limit, offset int) []*Principal {
	if offset > 0 {
		if offset >= len(principals) {
			return []*Principal{}
		}
		principals = principals[offset:]
	}
	if limit > 0 && limit < len(principals) {
		principals = principals[:limit]
	}
	return principals
}

// Count is List's total, ignoring limit and offset.
func (s *Store) Count(f Filter) (int, error) {
	if f.AvailableAt != nil {
		f.Limit, f.Offset = 0, 0
		principals, err := s.List(f)
		if err != nil {
			return 0, err
		}
		return len(principals), nil
	}
	query, args, err := s.buildQuery(f, `SELECT COUNT(*)`)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Counts is the /health summary over every row, disabled included.
func (s *Store) Counts() (Counts, error) {
	var c Counts
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(kind = ?), 0),
		       COALESCE(SUM(kind = ?), 0),
		       COALESCE(SUM(disabled_at > 0), 0)
		FROM principals`, KindHuman, KindGroup).
		Scan(&c.Principals, &c.Humans, &c.Groups, &c.Disabled)
	return c, err
}

// Patch edits the display fields. It refuses kind and disabled_at by having
// nowhere to put them: kind is fixed at creation, and disabled_at moves through
// Disable and Enable so removal is always an explicit act.
func (s *Store) Patch(id string, patch Patch) (*Principal, error) {
	current, err := s.getRow(id)
	if err != nil {
		return nil, err
	}
	if patch.DisplayName != nil {
		current.DisplayName = *patch.DisplayName
	}
	if patch.Email != nil {
		current.Email = *patch.Email
	}
	if patch.IsAdministrator != nil {
		if *patch.IsAdministrator && !ActsAsCaller(current.Kind) {
			return nil, fmt.Errorf("%w: only a principal that acts can be an administrator, and %s is a %s — a group is a set of principals and a contact holds nothing",
				ErrInvalidPrincipal, id, current.Kind)
		}
		current.IsAdministrator = *patch.IsAdministrator
	}
	if len(patch.Availability) > 0 {
		availability, err := decodeAvailabilityPatch(patch.Availability)
		if err != nil {
			return nil, err
		}
		if err := s.setAvailability(current, availability); err != nil {
			return nil, err
		}
	}
	if err := validateForWrite(current); err != nil {
		return nil, err
	}
	encoded, err := encodeAvailability(current.Availability)
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`UPDATE principals SET display_name=?, email=?, availability=?, is_administrator=?, updated_at=? WHERE id=?`,
		current.DisplayName, current.Email, encoded, current.IsAdministrator, now(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// decodeAvailabilityPatch reads the availability key of a PATCH: `null` and
// `{}` both clear the week (nil), anything else must be a full Availability.
func decodeAvailabilityPatch(raw json.RawMessage) (*Availability, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var a Availability
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%w: availability: %v (send an object {tzid, days, start, end}, or {} to clear)", ErrInvalidAvailability, err)
	}
	return &a, nil
}

// Disable is the only removal. The row stays, so every reference from another
// store keeps resolving; it just drops out of default listings. Idempotent: a
// second call leaves the original disabled_at alone.
func (s *Store) Disable(id string) (*Principal, error) {
	if _, err := s.getRow(id); err != nil {
		return nil, err
	}
	ts := now()
	if _, err := s.db.Exec(`UPDATE principals SET disabled_at=?, updated_at=? WHERE id=? AND disabled_at=0`, ts, ts, id); err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// Enable undoes Disable. Idempotent.
func (s *Store) Enable(id string) (*Principal, error) {
	if _, err := s.getRow(id); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE principals SET disabled_at=0, updated_at=? WHERE id=? AND disabled_at<>0`, now(), id); err != nil {
		return nil, err
	}
	return s.Get(id, false)
}

// The wire types below moved here from store.go so the TypeScript render
// (tygo.yaml) can read this file and leave the store internals out.

// Principal is one human or one group.
type Principal struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	DisabledAt  int64  `json:"disabled_at"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`

	// IsAdministrator says this human or peer administers the whole
	// deployment: the services that read it — kanban-store, grant-store,
	// llm-bridge-server — let an administrator past every per-resource check
	// they make, so it is the one fact that grants access to boards and
	// sessions nobody granted. Only a kind that acts as a caller can carry it;
	// a group is a set of principals, not someone who acts. It says nothing on
	// its own: a disabled principal is refused before this is read.
	IsAdministrator bool `json:"is_administrator"`

	// What this principal's kind may do, computed on read from
	// KindDefinitions and ignored on write. Services read these instead of
	// comparing kind to a word: acts_in_this_deployment says it may hold
	// grants, acts_as_caller that a request may act as it, and
	// restricted_to_its_grants that holding no grant of a relation refuses it
	// rather than leaving it unrestricted. holds_service_principal_tokens says
	// it may be issued a token of its own in place of a login.
	ActsInThisDeployment        bool `json:"acts_in_this_deployment"`
	ActsAsCaller                bool `json:"acts_as_caller"`
	RestrictedToItsGrants       bool `json:"restricted_to_its_grants"`
	HoldsServicePrincipalTokens bool `json:"holds_service_principal_tokens"`

	// Availability is a human's declared working week in their own zone.
	// Absent means unknown, and unknown is never available — see
	// availability.go. A group never carries one.
	Availability *Availability `json:"availability,omitempty"`

	// Computed on read by Get, ignored on write. Exactly one is present: a
	// human or a peer carries the groups it belongs to, a group carries its
	// members.
	// omitzero rather than omitempty: a group with nobody in it still answers
	// "members": [] instead of leaving the caller to infer which kind it is.
	Groups  []*Principal `json:"groups,omitzero" tstype:"Principal[]"`
	Members []*Principal `json:"members,omitzero" tstype:"Principal[]"`
}

// Patch carries only the fields a caller mentioned. kind is deliberately
// absent — it is fixed at creation, because a group that became a human would
// strand its memberships — and so is disabled_at, which moves only through
// Disable and Enable so that removal is always an explicit act.
type Patch struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	// Availability is raw because three shapes mean three things: absent
	// leaves the week alone, `null` or `{}` clears it, an object replaces it.
	Availability json.RawMessage `json:"availability"`
	// IsAdministrator promotes or demotes a human. Absent leaves it alone.
	IsAdministrator *bool `json:"is_administrator"`
}

// Counts is the /health summary. Principals is every row, disabled included,
// and equals Humans + Groups; Disabled is how many of those carry disabled_at.
type Counts struct {
	Principals int `json:"principals"`
	Humans     int `json:"humans"`
	Groups     int `json:"groups"`
	Disabled   int `json:"disabled"`
}

// GroupMembership is what PUT /principals/{group}/members/{member} answers: the
// pair that now exists, and whether this call created it (201) or found it
// already there (200).
type GroupMembership struct {
	GroupID  string `json:"group_id"`
	MemberID string `json:"member_id"`
	Created  bool   `json:"created"`
}
