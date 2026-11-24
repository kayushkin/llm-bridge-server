package grantstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

const grantColumns = `seq, principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at`

func scanGrant(scan func(...any) error) (*Grant, error) {
	var g Grant
	if err := scan(&g.Seq, &g.PrincipalID, &g.Relation, &g.ResourceType, &g.ResourceID, &g.Note, &g.GrantedAt, &g.RevokedAt); err != nil {
		return nil, err
	}
	g.ID = formatID(g.Seq)
	return &g, nil
}

// GrantRequest is what a caller sends to create a grant.
type GrantRequest struct {
	PrincipalID  string `json:"principal_id"`
	Relation     string `json:"relation"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Note         string `json:"note"`
}

// Create records a grant once principal-store confirms the principal exists
// and the resource's owner confirms the resource exists. Idempotent on the
// active tuple: created reports whether this call wrote the row, and the row
// returned is always the stored one, so a repeat answers with the original
// granted_at and note.
//
// The checks run in the order a caller can act on them: the relation, the
// resource type and whether the relation allows it, the id shapes, then the
// principal, then the resource. Both owners are asked before anything is
// written, so an owner that is down leaves no row behind (ErrOwnerUnavailable)
// — a grant naming an id nobody could confirm is exactly what the checks keep
// out.
//
// A disabled principal is accepted: a grant to a departed human is dormant,
// and a re-enabled one comes back with their grants intact. So is a disabled
// resource, for the same reason.
func (s *Store) Create(ctx context.Context, directory PrincipalDirectory, checker ResourceChecker, request GrantRequest) (*Grant, bool, error) {
	relation, err := lookupRelation(request.Relation)
	if err != nil {
		return nil, false, err
	}
	definition, err := lookupResourceType(request.ResourceType)
	if err != nil {
		return nil, false, err
	}
	if !relation.allowsResourceType(definition.name) {
		return nil, false, fmt.Errorf("%w: relation %s does not apply to a %s: it applies to %s",
			ErrInvalidGrant, relation.Name, definition.name, strings.Join(relation.ResourceTypes, ", "))
	}
	grantsAllResources := request.ResourceID == AllResources
	if grantsAllResources && !relation.AcceptsAllResources {
		return nil, false, fmt.Errorf("%w: relation %s does not accept resource_id %q, because %s: name one %s",
			ErrInvalidGrant, relation.Name, AllResources, relation.allResourcesRefusal, definition.name)
	}
	if !grantsAllResources {
		if err := validateResourceID(definition, request.ResourceID); err != nil {
			return nil, false, err
		}
	}
	if err := validatePrincipalID(request.PrincipalID); err != nil {
		return nil, false, err
	}
	summary, err := directory.LookupPrincipal(ctx, request.PrincipalID)
	if err != nil {
		if errors.Is(err, ErrPrincipalNotFound) {
			return nil, false, fmt.Errorf("%w: principal %s does not exist in principal-store",
				ErrInvalidGrant, request.PrincipalID)
		}
		return nil, false, fmt.Errorf("%w: could not confirm principal %s exists in principal-store, so nothing was written: %v",
			ErrOwnerUnavailable, request.PrincipalID, err)
	}
	// A contact is the outside person a ticket came from — a requester, not
	// someone this deployment gives anything to. Granting one anything would
	// hand a customer a tool, an agent or a board, so it is refused here
	// rather than left to whoever reads the grant later. principal-store says
	// which kinds hold grants; this store reads its answer.
	if !summary.ActsInThisDeployment {
		return nil, false, fmt.Errorf("%w: principal %s is a %s, and a %s holds no grants — it is the outside party on a ticket, not someone this deployment gives access to",
			ErrInvalidGrant, request.PrincipalID, summary.Kind, summary.Kind)
	}
	// "*" names no row, so there is no owner to ask; it covers whatever the
	// owner holds now and later.
	if !grantsAllResources {
		if err := checker.CheckResourceExists(ctx, definition.name, request.ResourceID); err != nil {
			if errors.Is(err, ErrResourceNotFound) {
				return nil, false, fmt.Errorf("%w: %s %s does not exist in %s",
					ErrInvalidGrant, definition.name, request.ResourceID, definition.owner)
			}
			return nil, false, fmt.Errorf("%w: could not confirm %s %s exists in %s, so nothing was written: %v",
				ErrOwnerUnavailable, definition.name, request.ResourceID, definition.owner, err)
		}
	}

	// The active tuple is unique by the partial index, so a create that
	// finds its row already there answers with it. Two creates of the same
	// tuple at once both miss it; the insert of the second meets the index and
	// does nothing, and it reads the first's row.
	tuple := []any{request.PrincipalID, relation.Name, definition.name, request.ResourceID}
	existing, err := s.activeGrant(ctx, tuple)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var seq int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO grants (principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, 0)
		ON CONFLICT (principal_id, relation, resource_type, resource_id) WHERE revoked_at = 0 DO NOTHING
		RETURNING seq`,
		request.PrincipalID, relation.Name, definition.name, request.ResourceID, strings.TrimSpace(request.Note), now()).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		existing, err := s.activeGrant(ctx, tuple)
		if err != nil {
			return nil, false, fmt.Errorf("another create of the same grant won, and its row could not be read: %w", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	created, err := s.Get(ctx, formatID(seq))
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
}

// activeGrant is the active row of one tuple, or sql.ErrNoRows.
func (s *Store) activeGrant(ctx context.Context, tuple []any) (*Grant, error) {
	return scanGrant(s.db.QueryRowContext(ctx, `
		SELECT `+grantColumns+` FROM grants
		WHERE principal_id = $1 AND relation = $2 AND resource_type = $3 AND resource_id = $4 AND revoked_at = 0`,
		tuple...).Scan)
}

// validatePrincipalID checks the shape principal-store hands out. Nothing is
// trimmed: a caller that sent whitespace has a bug upstream that a silent fix
// would hide.
func validatePrincipalID(principalID string) error {
	if principalID == "" {
		return fmt.Errorf("%w: principal_id is required: send principal-store's id, e.g. principal_000001", ErrInvalidGrant)
	}
	if strings.TrimSpace(principalID) != principalID {
		return fmt.Errorf("%w: principal_id %q has surrounding whitespace, and nothing is trimmed: send principal-store's id exactly",
			ErrInvalidGrant, principalID)
	}
	if !strings.HasPrefix(principalID, "principal_") {
		return fmt.Errorf("%w: principal_id %q is not a principal-store id (principal_000001): a display name or email is not accepted, because neither is unique",
			ErrInvalidGrant, principalID)
	}
	return nil
}

// Get returns one grant, revoked or not.
//
// Only the exact form of an id names a row: grant_1 or a name is not found,
// the same answer as a number nobody was given.
func (s *Store) Get(ctx context.Context, id string) (*Grant, error) {
	seq, err := postgresstorage.ParseRecordID(GrantIDPrefix, id)
	if err != nil || formatID(seq) != id {
		return nil, fmt.Errorf("%w: grant %s", ErrNotFound, id)
	}
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM grants WHERE seq = $1`, seq).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: grant %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Revoke marks a grant revoked. Idempotent: a grant already revoked is
// returned unchanged, with its original revoked_at, so a repeat does not move
// the timestamp. There is no delete.
func (s *Store) Revoke(ctx context.Context, id string) (*Grant, error) {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE grants SET revoked_at = $1 WHERE seq = $2 AND revoked_at = 0`, now(), existing.Seq); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// List returns grants matching the filter, ordered by resource_type,
// resource_id, relation, principal_id, then seq. Revoked grants are hidden
// unless IncludeRevoked. An unknown relation or resource type in the filter
// is ErrInvalidGrant rather than an empty answer, so a misspelling is not
// mistaken for "nobody has this".
func (s *Store) List(ctx context.Context, f Filter) ([]*Grant, error) {
	where := ` WHERE 1=1`
	var args []any
	placeholder := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if !f.IncludeRevoked {
		where += ` AND revoked_at = 0`
	}
	if f.PrincipalID != "" {
		where += ` AND principal_id = ` + placeholder(f.PrincipalID)
	}
	if f.Relation != "" {
		if _, err := lookupRelation(f.Relation); err != nil {
			return nil, err
		}
		where += ` AND relation = ` + placeholder(f.Relation)
	}
	if f.ResourceType != "" {
		if _, err := lookupResourceType(f.ResourceType); err != nil {
			return nil, err
		}
		where += ` AND resource_type = ` + placeholder(f.ResourceType)
	}
	if f.ResourceID != "" {
		where += ` AND resource_id = ` + placeholder(f.ResourceID)
	}
	// COLLATE "C" orders by bytes, as SQLite's BINARY did, whatever the
	// database's own collation is.
	query := `SELECT ` + grantColumns + ` FROM grants` + where +
		` ORDER BY resource_type COLLATE "C", resource_id COLLATE "C", relation COLLATE "C", principal_id COLLATE "C", seq`
	if f.Limit > 0 {
		query += ` LIMIT ` + placeholder(f.Limit)
		if f.Offset > 0 {
			query += ` OFFSET ` + placeholder(f.Offset)
		}
	}
	return s.queryGrants(ctx, query, args...)
}

// Effective returns every active grant a principal holds, directly or through
// an active group it belongs to: what a session started as this principal is
// offered. Groups come live from principal-store on every call, so a
// membership change is visible at the next session start without this store
// being told.
//
// Each row is the grant as stored, so its principal_id says whether it is the
// principal's own (equal to the id asked for) or inherited (a group's id). The
// same resource granted both ways appears twice, because both are true, and
// whoever is about to revoke the direct one needs to see the group still
// carries it. Ordered by resource_type, resource_id, relation, the principal's
// own row before inherited ones, then principal_id.
//
// A principal that does not exist is ErrNotFound; a directory that could not
// be asked is ErrOwnerUnavailable, never an empty answer — an empty answer
// would spawn a session with nothing offered and no explanation.
func (s *Store) Effective(ctx context.Context, directory PrincipalDirectory, principalID, relation, resourceType string) ([]*Grant, error) {
	if relation != "" {
		if _, err := lookupRelation(relation); err != nil {
			return nil, err
		}
	}
	if resourceType != "" {
		if _, err := lookupResourceType(resourceType); err != nil {
			return nil, err
		}
	}
	summary, err := directory.LookupPrincipal(ctx, principalID)
	if err != nil {
		if errors.Is(err, ErrPrincipalNotFound) {
			return nil, fmt.Errorf("%w: principal %s does not exist in principal-store", ErrNotFound, principalID)
		}
		return nil, fmt.Errorf("%w: could not read principal %s from principal-store, so the effective set is unknown: %v",
			ErrOwnerUnavailable, principalID, err)
	}
	holders := append([]string{principalID}, summary.GroupIDs...)
	args := []any{holders, principalID}
	where := ` WHERE revoked_at = 0 AND principal_id = ANY($1::text[])`
	if relation != "" {
		args = append(args, relation)
		where += fmt.Sprintf(` AND relation = $%d`, len(args))
	}
	if resourceType != "" {
		args = append(args, resourceType)
		where += fmt.Sprintf(` AND resource_type = $%d`, len(args))
	}
	// principal_id <> $2 sorts false (the principal's own row) before true.
	query := `SELECT ` + grantColumns + ` FROM grants` + where +
		` ORDER BY resource_type COLLATE "C", resource_id COLLATE "C", relation COLLATE "C", principal_id <> $2, principal_id COLLATE "C"`
	return s.queryGrants(ctx, query, args...)
}

func (s *Store) queryGrants(ctx context.Context, query string, args ...any) ([]*Grant, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Never nil: the empty answer is [] on the wire, not null.
	out := []*Grant{}
	for rows.Next() {
		g, err := scanGrant(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Grant is one relationship tuple.
type Grant struct {
	ID           string `json:"id"`
	Seq          int64  `json:"seq"`
	PrincipalID  string `json:"principal_id"`
	Relation     string `json:"relation"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Note         string `json:"note"`
	GrantedAt    int64  `json:"granted_at"`
	RevokedAt    int64  `json:"revoked_at"`
}

// Counts is the /health summary.
type Counts struct {
	Grants     int `json:"grants"`
	Active     int `json:"active"`
	Revoked    int `json:"revoked"`
	Principals int `json:"principals"` // distinct principals holding an active grant
}
