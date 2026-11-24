package principalstore

import (
	"context"
	"fmt"
	"time"
)

// requireKind loads a principal and checks it is the kind a membership needs
// it to be. A missing row is ErrNotFound; a row of the other kind is
// ErrInvalidMembership carrying the caller-facing explanation.
func (s *Store) requireKind(ctx context.Context, id, kind, explanation string) (*Principal, error) {
	p, err := s.getRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Kind != kind {
		return nil, fmt.Errorf("%w: %s is a %s, not a %s: %s", ErrInvalidMembership, id, p.Kind, kind, explanation)
	}
	return p, nil
}

func (s *Store) requireGroup(ctx context.Context, id string) (*Principal, error) {
	return s.requireKind(ctx, id, KindGroup, "memberships hang off a group")
}

// requireMember loads a principal and checks it may be a group's member: a
// kind that acts as a caller, which is a human or a peer.
func (s *Store) requireMember(ctx context.Context, id string) (*Principal, error) {
	p, err := s.getRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ActsAsCaller(p.Kind) {
		// The explanation is the no-nested-groups rule, stated where it bites:
		// a group in the member slot is the only way to nest, and v1 does not.
		return nil, fmt.Errorf("%w: %s is a %s, and a group's members are humans and peers: nested groups are not supported in v1 — add the group's members directly",
			ErrInvalidMembership, id, p.Kind)
	}
	return p, nil
}

// AddMember puts a human or a peer in a group. Idempotent: created reports whether this
// call wrote the row, so the handler can answer 201 the first time and 200 after.
func (s *Store) AddMember(ctx context.Context, groupID, memberID string) (created bool, err error) {
	group, err := s.requireGroup(ctx, groupID)
	if err != nil {
		return false, err
	}
	member, err := s.requireMember(ctx, memberID)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO group_members (group_seq, member_seq, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		group.Seq, member.Seq, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RemoveMember takes a human out of a group. ErrNotAMember if they were not in it.
func (s *Store) RemoveMember(ctx context.Context, groupID, memberID string) error {
	group, err := s.requireGroup(ctx, groupID)
	if err != nil {
		return err
	}
	member, err := s.getRow(ctx, memberID)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM group_members WHERE group_seq = $1 AND member_seq = $2`, group.Seq, member.Seq)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s is not a member of %s", ErrNotAMember, memberID, groupID)
	}
	return nil
}

// ListMembers returns the humans in a group, in display-name order.
// ErrInvalidMembership if the id is not a group.
func (s *Store) ListMembers(ctx context.Context, groupID string, includeDisabled bool) ([]*Principal, error) {
	group, err := s.requireGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return s.listRelated(ctx, `
		SELECT `+principalColumns+`
		FROM group_members gm JOIN principals p ON p.seq = gm.member_seq
		WHERE gm.group_seq = $1`, group.Seq, includeDisabled)
}

// ListAvailableMembers is ListMembers narrowed to the members available at
// the instant: the read kanban-store's assignment pool makes. Disabled
// members are never available, so there is no include_disabled here.
func (s *Store) ListAvailableMembers(ctx context.Context, groupID string, at time.Time) ([]*Principal, error) {
	members, err := s.ListMembers(ctx, groupID, false)
	if err != nil {
		return nil, err
	}
	return s.filterAvailable(ctx, members, at)
}

// ListGroups returns the groups a human belongs to, in display-name order.
// ErrInvalidMembership if the id is not a human.
func (s *Store) ListGroups(ctx context.Context, memberID string, includeDisabled bool) ([]*Principal, error) {
	member, err := s.requireMember(ctx, memberID)
	if err != nil {
		return nil, err
	}
	return s.listRelated(ctx, `
		SELECT `+principalColumns+`
		FROM group_members gm JOIN principals p ON p.seq = gm.group_seq
		WHERE gm.member_seq = $1`, member.Seq, includeDisabled)
}

func (s *Store) listRelated(ctx context.Context, query string, seq int64, includeDisabled bool) ([]*Principal, error) {
	if !includeDisabled {
		query += ` AND p.disabled_at = 0`
	}
	query += displayNameOrder
	rows, err := s.db.QueryContext(ctx, query, seq)
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
	return out, rows.Err()
}
