package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// possessionMergeMoves is what one merge changed, row by row, so that undo
// can put back exactly that and nothing else.
type possessionMergeMoves struct {
	// FilledFields are the kept possession's blank fields the duplicate
	// filled, with the kept possession's value before the merge.
	FilledFields map[string]any `json:"filled_fields"`
	// KindRecordMoved is the duplicate's kind record, moved to the kept
	// possession because it had no kind: {table, id}.
	KindRecordMoved *movedKindRecord `json:"kind_record_moved,omitempty"`
	// DuplicateKindRecordID is the duplicate's kind record when both had the
	// same kind; parts and a machine moved from it to the kept one.
	DuplicateKindRecordID    string   `json:"duplicate_kind_record_id,omitempty"`
	KeptKindRecordID         string   `json:"kept_kind_record_id,omitempty"`
	InstalledPartsMoved      []string `json:"installed_parts_moved"`
	HarnessMachineMoved      string   `json:"harness_machine_moved,omitempty"`
	OdometerReadingsInserted []int64  `json:"odometer_readings_inserted"` // read_at of each reading copied to the kept vehicle
	// OutfitMemberships are the duplicate's places in outfits. AddedToKept
	// says the kept possession was not already in that outfit and took the
	// place; otherwise the duplicate's place was only removed.
	OutfitMemberships  []outfitMembership `json:"outfit_memberships"`
	PhotosInserted     []string           `json:"photos_inserted"` // photo ids linked to the kept possession by the merge
	CoverSetPhotoID    string             `json:"cover_set_photo_id,omitempty"`
	SightingsRepointed []string           `json:"sightings_repointed"`
	SlotsRepointed     []string           `json:"slots_repointed"` // outfit slots linked to the duplicate, now to the kept one
}

type movedKindRecord struct {
	Table string `json:"table"`
	ID    string `json:"id"`
}

type outfitMembership struct {
	OutfitID     string `json:"outfit_id"`
	PossessionID string `json:"possession_id,omitempty"`
	Position     int    `json:"position"`
	AddedToKept  bool   `json:"added_to_kept,omitempty"`
}

// PossessionMerge is one merge as the API answers it.
type PossessionMerge struct {
	ID                    string               `json:"id"`
	DuplicatePossessionID string               `json:"duplicate_possession_id"`
	KeptPossessionID      string               `json:"kept_possession_id"`
	MergedBy              string               `json:"merged_by"`
	Reason                string               `json:"reason"`
	Moved                 possessionMergeMoves `json:"moved"`
	MergedAt              int64                `json:"merged_at"`
	UndoneBy              string               `json:"undone_by"`
	UndoneAt              int64                `json:"undone_at"`
}

// MergePossession folds a duplicate into the possession it duplicates. The
// kept possession takes the duplicate's photos, sightings, kind (when it has
// none of its own), and any field it leaves blank; outfits and installed
// parts that pointed at the duplicate's kind record point at the kept one.
// The duplicate is deleted with merged_into_id set, so its id still answers
// with include_deleted and says where it went. Two possessions of different
// kinds are a conflict: change one's kind first. Every merge is recorded as
// a possession_merge, which POST /possession-merges/{id}/undo reverses; the
// answer is the kept possession with merge_id.
func (s *Store) MergePossession(ctx context.Context, duplicateID, keptID, actor, reason string) (map[string]any, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	mergeID, err := s.mergePossessionInTransaction(transaction, duplicateID, keptID, actor, reason, s.unixNow())
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	kept, err := s.GetPossession(keptID, false)
	if err != nil {
		return nil, err
	}
	kept["merge_id"] = mergeID
	return kept, nil
}

