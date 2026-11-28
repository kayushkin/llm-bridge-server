package possessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
	// ExpectedLow and ExpectedHigh are how many individual pieces the
	// owner says the group holds; null when not said.
	ExpectedLow  *int   `json:"expected_low"`
	ExpectedHigh *int   `json:"expected_high"`
	ChangedBy    string `json:"changed_by"`
	ChangedAt    int64  `json:"changed_at"`
}

// WearableGroupChange is the body of PUT /wearable-groups/{group}: what is
// given changes, what is left out stays.
type WearableGroupChange struct {
	Closed       *bool  `json:"closed"`
	ExpectedLow  *int   `json:"expected_low"`
	ExpectedHigh *int   `json:"expected_high"`
	Actor        string `json:"actor"`
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
	case kind == "garment" && (slices.Contains(GarmentCategories, subType) || slices.Contains(GroupedGarmentTypes, subType)):
		return nil
	case kind == "jewelry" && slices.Contains(JewelryTypes, subType) && (subType == "necklace" || !slices.Contains(NecklaceJewelryTypes, subType)):
		return nil
	case kind == "bag_or_case" && slices.Contains(BagTypes, subType):
		return nil
	}
	return fmt.Errorf("%w: %q is not a wearable group; a group is watch, eyewear, garment:<category>, garment:<%s>, jewelry:<jewelry type> (pendants and chains are jewelry:necklace) or bag_or_case:<bag type>",
		ErrInvalid, group, strings.Join(GroupedGarmentTypes, "|"))
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
	return s.ChangeWearableGroup(ctx, group, WearableGroupChange{Closed: &closed, Actor: actor})
}

// ChangeWearableGroup closes or opens a group and sets how many pieces the
// owner expects in it.
func (s *Store) ChangeWearableGroup(ctx context.Context, group string, change WearableGroupChange) (WearableGroup, error) {
	actor := change.Actor
	if actor == "" {
		return WearableGroup{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	if change.Closed == nil && change.ExpectedLow == nil && change.ExpectedHigh == nil {
		return WearableGroup{}, fmt.Errorf("%w: give closed, expected_low or expected_high", ErrInvalid)
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
	var closed int
	var low, high sql.NullInt64
	err = transaction.QueryRow(`SELECT id, closed, expected_low, expected_high FROM wearable_groups WHERE group_key = ?`, group).Scan(&id, &closed, &low, &high)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WearableGroup{}, err
	}
	if change.Closed != nil {
		closed = boolToInteger(*change.Closed)
	}
	if change.ExpectedLow != nil {
		low = sql.NullInt64{Int64: int64(*change.ExpectedLow), Valid: true}
	}
	if change.ExpectedHigh != nil {
		high = sql.NullInt64{Int64: int64(*change.ExpectedHigh), Valid: true}
	}
	if (low.Valid && low.Int64 < 0) || (high.Valid && high.Int64 < 1) || (low.Valid && high.Valid && low.Int64 > high.Int64) {
		return WearableGroup{}, fmt.Errorf("%w: expected_low is 0 or more, expected_high 1 or more and not below expected_low", ErrInvalid)
	}
	if exists {
		if err := saveRevision(transaction, "wearable_groups", "id", id, actor, now); err != nil {
			return WearableGroup{}, err
		}
		if _, err := transaction.Exec(`UPDATE wearable_groups SET closed = ?, expected_low = ?, expected_high = ?, changed_by = ?, changed_at = ? WHERE id = ?`, closed, low, high, actor, now, id); err != nil {
			return WearableGroup{}, err
		}
	} else {
		var sequence int64
		if id, sequence, err = nextID(transaction, "wearable_groups", "wearable_group"); err != nil {
			return WearableGroup{}, err
		}
		if _, err := transaction.Exec(`INSERT INTO wearable_groups (id, seq, group_key, closed, expected_low, expected_high, changed_by, changed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, sequence, group, closed, low, high, actor, now); err != nil {
			return WearableGroup{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return WearableGroup{}, err
	}
	return s.GetWearableGroup(group)
}

// GetWearableGroup answers one group, with or without a row.
func (s *Store) GetWearableGroup(group string) (WearableGroup, error) {
	if err := checkGroupKey(group); err != nil {
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
	return WearableGroup{Group: group}, nil
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
		if possessionGroup(possession) == "" {
			continue
		}
		group := get(possessionGroup(possession))
		group.Members++
		switch status := possession["status"].(string); {
		case slices.Contains(ownedStatuses, status):
			group.Owned++
		case status == "unconfirmed":
			group.Unconfirmed++
		default:
			group.Other++
		}
	}
	rows, err := s.db.Query(`SELECT id, group_key, closed, expected_low, expected_high, changed_by, changed_at FROM wearable_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, changedBy string
		var closed int
		var changedAt int64
		var low, high sql.NullInt64
		if err := rows.Scan(&id, &key, &closed, &low, &high, &changedBy, &changedAt); err != nil {
			return nil, err
		}
		group := get(key)
		group.ID, group.Closed, group.ChangedBy, group.ChangedAt = &id, closed == 1, changedBy, changedAt
		if low.Valid {
			value := int(low.Int64)
			group.ExpectedLow = &value
		}
		if high.Valid {
			value := int(high.Int64)
			group.ExpectedHigh = &value
		}
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
		if possessionGroup(possession) == group && slices.Contains(ownedStatuses, possession["status"].(string)) && !slices.Contains(exclude, possession["id"].(string)) {
			members = append(members, possession)
		}
	}
	return members, nil
}

// wearableGroup is the group a wearable is compared and counted within, as
// the owner thinks of them (2026-11-07): a garment's category (shirts are
// tops, pants bottoms, jackets outerwear), except the garment types in
// GroupedGarmentTypes, which are groups of their own (scarves); a jewelry
// type, with pendants and chains counted as necklaces; a bag type; watches
// and eyewear one group each. Socks are in no group. "" for a possession
// that is not in a group.
func wearableGroup(kind string, fields func(name string) string) string {
	switch kind {
	case "garment":
		garmentType := fields("garment_type")
		if slices.Contains(ungroupedGarmentTypes, garmentType) {
			return ""
		}
		if slices.Contains(GroupedGarmentTypes, garmentType) {
			return "garment:" + garmentType
		}
		return "garment:" + fields("category")
	case "jewelry":
		jewelryType := fields("jewelry_type")
		if slices.Contains(NecklaceJewelryTypes, jewelryType) {
			jewelryType = "necklace"
		}
		return "jewelry:" + jewelryType
	case "bag_or_case":
		return "bag_or_case:" + fields("bag_type")
	case "watch", "eyewear":
		return kind
	}
	return ""
}

func possessionGroup(possession map[string]any) string {
	kind := possession["kind"].(string)
	if !slices.Contains(WearableKinds, kind) {
		return ""
	}
	record := possession[kind].(map[string]any)
	return wearableGroup(kind, func(name string) string { value, _ := record[name].(string); return value })
}

// liveWearables answers every live possession of a wearable kind, oldest
// first.
func (s *Store) liveWearables() ([]map[string]any, error) {
	all := []map[string]any{}
	for _, kind := range WearableKinds {
		possessions, err := s.ListPossessions(PossessionFilter{Kind: kind, Limit: 1000})
		if err != nil {
			return nil, err
		}
		if len(possessions) == 1000 {
			return nil, fmt.Errorf("there are 1000 or more %s possessions; the audit reads at most 1000 of a kind", kind)
		}
		all = append(all, possessions...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i]["id"].(string) < all[j]["id"].(string) })
	return all, nil
}
