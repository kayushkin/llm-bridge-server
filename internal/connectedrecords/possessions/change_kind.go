package possessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ChangeKind moves a possession to another kind: its old kind record is
// saved to revisions and removed, and a new one is made from kindFields. An
// empty kind leaves it with none. A computer with parts installed in it is a
// conflict: move the parts first.
func (s *Store) ChangeKind(ctx context.Context, possessionID, kindName string, kindFields map[string]json.RawMessage, actor string) (map[string]any, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	var kind *RecordType
	if kindName != "" {
		found, known := KindByName(kindName)
		if !known {
			return nil, fmt.Errorf("%w: kind %q is not one of %s", ErrInvalid, kindName, strings.Join(KindNames(), ", "))
		}
		kind = &found
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	existing, err := possessionView(transaction, possessionID, false)
	if err != nil {
		return nil, err
	}
	now := s.unixNow()
	if currentName := existing["kind"].(string); currentName != "" {
		if currentName == kindName {
			return nil, fmt.Errorf("%w: %s is already a %s; PATCH changes its fields", ErrConflict, possessionID, kindName)
		}
		current, _ := KindByName(currentName)
		currentID := existing[currentName].(map[string]any)["id"].(string)
		if currentName == "computer" {
			if parts := existing["installed_part_ids"].([]string); len(parts) > 0 {
				return nil, fmt.Errorf("%w: %s has parts installed (%s); move them first", ErrConflict, possessionID, strings.Join(parts, ", "))
			}
		}
		if err := saveRevision(transaction, current.Table, "id", currentID, actor, now); err != nil {
			return nil, err
		}
		if _, err := transaction.Exec(`DELETE FROM `+current.Table+` WHERE id = ?`, currentID); err != nil {
			return nil, err
		}
	}
	if kind != nil {
		if err := s.insertKind(ctx, transaction, *kind, possessionID, kindFields); err != nil {
			return nil, err
		}
	}
	if !contains(WearableKinds, kindName) {
		inOutfits, err := stringColumn(transaction, `SELECT outfit_id FROM outfit_possessions WHERE possession_id = ?`, possessionID)
		if err != nil {
			return nil, err
		}
		if len(inOutfits) > 0 {
			return nil, fmt.Errorf("%w: %s is in outfits (%s), which hold only wearables", ErrConflict, possessionID, strings.Join(inOutfits, ", "))
		}
	}
	if _, err := transaction.Exec(`UPDATE possessions SET updated_at = ? WHERE id = ?`, now, possessionID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return s.GetPossession(possessionID, false)
}
