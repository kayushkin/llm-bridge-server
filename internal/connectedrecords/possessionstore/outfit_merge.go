package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// outfitMergeMoves is what combining two outfits changed, so undo can put
// back exactly that. The duplicate outfit keeps its own pieces and wearings
// rows; it is only deleted.
type outfitMergeMoves struct {
	PiecesAdded      []outfitMembership `json:"pieces_added"`      // the duplicate's pieces the kept outfit did not hold
	WearingsAdded    []string           `json:"wearings_added"`    // the duplicate's days the kept outfit had not been worn on
	ReviewsRepointed []string           `json:"reviews_repointed"` // outfit readings that now answer the kept outfit
	// The owner's marks the kept outfit took from the duplicate: favorite
	// when only the duplicate was one, and the duplicate's tags it lacked.
	FavoriteSet bool     `json:"favorite_set"`
	TagsAdded   []string `json:"tags_added"`
}

// OutfitMerge is one combining of two outfits as the API answers it.
type OutfitMerge struct {
	ID                string           `json:"id"`
	DuplicateOutfitID string           `json:"duplicate_outfit_id"`
	KeptOutfitID      string           `json:"kept_outfit_id"`
	MergedBy          string           `json:"merged_by"`
	Reason            string           `json:"reason"`
	Moved             outfitMergeMoves `json:"moved"`
	MergedAt          int64            `json:"merged_at"`
	UndoneBy          string           `json:"undone_by"`
	UndoneAt          int64            `json:"undone_at"`
}

