package possessionstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// MergePossession folds a duplicate into the possession it duplicates. The
// kept possession takes the duplicate's photos, sightings, kind (when it has
// none of its own), and any field it leaves blank; outfits and installed
// parts that pointed at the duplicate's kind record point at the kept one.
// The duplicate is deleted with merged_into_id set, so its id still answers
// with include_deleted and says where it went. Two possessions of different
// kinds are a conflict: change one's kind first.
func (s *Store) MergePossession(ctx context.Context, duplicateID, keptID, actor string) (map[string]any, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	if duplicateID == keptID {
		return nil, fmt.Errorf("%w: a possession cannot be merged into itself", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	duplicate, err := possessionView(transaction, duplicateID, false)
	if err != nil {
		return nil, err
	}
	kept, err := possessionView(transaction, keptID, false)
	if err != nil {
		return nil, err
	}
	duplicateKind, keptKind := duplicate["kind"].(string), kept["kind"].(string)
	if duplicateKind != "" && keptKind != "" && duplicateKind != keptKind {
		return nil, fmt.Errorf("%w: %s is a %s and %s a %s; give them one kind before merging", ErrConflict, duplicateID, duplicateKind, keptID, keptKind)
	}
	now := s.unixNow()
	for _, id := range []string{duplicateID, keptID} {
		if err := saveRevision(transaction, "possessions", "id", id, actor, now); err != nil {
			return nil, err
		}
	}

	// Blank fields of the kept possession take the duplicate's values.
	fill := map[string]json.RawMessage{}
	for _, name := range []string{"brand", "model", "serial_number", "description", "condition", "spot", "acquired_how", "price_currency"} {
		if kept[name] == "" && duplicate[name] != "" && duplicate[name] != nil {
			encoded, _ := json.Marshal(duplicate[name])
			fill[name] = encoded
		}
	}
	for _, name := range []string{"place_id", "purchase_line_id", "price_cents"} {
		if kept[name] == nil && duplicate[name] != nil {
			encoded, _ := json.Marshal(duplicate[name])
			fill[name] = encoded
		}
	}
	if kept["acquired_at"] == int64(0) && duplicate["acquired_at"] != int64(0) {
		encoded, _ := json.Marshal(duplicate["acquired_at"])
		fill["acquired_at"] = encoded
	}
	if len(fill) > 0 {
		values, err := possessionType.decodeFields(fill, false)
		if err != nil {
			return nil, err
		}
		if err := updateRecord(transaction, possessionType, "id", keptID, map[string]any{"updated_at": now}, values); err != nil {
			return nil, err
		}
	}

	// The kind.
	if duplicateKind != "" {
		kind, _ := KindByName(duplicateKind)
		duplicateKindID := duplicate[duplicateKind].(map[string]any)["id"].(string)
		if keptKind == "" {
			if _, err := transaction.Exec(`UPDATE `+kind.Table+` SET possession_id = ? WHERE id = ?`, keptID, duplicateKindID); err != nil {
				return nil, err
			}
		} else {
			keptKindRecord := kept[keptKind].(map[string]any)
			keptKindID := keptKindRecord["id"].(string)
			if err := saveRevision(transaction, kind.Table, "id", duplicateKindID, actor, now); err != nil {
				return nil, err
			}
			switch duplicateKind {
			case "computer":
				if _, err := transaction.Exec(`UPDATE computer_parts SET installed_in_computer_id = ? WHERE installed_in_computer_id = ?`, keptKindID, duplicateKindID); err != nil {
					return nil, err
				}
				machine := duplicate["computer"].(map[string]any)["harness_machine_id"]
				if machine != nil && keptKindRecord["harness_machine_id"] == nil {
					if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = NULL WHERE id = ?`, duplicateKindID); err != nil {
						return nil, err
					}
					if _, err := transaction.Exec(`UPDATE computers SET harness_machine_id = ? WHERE id = ?`, machine, keptKindID); err != nil {
						return nil, err
					}
				}
			case "vehicle":
				if _, err := transaction.Exec(`INSERT OR IGNORE INTO vehicle_odometer_readings (vehicle_id, read_at, miles, source) SELECT ?, read_at, miles, source FROM vehicle_odometer_readings WHERE vehicle_id = ?`, keptKindID, duplicateKindID); err != nil {
					return nil, err
				}
			}
		}
	}

	// Outfits.
	if _, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_possessions (outfit_id, possession_id, position) SELECT outfit_id, ?, position FROM outfit_possessions WHERE possession_id = ?`, keptID, duplicateID); err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE possession_id = ?`, duplicateID); err != nil {
		return nil, err
	}

	// Photos and sightings.
	if _, err := transaction.Exec(`INSERT OR IGNORE INTO possession_photos (possession_id, photo_id, box_left, box_top, box_right, box_bottom, is_cover, sighting_id, linked_by, linked_at)
		SELECT ?, photo_id, box_left, box_top, box_right, box_bottom, 0, sighting_id, ?, ? FROM possession_photos WHERE possession_id = ?`, keptID, actor, now, duplicateID); err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(`UPDATE possession_photos SET is_cover = 1 WHERE possession_id = ?1 AND NOT EXISTS (SELECT 1 FROM possession_photos WHERE possession_id = ?1 AND is_cover = 1)
		AND photo_id = (SELECT photo_id FROM possession_photos WHERE possession_id = ?1 ORDER BY linked_at, photo_id LIMIT 1)`, keptID); err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(`UPDATE sightings SET possession_id = ? WHERE possession_id = ?`, keptID, duplicateID); err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(`UPDATE possessions SET merged_into_id = ?, deleted_at = ?, updated_at = ? WHERE id = ?`, keptID, now, now, duplicateID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(keptID, false)
}
