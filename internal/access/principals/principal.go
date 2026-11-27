package principals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const principalColumns = `
	p.seq, p.kind, p.display_name, p.email, p.disabled_at, p.created_at, p.updated_at, p.availability, p.is_administrator`

// displayNameOrder is how every list is sorted: by display name with ASCII
// letters folded to one case, then by number. It is the order the standalone
// store's SQLite gave (COLLATE NOCASE, which folds ASCII only, then bytes),
// written for Postgres: lower() under the C collation folds ASCII only, and
// the C collation compares bytes.
const displayNameOrder = ` ORDER BY lower(p.display_name COLLATE "C"), p.seq ASC`

func scanPrincipal(scan func(...any) error) (*Principal, error) {
	var p Principal
	var availability string
	err := scan(&p.Seq, &p.Kind, &p.DisplayName, &p.Email, &p.DisabledAt, &p.CreatedAt, &p.UpdatedAt, &availability, &p.IsAdministrator)
	if err != nil {
		return nil, err
	}
	p.ID = formatID(p.Seq)
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

// Create records a new principal and hands back its id. The number comes from
// the table's identity column, so it is never handed out twice.
func (s *Store) Create(ctx context.Context, input *Principal) (*Principal, error) {
	if err := validateForWrite(input); err != nil {
		return nil, err
	}
	ts := now()
	var seq int64
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO principals (kind, display_name, email, disabled_at, created_at, updated_at, search_document)
		VALUES ($1, $2, $3, 0, $4, $5, `+searchDocumentSQL(6, 7)+`) RETURNING seq`,
		input.Kind, input.DisplayName, input.Email, ts, ts, searchTextOf(input.DisplayName), searchTextOf(input.Email)).Scan(&seq); err != nil {
		return nil, err
	}
	return s.Get(ctx, formatID(seq), false)
}

// getRow reads one principal without expanding its memberships. Disabled rows
// are returned like any other: a card assigned to a departed human still has
// to render their name. An id that is not exactly the form this store hands
// out names no row.
func (s *Store) getRow(ctx context.Context, id string) (*Principal, error) {
	seq, ok := principalNumberOf(id)
	if !ok {
		return nil, fmt.Errorf("%w: principal %s", ErrNotFound, id)
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+principalColumns+` FROM principals p WHERE p.seq = $1`, seq)
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
func (s *Store) Get(ctx context.Context, id string, includeDisabledMemberships bool) (*Principal, error) {
	p, err := s.getRow(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case p.ActsAsCaller:
		if p.Groups, err = s.ListGroups(ctx, id, includeDisabledMemberships); err != nil {
			return nil, err
		}
	case p.Kind == KindGroup:
		if p.Members, err = s.ListMembers(ctx, id, includeDisabledMemberships); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// buildQuery assembles the shared WHERE for List and Count.
func (s *Store) buildQuery(f Filter, selectClause string) (string, []any, error) {
	var args []any
	argument := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	where := ` FROM principals p WHERE true`
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
		where += ` AND p.kind = ` + argument(kind)
	} else if f.AvailableAt != nil {
		where += ` AND p.kind = ` + argument(KindHuman)
	}
	if strings.TrimSpace(f.Query) != "" {
		query, err := searchTSQueryOf(f.Query)
		if err != nil {
			return "", nil, err
		}
		if query == "" {
			where += ` AND false`
		} else {
			where += ` AND p.search_document @@ to_tsquery('simple', ` + argument(query) + `)`
		}
	}
	return selectClause + where, args, nil
}

// List returns principals in display-name order. Memberships are not expanded
// here; Get does that for one row.
//
// With AvailableAt set the week is evaluated in Go, so the page is cut after
// the filter rather than before — otherwise a page of ten could come back as
// three with seven more hiding behind the next offset.
func (s *Store) List(ctx context.Context, f Filter) ([]*Principal, error) {
	query, args, err := s.buildQuery(f, `SELECT `+principalColumns)
	if err != nil {
		return nil, err
	}
	query += displayNameOrder
	if f.AvailableAt == nil {
		query += pageClause(f.Limit, f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
		if out, err = s.filterAvailable(ctx, out, *f.AvailableAt); err != nil {
			return nil, err
		}
		out = page(out, f.Limit, f.Offset)
	}
	return out, nil
}

// pageClause is the LIMIT and OFFSET of a page. A limit or offset of zero or
// less is no limit and no offset, as the standalone store read them.
func pageClause(limit, offset int) string {
	clause := ""
	if limit > 0 {
		clause += fmt.Sprintf(` LIMIT %d`, limit)
	}
	if offset > 0 {
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
func (s *Store) Count(ctx context.Context, f Filter) (int, error) {
	if f.AvailableAt != nil {
		f.Limit, f.Offset = 0, 0
		principals, err := s.List(ctx, f)
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
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Counts is the /health summary over every row, disabled included.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE kind = $1),
		       COUNT(*) FILTER (WHERE kind = $2),
		       COUNT(*) FILTER (WHERE disabled_at > 0)
		FROM principals`, KindHuman, KindGroup).
		Scan(&c.Principals, &c.Humans, &c.Groups, &c.Disabled)
	return c, err
}

// Patch edits the display fields. It refuses kind and disabled_at by having
// nowhere to put them: kind is fixed at creation, and disabled_at moves through
// Disable and Enable so removal is always an explicit act.
func (s *Store) Patch(ctx context.Context, id string, patch Patch) (*Principal, error) {
	current, err := s.getRow(ctx, id)
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
	_, err = s.db.ExecContext(ctx, `UPDATE principals SET display_name = $1, email = $2, availability = $3, is_administrator = $4, updated_at = $5,
		search_document = `+searchDocumentSQL(7, 8)+` WHERE seq = $6`,
		current.DisplayName, current.Email, encoded, current.IsAdministrator, now(), current.Seq,
		searchTextOf(current.DisplayName), searchTextOf(current.Email))
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id, false)
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
func (s *Store) Disable(ctx context.Context, id string) (*Principal, error) {
	current, err := s.getRow(ctx, id)
	if err != nil {
		return nil, err
	}
	ts := now()
	if _, err := s.db.ExecContext(ctx, `UPDATE principals SET disabled_at = $1, updated_at = $2 WHERE seq = $3 AND disabled_at = 0`, ts, ts, current.Seq); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, false)
}

// Enable undoes Disable. Idempotent.
func (s *Store) Enable(ctx context.Context, id string) (*Principal, error) {
	current, err := s.getRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE principals SET disabled_at = 0, updated_at = $1 WHERE seq = $2 AND disabled_at <> 0`, now(), current.Seq); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, false)
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
