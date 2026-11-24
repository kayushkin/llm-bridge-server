package grantstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const grantColumns = `id, seq, principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at`

func scanGrant(scan func(...any) error) (*Grant, error) {
	var g Grant
	if err := scan(&g.ID, &g.Seq, &g.PrincipalID, &g.Relation, &g.ResourceType, &g.ResourceID, &g.Note, &g.GrantedAt, &g.RevokedAt); err != nil {
		return nil, err
	}
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	existing, err := scanGrant(tx.QueryRowContext(ctx, `
		SELECT `+grantColumns+` FROM grants
		WHERE principal_id = ? AND relation = ? AND resource_type = ? AND resource_id = ? AND revoked_at = 0`,
		request.PrincipalID, relation.Name, definition.name, request.ResourceID).Scan)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM grants`).Scan(&maxSeq); err != nil {
		return nil, false, err
	}
	seq := maxSeq.Int64 + 1
	id := formatID(seq)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO grants (id, seq, principal_id, relation, resource_type, resource_id, note, granted_at, revoked_at)
		VALUES (?,?,?,?,?,?,?,?,0)`,
		id, seq, request.PrincipalID, relation.Name, definition.name, request.ResourceID, strings.TrimSpace(request.Note), now()); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	created, err := s.Get(id)
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
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
func (s *Store) Get(id string) (*Grant, error) {
	g, err := scanGrant(s.db.QueryRow(`SELECT `+grantColumns+` FROM grants WHERE id = ?`, id).Scan)
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
func (s *Store) Revoke(id string) (*Grant, error) {
	if _, err := s.db.Exec(`UPDATE grants SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// List returns grants matching the filter, ordered by resource_type,
// resource_id, relation, principal_id, then seq. Revoked grants are hidden
// unless IncludeRevoked. An unknown relation or resource type in the filter
// is ErrInvalidGrant rather than an empty answer, so a misspelling is not
// mistaken for "nobody has this".
func (s *Store) List(f Filter) ([]*Grant, error) {
	where := ` WHERE 1=1`
	var args []any
	if !f.IncludeRevoked {
		where += ` AND revoked_at = 0`
	}
	if f.PrincipalID != "" {
		where += ` AND principal_id = ?`
		args = append(args, f.PrincipalID)
	}
	if f.Relation != "" {
		if _, err := lookupRelation(f.Relation); err != nil {
			return nil, err
		}
		where += ` AND relation = ?`
		args = append(args, f.Relation)
	}
	if f.ResourceType != "" {
		if _, err := lookupResourceType(f.ResourceType); err != nil {
			return nil, err
		}
		where += ` AND resource_type = ?`
		args = append(args, f.ResourceType)
	}
	if f.ResourceID != "" {
		where += ` AND resource_id = ?`
		args = append(args, f.ResourceID)
	}
	query := `SELECT ` + grantColumns + ` FROM grants` + where + ` ORDER BY resource_type, resource_id, relation, principal_id, seq`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
		if f.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, f.Offset)
		}
	}
	return s.queryGrants(query, args...)
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
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(holders)), ",")
	args := make([]any, 0, len(holders)+3)
	for _, holder := range holders {
		args = append(args, holder)
	}
	where := ` WHERE revoked_at = 0 AND principal_id IN (` + placeholders + `)`
	if relation != "" {
		where += ` AND relation = ?`
		args = append(args, relation)
	}
	if resourceType != "" {
		where += ` AND resource_type = ?`
		args = append(args, resourceType)
	}
	// principal_id <> ? sorts false (0, the principal's own row) before true.
	query := `SELECT ` + grantColumns + ` FROM grants` + where +
		` ORDER BY resource_type, resource_id, relation, principal_id <> ?, principal_id`
	args = append(args, principalID)
	return s.queryGrants(query, args...)
}

func (s *Store) queryGrants(query string, args ...any) ([]*Grant, error) {
	rows, err := s.db.Query(query, args...)
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