func (s *Store) mergePossessionInTransaction(transaction *databaseTransaction, duplicateID, keptID, actor, reason string, now int64) (string, error) {
	if duplicateID == keptID {
		return "", fmt.Errorf("%w: a possession cannot be merged into itself", ErrInvalid)
	}
	duplicate, err := possessionView(transaction, duplicateID, false)
	if err != nil {
		return "", err
	}
	kept, err := possessionView(transaction, keptID, false)
	if err != nil {
		return "", err
	}
	duplicateKind, keptKind := duplicate["kind"].(string), kept["kind"].(string)
	if duplicateKind != "" && keptKind != "" && duplicateKind != keptKind {
		return "", fmt.Errorf("%w: %s is a %s and %s a %s; give them one kind before merging", ErrConflict, duplicateID, duplicateKind, keptID, keptKind)
	}
	for _, id := range []string{duplicateID, keptID} {
		if err := saveRevision(transaction, "possessions", "id", id, actor, now); err != nil {
			return "", err
		}
	}
	moves := possessionMergeMoves{FilledFields: map[string]any{}, InstalledPartsMoved: []string{}, OdometerReadingsInserted: []int64{},
		OutfitMemberships: []outfitMembership{}, PhotosInserted: []string{}, SightingsRepointed: []string{}, SlotsRepointed: []string{}}

	// Blank fields of the kept possession take the duplicate's values.
	fill := map[string]json.RawMessage{}
	for _, name := range []string{"brand", "model", "serial_number", "description", "condition", "spot", "acquired_how", "price_currency", "room"} {
		if kept[name] == "" && duplicate[name] != "" && duplicate[name] != nil {
			fill[name], _ = json.Marshal(duplicate[name])
			moves.FilledFields[name] = kept[name]
		}
	}
	for _, name := range []string{"place_id", "purchase_line_id", "product_id", "budget_transaction_id", "price_cents"} {
		if kept[name] == nil && duplicate[name] != nil {
			fill[name], _ = json.Marshal(duplicate[name])
			moves.FilledFields[name] = nil
		}
	}
	if kept["acquired_at"] == int64(0) && duplicate["acquired_at"] != int64(0) {
		fill["acquired_at"], _ = json.Marshal(duplicate["acquired_at"])
		moves.FilledFields["acquired_at"] = int64(0)
	}
	if len(fill) > 0 {
		values, err := possessionType.decodeFields(fill, false)
		if err != nil {
			return "", err
		}
		if err := updateRecord(transaction, possessionType, "id", keptID, map[string]any{"updated_at": now}, values); err != nil {
			return "", err
		}
	}

	// The kind.
	if duplicateKind != "" {
		kind, _ := KindByName(duplicateKind)
		duplicateKindID := duplicate[duplicateKind].(map[string]any)["id"].(string)
		if keptKind == "" {
			if _, err := transaction.Exec(`UPDATE `+kind.Table+` SET possession_id = ? WHERE id = ?`, keptID, duplicateKindID); err != nil {
				return "", err
			}
			moves.KindRecordMoved = &movedKindRecord{Table: kind.Table, ID: duplicateKindID}
		} else {
			keptKindRecord := kept[keptKind].(map[string]any)
			keptKindID := keptKindRecord["id"].(string)
			moves.DuplicateKindRecordID, moves.KeptKindRecordID = duplicateKindID, keptKindID
			if err := saveRevision(transaction, kind.Table, "id", duplicateKindID, actor, now); err != nil {
				return "", err
			}
			switch duplicateKind {
			case "computer":
				parts, err := stringColumn(transaction, `SELECT id FROM computer_parts WHERE installed_in_computer_id = ? ORDER BY seq`, duplicateKindID)
				if err != nil {
					return "", err
				}
				if _, err := transaction.Exec(`UPDATE computer_parts SET installed_in_computer_id = ? WHERE installed_in_computer_id = ?`, keptKindID, duplicateKindID); err != nil {
					return "", err
				}
				moves.InstalledPartsMoved = parts
				machine := duplicate["computer"].(map[string]any)["harness_machine_id"]
				if machine != nil && keptKindRecord["harness_machine_id"] == nil {
					if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = NULL WHERE id = ?`, duplicateKindID); err != nil {
						return "", err
					}
					if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = ? WHERE id = ?`, machine, keptKindID); err != nil {
						return "", err
					}
					moves.HarnessMachineMoved = machine.(string)
				}
			case "vehicle":
				readings, err := int64Column(transaction, `SELECT read_at FROM vehicle_odometer_readings WHERE vehicle_id = ? AND read_at NOT IN (SELECT read_at FROM vehicle_odometer_readings WHERE vehicle_id = ?) ORDER BY read_at`, duplicateKindID, keptKindID)
				if err != nil {
					return "", err
				}
				if _, err := transaction.Exec(`INSERT INTO vehicle_odometer_readings (vehicle_id, read_at, miles, source) SELECT ?::text, read_at, miles, source FROM vehicle_odometer_readings WHERE vehicle_id = ? ON CONFLICT DO NOTHING`, keptKindID, duplicateKindID); err != nil {
					return "", err
				}
				moves.OdometerReadingsInserted = readings
			}
		}
	}

	// Outfits.
	rows, err := transaction.Query(`SELECT outfit_id, position, NOT EXISTS (SELECT 1 FROM outfit_possessions AS kept WHERE kept.outfit_id = outfit_possessions.outfit_id AND kept.possession_id = ?)
		FROM outfit_possessions WHERE possession_id = ? ORDER BY outfit_id`, keptID, duplicateID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var membership outfitMembership
		if err := rows.Scan(&membership.OutfitID, &membership.Position, &membership.AddedToKept); err != nil {
			rows.Close()
			return "", err
		}
		moves.OutfitMemberships = append(moves.OutfitMemberships, membership)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position) SELECT outfit_id, ?::text, position FROM outfit_possessions WHERE possession_id = ? ON CONFLICT DO NOTHING`, keptID, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE possession_id = ?`, duplicateID); err != nil {
		return "", err
	}

	// Photos and sightings.
	if moves.PhotosInserted, err = stringColumn(transaction, `SELECT photo_id FROM possession_photos WHERE possession_id = ? AND photo_id NOT IN (SELECT photo_id FROM possession_photos WHERE possession_id = ?) ORDER BY photo_id`, duplicateID, keptID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`INSERT INTO possession_photos (possession_id, photo_id, box_left, box_top, box_right, box_bottom, is_cover, sighting_id, linked_by, linked_at)
		SELECT ?::text, photo_id, box_left, box_top, box_right, box_bottom, 0, sighting_id, ?::text, ?::bigint FROM possession_photos WHERE possession_id = ? ON CONFLICT DO NOTHING`, keptID, actor, now, duplicateID); err != nil {
		return "", err
	}
	if kept["cover_photo_id"] == nil {
		if moves.CoverSetPhotoID, err = chooseCover(transaction, keptID); err != nil {
			return "", err
		}
	}
	if moves.SightingsRepointed, err = stringColumn(transaction, `SELECT id FROM sightings WHERE possession_id = ? ORDER BY seq`, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE sightings SET possession_id = ? WHERE possession_id = ?`, keptID, duplicateID); err != nil {
		return "", err
	}
	if moves.SlotsRepointed, err = stringColumn(transaction, `SELECT id FROM outfit_slots WHERE possession_id = ? ORDER BY seq`, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE outfit_slots SET possession_id = ?, updated_at = ? WHERE possession_id = ?`, keptID, now, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE possessions SET merged_into_id = ?, deleted_at = ?, updated_at = ? WHERE id = ?`, keptID, now, now, duplicateID); err != nil {
		return "", err
	}
	mergeID, sequence, err := nextID(transaction, "possession_merges", "possession_merge")
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(moves)
	if err != nil {
		return "", err
	}
	order, err := nextMergeOrder(transaction)
	if err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`INSERT INTO possession_merges (id, seq, duplicate_possession_id, kept_possession_id, merged_by, reason, moved, merged_at, merge_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mergeID, sequence, duplicateID, keptID, actor, reason, string(encoded), now, order); err != nil {
		return "", err
	}
	return mergeID, nil
}

