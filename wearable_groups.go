package possessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// WearableGroup is one group of wearables as GET /wearable-groups answers
// it: how many live members it has by status, and whether the owner closed
// it.
type WearableGroup struct {
	ID          *string `json:"id"` // wearable_group_…; null for a group never opened or closed by hand
	Group       string  `json:"group"`
	Closed      bool    `json:"closed"`
	Members     int     `json:"members"`
	Owned       int     `json:"owned"` // owned or lent out
	Unconfirmed int     `json:"unconfirmed"`
	Other       int     `json:"other"` // sold, given away, lost, discarded
	ChangedBy   string  `json:"changed_by"`
	ChangedAt   int64   `json:"changed_at"`
}

// ownedStatuses are the statuses of a closed group's members: what the
// owner has.
var ownedStatuses = []string{"owned", "lent_out"}

// checkGroupKey holds a group key to the ones wearableGroup makes.
func checkGroupKey(group string) error {
	kind, subType, hasSubType := strings.Cut(group, ":")
	switch {
	case (kind == "watch" || kind == "eyewear") && !hasSubType:
		return nil
	case kind == "garment" && contains(GarmentCategories, subType):
		return nil
	case kind == "jewelry" && contains(JewelryTypes, subType):
		return nil
	case kind == "bag_or_case" && contains(BagTypes, subType):
		return nil
	}
	return fmt.Errorf("%w: %q is not a wearable group; a group is watch, eyewear, garment:<category>, jewelry:<jewelry type> or bag_or_case:<bag type>", ErrInvalid, group)
}

// groupClosed says whether the owner closed a group.
func groupClosed(q querier, group string) (bool, error) {
	var closed int
	err := q.QueryRow(`SELECT closed FROM wearable_groups WHERE group_key = ?`, group).Scan(&closed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return closed == 1, err
}

// SetWearableGroupClosed closes or opens a group.
func (s *Store) SetWearableGroupClosed(ctx context.Context, group string, closed bool, actor string) (WearableGroup, error) {
	if actor == "" {
		return WearableGroup{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	if err := checkGroupKey(group); err != nil {
		return WearableGroup{}, err
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WearableGroup{}, err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	var id string
	err = transaction.QueryRow(`SELECT id FROM wearable_groups WHERE group_key = ?`, group).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var sequence int64
		if id, sequence, err = nextID(transaction, "wearable_groups", "wearable_group"); err != nil {
			return WearableGroup{}, err
		}
		if _, err := transaction.Exec(`INSERT INTO wearable_groups (id, seq, group_key, closed, changed_by, changed_at) VALUES (?, ?, ?, ?, ?, ?)`, id, sequence, group, boolToInteger(closed), actor, now); err != nil {
			return WearableGroup{}, err
		}
	case err != nil:
		return WearableGroup{}, err
	default:
		if err := saveRevision(transaction, "wearable_groups", "id", id, actor, now); err != nil {
			return WearableGroup{}, err
		}
		if _, err := transaction.Exec(`UPDATE wearable_groups SET closed = ?, changed_by = ?, changed_at = ? WHERE id = ?`, boolToInteger(closed), actor, now, id); err != nil {
			return WearableGroup{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return WearableGroup{}, err
	}
	groups, err := s.ListWearableGroups()
	if err != nil {
		return WearableGroup{}, err
	}
	for _, listed := range groups {
		if listed.Group == group {
			return listed, nil
		}
	}
	return WearableGroup{}, fmt.Errorf("%s was written but is not listed", group)
}

// ListWearableGroups answers every group that has a live member or a row.
func (s *Store) ListWearableGroups() ([]WearableGroup, error) {
	groups := map[string]*WearableGroup{}
	get := func(key string) *WearableGroup {
		if groups[key] == nil {
			groups[key] = &WearableGroup{Group: key}
		}
		return groups[key]
	}
	wearables, err := s.liveWearables()
	if err != nil {
		return nil, err
	}
	for _, possession := range wearables {
		group := get(possessionGroup(possession))
		group.Members++
		switch status := possession["status"].(string); {
		case contains(ownedStatuses, status):
			group.Owned++
		case status == "unconfirmed":
			group.Unconfirmed++
		default:
			group.Other++
		}
	}
	rows, err := s.db.Query(`SELECT id, group_key, closed, changed_by, changed_at FROM wearable_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, changedBy string
		var closed int
		var changedAt int64
		if err := rows.Scan(&id, &key, &closed, &changedBy, &changedAt); err != nil {
			return nil, err
		}
		group := get(key)
		group.ID, group.Closed, group.ChangedBy, group.ChangedAt = &id, closed == 1, changedBy, changedAt
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list := make([]WearableGroup, 0, len(groups))
	for _, group := range groups {
		list = append(list, *group)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Group < list[j].Group })
	return list, nil
}

// closedGroupMembers answers the owned members of a group, oldest first,
// leaving out the ids in exclude.
func (s *Store) closedGroupMembers(group string, exclude ...string) ([]map[string]any, error) {
	wearables, err := s.liveWearables()
	if err != nil {
		return nil, err
	}
	members := []map[string]any{}
	for _, possession := range wearables {
		if possessionGroup(possession) == group && contains(ownedStatuses, possession["status"].(string)) && !contains(exclude, possession["id"].(string)) {
			members = append(members, possession)
		}
	}
	return members, nil
}
