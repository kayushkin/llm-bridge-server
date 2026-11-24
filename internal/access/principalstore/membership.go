package principalstore

import (
	"fmt"
	"time"
)

// requireKind loads a principal and checks it is the kind a membership needs
// it to be. A missing row is ErrNotFound; a row of the other kind is
// ErrInvalidMembership carrying the caller-facing explanation.
func (s *Store) requireKind(id, kind, explanation string) (*Principal, error) {
	p, err := s.getRow(id)
	if err != nil {
		return nil, err
	}
	if p.Kind != kind {
		return nil, fmt.Errorf("%w: %s is a %s, not a %s: %s", ErrInvalidMembership, id, p.Kind, kind, explanation)
	}
	return p, nil
}

func (s *Store) requireGroup(id string) (*Principal, error) {
	return s.requireKind(id, KindGroup, "memberships hang off a group")
}

// requireMember loads a principal and checks it may be a group's member: a
// kind that acts as a caller, which is a human or a peer.
func (s *Store) requireMember(id string) (*Principal, error) {
	p, err := s.getRow(id)
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
func (s *Store) AddMember(groupID, memberID string) (created bool, err error) {
	if _, err := s.requireGroup(groupID); err != nil {
		return false, err
	}
	if _, err := s.requireMember(memberID); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO group_members (group_id, member_id, created_at) VALUES (?,?,?)`,
		groupID, memberID, now())
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
func (s *Store) RemoveMember(groupID, memberID string) error {
	if _, err := s.requireGroup(groupID); err != nil {
		return err
	}
	if _, err := s.getRow(memberID); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM group_members WHERE group_id=? AND member_id=?`, groupID, memberID)
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
func (s *Store) ListMembers(groupID string, includeDisabled bool) ([]*Principal, error) {
	if _, err := s.requireGroup(groupID); err != nil {
		return nil, err
	}
	return s.listRelated(`
		SELECT `+principalColumns+`
		FROM group_members gm JOIN principals p ON p.id = gm.member_id
		WHERE gm.group_id = ?`, groupID, includeDisabled)
}

// ListAvailableMembers is ListMembers narrowed to the members available at
// the instant: the read kanban-store's assignment pool makes. Disabled
// members are never available, so there is no include_disabled here.
func (s *Store) ListAvailableMembers(groupID string, at time.Time) ([]*Principal, error) {
	members, err := s.ListMembers(groupID, false)
	if err != nil {
		return nil, err
	}
	return s.filterAvailable(members, at)
}

// ListGroups returns the groups a human belongs to, in display-name order.
// ErrInvalidMembership if the id is not a human.
func (s *Store) ListGroups(memberID string, includeDisabled bool) ([]*Principal, error) {
	if _, err := s.requireMember(memberID); err != nil {
		return nil, err
	}
	return s.listRelated(`
		SELECT `+principalColumns+`
		FROM group_members gm JOIN principals p ON p.id = gm.group_id
		WHERE gm.member_id = ?`, memberID, includeDisabled)
}

func (s *Store) listRelated(query, id string, includeDisabled bool) ([]*Principal, error) {
	if !includeDisabled {
		query += ` AND p.disabled_at = 0`
	}
	query += ` ORDER BY p.display_name COLLATE NOCASE ASC, p.seq ASC`
	rows, err := s.db.Query(query, id)
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