// chooseCover makes the earliest linked photo the cover when a possession
// has none, and answers its id ("" when it already had one or has no photo).
func chooseCover(transaction *databaseTransaction, possessionID string) (string, error) {
	var hasCover int
	if err := transaction.QueryRow(`SELECT COUNT(*) FROM possession_photos WHERE possession_id = ? AND is_cover = 1`, possessionID).Scan(&hasCover); err != nil {
		return "", err
	}
	if hasCover > 0 {
		return "", nil
	}
	var photoID string
	err := transaction.QueryRow(`SELECT photo_id FROM possession_photos WHERE possession_id = ? ORDER BY linked_at, photo_id LIMIT 1`, possessionID).Scan(&photoID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = transaction.Exec(`UPDATE possession_photos SET is_cover = 1 WHERE possession_id = ? AND photo_id = ?`, possessionID, photoID)
	return photoID, err
}

func int64Column(q querier, query string, arguments ...any) ([]int64, error) {
	rows, err := q.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []int64{}
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func possessionMergeByID(q querier, id string) (PossessionMerge, error) {
	var merge PossessionMerge
	var moved string
	err := q.QueryRow(`SELECT id, duplicate_possession_id, kept_possession_id, merged_by, reason, moved, merged_at, undone_by, undone_at FROM possession_merges WHERE id = ?`, id).Scan(
		&merge.ID, &merge.DuplicatePossessionID, &merge.KeptPossessionID, &merge.MergedBy, &merge.Reason, &moved, &merge.MergedAt, &merge.UndoneBy, &merge.UndoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PossessionMerge{}, fmt.Errorf("%w: no possession merge %s", ErrNotFound, id)
	}
	if err != nil {
		return PossessionMerge{}, err
	}
	if err := json.Unmarshal([]byte(moved), &merge.Moved); err != nil {
		return PossessionMerge{}, fmt.Errorf("%s holds moves that are not JSON: %w", id, err)
	}
	return merge, nil
}

// GetPossessionMerge answers one merge.
func (s *Store) GetPossessionMerge(id string) (PossessionMerge, error) {
	return possessionMergeByID(s.db, id)
}

// ListPossessionMerges answers merges, newest first; possessionID narrows
// them to those where it was the duplicate or the kept one.
func (s *Store) ListPossessionMerges(possessionID string, limit, offset int) ([]PossessionMerge, error) {
	if limit <= 0 {
		limit = 100
	}
	ids, err := stringColumn(s.db, `SELECT id FROM possession_merges WHERE ? = '' OR duplicate_possession_id = ? OR kept_possession_id = ? ORDER BY seq DESC LIMIT ? OFFSET ?`,
		possessionID, possessionID, possessionID, limit, offset)
	if err != nil {
		return nil, err
	}
	merges := make([]PossessionMerge, 0, len(ids))
	for _, id := range ids {
		merge, err := possessionMergeByID(s.db, id)
		if err != nil {
			return nil, err
		}
		merges = append(merges, merge)
	}
	return merges, nil
}

// UndoPossessionMerge reverses a merge: the duplicate comes back, and every
// row the merge inserted, re-pointed or filled is put back as it was. A
// later merge, not undone, that touched either possession or an outfit the
// merge changed is a conflict: undo that one first.
func (s *Store) UndoPossessionMerge(ctx context.Context, mergeID, actor string) (PossessionMerge, error) {
	if actor == "" {
		return PossessionMerge{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PossessionMerge{}, err
	}
	defer transaction.Rollback()
	if err := s.undoPossessionMergeInTransaction(transaction, mergeID, actor, s.unixNow()); err != nil {
		return PossessionMerge{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PossessionMerge{}, err
	}
	return possessionMergeByID(s.db, mergeID)
}

func (s *Store) undoPossessionMergeInTransaction(transaction *databaseTransaction, mergeID, actor string, now int64) error {
	merge, err := possessionMergeByID(transaction, mergeID)
	if err != nil {
		return err
	}
	if merge.UndoneAt != 0 {
		return fmt.Errorf("%w: %s was undone already", ErrConflict, mergeID)
	}
	duplicateID, keptID, moves := merge.DuplicatePossessionID, merge.KeptPossessionID, merge.Moved
	order, err := mergeOrderOf(transaction, "possession_merges", mergeID)
	if err != nil {
		return err
	}
	later, err := stringColumn(transaction, `SELECT id FROM possession_merges WHERE merge_order > ? AND undone_at = 0
		AND (duplicate_possession_id IN (?, ?) OR kept_possession_id IN (?, ?)) ORDER BY merge_order DESC`, order, duplicateID, keptID, duplicateID, keptID)
	if err != nil {
		return err
	}
	outfitIDs := []string{}
	for _, membership := range moves.OutfitMemberships {
		outfitIDs = append(outfitIDs, membership.OutfitID)
	}
	laterOutfitMerges, err := outfitMergesTouchingAfter(transaction, order, outfitIDs)
	if err != nil {
		return err
	}
	later = append(later, laterOutfitMerges...)
	if len(later) > 0 {
		return fmt.Errorf("%w: undo %s first; it changed the same records later", ErrConflict, strings.Join(later, ", "))
	}
	for _, id := range []string{duplicateID, keptID} {
		if err := saveRevision(transaction, "possessions", "id", id, actor, now); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`UPDATE possessions SET merged_into_id = NULL, deleted_at = 0, updated_at = ? WHERE id = ?`, now, duplicateID); err != nil {
		return err
	}
	for name, previous := range moves.FilledFields {
		if _, known := possessionType.column(name); !known {
			return fmt.Errorf("%s filled %q, which is not a possession column", mergeID, name)
		}
		if _, err := transaction.Exec(`UPDATE possessions SET `+name+` = ?, updated_at = ? WHERE id = ?`, previous, now, keptID); err != nil {
			return err
		}
	}
	if moved := moves.KindRecordMoved; moved != nil {
		known := false
		for _, kind := range KindTypes {
			known = known || kind.Table == moved.Table
		}
		if !known {
			return fmt.Errorf("%s moved a record of %q, which is not a kind table", mergeID, moved.Table)
		}
		if _, err := transaction.Exec(`UPDATE `+moved.Table+` SET possession_id = ? WHERE id = ?`, duplicateID, moved.ID); err != nil {
			return err
		}
	}
	for _, partID := range moves.InstalledPartsMoved {
		if _, err := transaction.Exec(`UPDATE computer_parts SET installed_in_computer_id = ? WHERE id = ? AND installed_in_computer_id = ?`, moves.DuplicateKindRecordID, partID, moves.KeptKindRecordID); err != nil {
			return err
		}
	}
	if moves.HarnessMachineMoved != "" {
		if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = NULL WHERE id = ? AND harness_machine_id = ?`, moves.KeptKindRecordID, moves.HarnessMachineMoved); err != nil {
			return err
		}
		if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = ? WHERE id = ?`, moves.HarnessMachineMoved, moves.DuplicateKindRecordID); err != nil {
			return err
		}
	}
	for _, readAt := range moves.OdometerReadingsInserted {
		if _, err := transaction.Exec(`DELETE FROM vehicle_odometer_readings WHERE vehicle_id = ? AND read_at = ?`, moves.KeptKindRecordID, readAt); err != nil {
			return err
		}
	}
	for _, membership := range moves.OutfitMemberships {
		if _, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, membership.OutfitID, duplicateID, membership.Position); err != nil {
			return err
		}
		if membership.AddedToKept {
			if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ? AND possession_id = ?`, membership.OutfitID, keptID); err != nil {
				return err
			}
		}
	}
	for _, photoID := range moves.PhotosInserted {
		if _, err := transaction.Exec(`DELETE FROM possession_photos WHERE possession_id = ? AND photo_id = ?`, keptID, photoID); err != nil {
			return err
		}
	}
	if _, err := chooseCover(transaction, keptID); err != nil {
		return err
	}
	for _, sightingID := range moves.SightingsRepointed {
		if _, err := transaction.Exec(`UPDATE sightings SET possession_id = ? WHERE id = ? AND possession_id = ?`, duplicateID, sightingID, keptID); err != nil {
			return err
		}
	}
	for _, slotID := range moves.SlotsRepointed {
		if _, err := transaction.Exec(`UPDATE outfit_slots SET possession_id = ?, updated_at = ? WHERE id = ? AND possession_id = ?`, duplicateID, now, slotID, keptID); err != nil {
			return err
		}
	}
	_, err = transaction.Exec(`UPDATE possession_merges SET undone_by = ?, undone_at = ? WHERE id = ?`, actor, now, mergeID)
	return err
}

// nextMergeOrder is one more than the latest merge of either kind, so undo
// can tell which merge came after which.
func nextMergeOrder(transaction *databaseTransaction) (int64, error) {
	var latest int64
	err := transaction.QueryRow(`SELECT GREATEST(COALESCE((SELECT MAX(merge_order) FROM possession_merges), 0), COALESCE((SELECT MAX(merge_order) FROM outfit_merges), 0))`).Scan(&latest)
	return latest + 1, err
}

func mergeOrderOf(q querier, table, id string) (int64, error) {
	var order int64
	err := q.QueryRow(`SELECT merge_order FROM `+table+` WHERE id = ?`, id).Scan(&order)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no merge %s", ErrNotFound, id)
	}
	return order, err
}

// outfitMergesTouchingAfter answers outfit merges, not undone, made after
// the merge of this order, that combined any of these outfits.
func outfitMergesTouchingAfter(q querier, order int64, outfitIDs []string) ([]string, error) {
	found := []string{}
	for _, outfitID := range outfitIDs {
		ids, err := stringColumn(q, `SELECT id FROM outfit_merges WHERE undone_at = 0 AND merge_order > ? AND (duplicate_outfit_id = ? OR kept_outfit_id = ?)`, order, outfitID, outfitID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !contains(found, id) {
				found = append(found, id)
			}
		}
	}
	return found, nil
}