// combineOutfitsInTransaction folds one outfit into another: the kept
// outfit takes the union of pieces, every wearing and every outfit reading;
// the duplicate is deleted with merged_into_id set. It answers the
// outfit_merge id.
func (s *Store) combineOutfitsInTransaction(transaction *databaseTransaction, duplicateID, keptID, actor, reason string, now int64) (string, error) {
	if duplicateID == keptID {
		return "", fmt.Errorf("%w: an outfit cannot be combined with itself", ErrInvalid)
	}
	sides := map[string]Outfit{}
	for _, id := range []string{duplicateID, keptID} {
		outfit, err := outfitByID(transaction, id, false)
		if err != nil {
			return "", err
		}
		sides[id] = outfit
		if err := saveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
			return "", err
		}
	}
	moves := outfitMergeMoves{PiecesAdded: []outfitMembership{}, TagsAdded: []string{}}
	kept, duplicate := sides[keptID], sides[duplicateID]
	moves.FavoriteSet = duplicate.Favorite && !kept.Favorite
	keptTags := append([]string{}, kept.Tags...)
	for _, tag := range duplicate.Tags {
		if !contains(keptTags, tag) {
			keptTags = append(keptTags, tag)
			moves.TagsAdded = append(moves.TagsAdded, tag)
		}
	}
	if _, err := transaction.Exec(`UPDATE outfits SET favorite = ?, tags = ? WHERE id = ?`, kept.Favorite || duplicate.Favorite, mustJSON(keptTags), keptID); err != nil {
		return "", err
	}
	var err error
	rows, err := transaction.Query(`SELECT possession_id FROM outfit_possessions WHERE outfit_id = ? AND possession_id NOT IN (SELECT possession_id FROM outfit_possessions WHERE outfit_id = ?) ORDER BY position`, duplicateID, keptID)
	if err != nil {
		return "", err
	}
	added := []string{}
	for rows.Next() {
		var possessionID string
		if err := rows.Scan(&possessionID); err != nil {
			rows.Close()
			return "", err
		}
		added = append(added, possessionID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	var nextPosition int
	if err := transaction.QueryRow(`SELECT COALESCE(MAX(position), -1) + 1 FROM outfit_possessions WHERE outfit_id = ?`, keptID).Scan(&nextPosition); err != nil {
		return "", err
	}
	for _, possessionID := range added {
		if _, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position) VALUES (?, ?, ?)`, keptID, possessionID, nextPosition); err != nil {
			return "", err
		}
		moves.PiecesAdded = append(moves.PiecesAdded, outfitMembership{OutfitID: keptID, PossessionID: possessionID, Position: nextPosition})
		nextPosition++
	}
	if moves.WearingsAdded, err = stringColumn(transaction, `SELECT worn_on FROM outfit_wearings WHERE outfit_id = ? AND worn_on NOT IN (SELECT worn_on FROM outfit_wearings WHERE outfit_id = ?) ORDER BY worn_on`, duplicateID, keptID); err != nil {
		return "", err
	}
	for _, day := range moves.WearingsAdded {
		if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, keptID, day); err != nil {
			return "", err
		}
	}
	if moves.ReviewsRepointed, err = stringColumn(transaction, `SELECT id FROM photo_reviews WHERE outfit_id = ? ORDER BY seq`, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE outfit_id = ?`, keptID, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE outfits SET merged_into_id = ?, deleted_at = ?, updated_at = ? WHERE id = ?`, keptID, now, now, duplicateID); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE outfits SET updated_at = ? WHERE id = ?`, now, keptID); err != nil {
		return "", err
	}
	mergeID, sequence, err := nextID(transaction, "outfit_merges", "outfit_merge")
	if err != nil {
		return "", err
	}
	order, err := nextMergeOrder(transaction)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(moves)
	if err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`INSERT INTO outfit_merges (id, seq, duplicate_outfit_id, kept_outfit_id, merged_by, reason, moved, merged_at, merge_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mergeID, sequence, duplicateID, keptID, actor, reason, string(encoded), now, order); err != nil {
		return "", err
	}
	return mergeID, nil
}

// CombineOutfits folds the duplicate outfit into the kept one, recorded as
// an outfit_merge that POST /outfit-merges/{id}/undo reverses.
func (s *Store) CombineOutfits(ctx context.Context, duplicateID, keptID, actor, reason string) (OutfitMerge, error) {
	if actor == "" {
		return OutfitMerge{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutfitMerge{}, err
	}
	defer transaction.Rollback()
	mergeID, err := s.combineOutfitsInTransaction(transaction, duplicateID, keptID, actor, reason, s.unixNow())
	if err != nil {
		return OutfitMerge{}, err
	}
	if err := transaction.Commit(); err != nil {
		return OutfitMerge{}, err
	}
	return outfitMergeByID(s.db, mergeID)
}

func outfitMergeByID(q querier, id string) (OutfitMerge, error) {
	var merge OutfitMerge
	var moved string
	err := q.QueryRow(`SELECT id, duplicate_outfit_id, kept_outfit_id, merged_by, reason, moved, merged_at, undone_by, undone_at FROM outfit_merges WHERE id = ?`, id).Scan(
		&merge.ID, &merge.DuplicateOutfitID, &merge.KeptOutfitID, &merge.MergedBy, &merge.Reason, &moved, &merge.MergedAt, &merge.UndoneBy, &merge.UndoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OutfitMerge{}, fmt.Errorf("%w: no outfit merge %s", ErrNotFound, id)
	}
	if err != nil {
		return OutfitMerge{}, err
	}
	if err := json.Unmarshal([]byte(moved), &merge.Moved); err != nil {
		return OutfitMerge{}, fmt.Errorf("%s holds moves that are not JSON: %w", id, err)
	}
	return merge, nil
}

// GetOutfitMerge answers one outfit merge.
func (s *Store) GetOutfitMerge(id string) (OutfitMerge, error) { return outfitMergeByID(s.db, id) }

// ListOutfitMerges answers outfit merges, newest first; outfitID narrows
// them to those where it was either side.
func (s *Store) ListOutfitMerges(outfitID string, limit, offset int) ([]OutfitMerge, error) {
	if limit <= 0 {
		limit = 100
	}
	ids, err := stringColumn(s.db, `SELECT id FROM outfit_merges WHERE ? = '' OR duplicate_outfit_id = ? OR kept_outfit_id = ? ORDER BY seq DESC LIMIT ? OFFSET ?`,
		outfitID, outfitID, outfitID, limit, offset)
	if err != nil {
		return nil, err
	}
	merges := make([]OutfitMerge, 0, len(ids))
	for _, id := range ids {
		merge, err := outfitMergeByID(s.db, id)
		if err != nil {
			return nil, err
		}
		merges = append(merges, merge)
	}
	return merges, nil
}

// UndoOutfitMerge brings the duplicate outfit back and takes from the kept
// one exactly the pieces, wearings and readings the merge gave it. A later
// merge, not undone, of either outfit, or a later possession merge that
// moved a piece of either, is a conflict: undo that one first.
func (s *Store) UndoOutfitMerge(ctx context.Context, mergeID, actor string) (OutfitMerge, error) {
	if actor == "" {
		return OutfitMerge{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutfitMerge{}, err
	}
	defer transaction.Rollback()
	merge, err := outfitMergeByID(transaction, mergeID)
	if err != nil {
		return OutfitMerge{}, err
	}
	if merge.UndoneAt != 0 {
		return OutfitMerge{}, fmt.Errorf("%w: %s was undone already", ErrConflict, mergeID)
	}
	order, err := mergeOrderOf(transaction, "outfit_merges", mergeID)
	if err != nil {
		return OutfitMerge{}, err
	}
	later, err := outfitMergesTouchingAfter(transaction, order, []string{merge.DuplicateOutfitID, merge.KeptOutfitID})
	if err != nil {
		return OutfitMerge{}, err
	}
	possessionMerges, err := stringColumn(transaction, `SELECT id FROM possession_merges WHERE undone_at = 0 AND merge_order > ?
		AND EXISTS (SELECT 1 FROM json_array_items((possession_merges.moved::jsonb -> 'outfit_memberships')::text) AS membership WHERE membership ->> 'outfit_id' IN (?, ?))`,
		order, merge.DuplicateOutfitID, merge.KeptOutfitID)
	if err != nil {
		return OutfitMerge{}, err
	}
	later = append(later, possessionMerges...)
	if len(later) > 0 {
		return OutfitMerge{}, fmt.Errorf("%w: undo %s first; it changed the same outfits later", ErrConflict, strings.Join(later, ", "))
	}
	now := s.unixNow()
	for _, id := range []string{merge.DuplicateOutfitID, merge.KeptOutfitID} {
		if err := saveRevision(transaction, "outfits", "id", id, actor, now); err != nil {
			return OutfitMerge{}, err
		}
	}
	for _, piece := range merge.Moved.PiecesAdded {
		if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ? AND possession_id = ?`, merge.KeptOutfitID, piece.PossessionID); err != nil {
			return OutfitMerge{}, err
		}
	}
	for _, day := range merge.Moved.WearingsAdded {
		if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ? AND worn_on = ?`, merge.KeptOutfitID, day); err != nil {
			return OutfitMerge{}, err
		}
	}
	if merge.Moved.FavoriteSet || len(merge.Moved.TagsAdded) > 0 {
		kept, err := outfitByID(transaction, merge.KeptOutfitID, false)
		if err != nil {
			return OutfitMerge{}, err
		}
		tags := []string{}
		for _, tag := range kept.Tags {
			if !contains(merge.Moved.TagsAdded, tag) {
				tags = append(tags, tag)
			}
		}
		favorite := kept.Favorite && !merge.Moved.FavoriteSet
		if _, err := transaction.Exec(`UPDATE outfits SET favorite = ?, tags = ? WHERE id = ?`, favorite, mustJSON(tags), merge.KeptOutfitID); err != nil {
			return OutfitMerge{}, err
		}
	}
	for _, reviewID := range merge.Moved.ReviewsRepointed {
		if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ? AND outfit_id = ?`, merge.DuplicateOutfitID, reviewID, merge.KeptOutfitID); err != nil {
			return OutfitMerge{}, err
		}
	}
	if _, err := transaction.Exec(`UPDATE outfits SET merged_into_id = NULL, deleted_at = 0, updated_at = ? WHERE id = ?`, now, merge.DuplicateOutfitID); err != nil {
		return OutfitMerge{}, err
	}
	if _, err := transaction.Exec(`UPDATE outfit_merges SET undone_by = ?, undone_at = ? WHERE id = ?`, actor, now, mergeID); err != nil {
		return OutfitMerge{}, err
	}
	if err := transaction.Commit(); err != nil {
		return OutfitMerge{}, err
	}
	return outfitMergeByID(s.db, mergeID)
}
