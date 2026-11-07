package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Rebuilding outfits from looks: one outfit per distinct set of pieces the
// looks are linked to, worn on each look's day. A set already made by an
// earlier rebuild is reused. The outfits the looks' readings pointed at are
// replaced: deleted, with merged_into_id naming the outfit that took most of
// their readings, once no reading points at them. The whole rebuild is one
// transaction and one outfit_rebuild record, which undo reverses.

// outfitRebuildMoves is what a rebuild changed.
type outfitRebuildMoves struct {
	OutfitsMade     []string        `json:"outfits_made"`
	OutfitsReplaced []string        `json:"outfits_replaced"`
	WearingsAdded   []outfitWearing `json:"wearings_added"` // on outfits an earlier rebuild made
	Looks           []pointerMove   `json:"looks"`
	Reviews         []pointerMove   `json:"reviews"`
}

type outfitWearing struct {
	OutfitID string `json:"outfit_id"`
	WornOn   string `json:"worn_on"`
}

// pointerMove is one row whose outfit_id the rebuild changed.
type pointerMove struct {
	ID          string  `json:"id"`
	OldOutfitID *string `json:"old_outfit_id"`
	NewOutfitID string  `json:"new_outfit_id"`
}

// OutfitRebuild is one rebuild as the API answers it.
type OutfitRebuild struct {
	ID        string             `json:"id"`
	RebuiltBy string             `json:"rebuilt_by"`
	Moved     outfitRebuildMoves `json:"moved"`
	RebuiltAt int64              `json:"rebuilt_at"`
	UndoneBy  string             `json:"undone_by"`
	UndoneAt  int64              `json:"undone_at"`
}

func outfitRebuildByID(q querier, id string) (OutfitRebuild, error) {
	var rebuild OutfitRebuild
	var moved string
	err := q.QueryRow(`SELECT id, rebuilt_by, moved, rebuilt_at, undone_by, undone_at FROM outfit_rebuilds WHERE id = ?`, id).Scan(
		&rebuild.ID, &rebuild.RebuiltBy, &moved, &rebuild.RebuiltAt, &rebuild.UndoneBy, &rebuild.UndoneAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OutfitRebuild{}, fmt.Errorf("%w: no outfit rebuild %s", ErrNotFound, id)
	}
	if err != nil {
		return OutfitRebuild{}, err
	}
	if err := json.Unmarshal([]byte(moved), &rebuild.Moved); err != nil {
		return OutfitRebuild{}, fmt.Errorf("%s holds moves that are not JSON: %w", id, err)
	}
	return rebuild, nil
}

// GetOutfitRebuild answers one rebuild.
func (s *Store) GetOutfitRebuild(id string) (OutfitRebuild, error) {
	return outfitRebuildByID(s.db, id)
}

// RebuildOutfitsFromLooks gives every look without an outfit the outfit of
// its pieces.
func (s *Store) RebuildOutfitsFromLooks(ctx context.Context, actor string) (AuditRun, error) {
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run, err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	rebuildID, sequence, err := nextID(transaction, "outfit_rebuilds", "outfit_rebuild")
	if err != nil {
		return run, err
	}
	lookIDs, err := stringColumn(transaction, `SELECT id FROM outfit_looks WHERE outfit_id IS NULL ORDER BY worn_on, seq`)
	if err != nil {
		return run, err
	}
	moves := outfitRebuildMoves{OutfitsMade: []string{}, OutfitsReplaced: []string{}, WearingsAdded: []outfitWearing{}, Looks: []pointerMove{}, Reviews: []pointerMove{}}
	outfitOfSet := map[string]string{}
	takenFrom := map[string]map[string]int{} // old outfit → new outfit → readings moved
	for _, lookID := range lookIDs {
		run.Looked++
		pieces, err := lookPieces(transaction, lookID)
		if err != nil {
			return run, err
		}
		if len(pieces) == 0 {
			run.leave(lookID, "none of its slots is linked to a piece yet")
			continue
		}
		key := strings.Join(pieces, ",")
		target, found := outfitOfSet[key]
		if !found {
			if target, err = rebuiltOutfitWithExactly(transaction, pieces); err != nil {
				return run, err
			}
			if target == "" {
				var outfitSequence int64
				if target, outfitSequence, err = nextID(transaction, "outfits", "outfit"); err != nil {
					return run, err
				}
				if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at) VALUES (?, ?, '', '', '', 'kept', 'Rebuilt from the looks read from selfies', '', ?, ?, ?)`,
					target, outfitSequence, rebuildID, now, now); err != nil {
					return run, err
				}
				if err := replaceOutfitPossessions(transaction, target, pieces); err != nil {
					return run, err
				}
				moves.OutfitsMade = append(moves.OutfitsMade, target)
				run.Counts["outfits_made"]++
			} else {
				run.Counts["outfits_reused"]++
			}
			outfitOfSet[key] = target
		}
		look, err := lookByID(transaction, lookID)
		if err != nil {
			return run, err
		}
		result, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, target, look.WornOn)
		if err != nil {
			return run, err
		}
		if added, _ := result.RowsAffected(); added > 0 && !contains(moves.OutfitsMade, target) {
			moves.WearingsAdded = append(moves.WearingsAdded, outfitWearing{OutfitID: target, WornOn: look.WornOn})
		}
		if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = ?, updated_at = ? WHERE id = ?`, target, now, lookID); err != nil {
			return run, err
		}
		moves.Looks = append(moves.Looks, pointerMove{ID: lookID, NewOutfitID: target})
		reviewRows, err := transaction.Query(`SELECT photo_reviews.id, photo_reviews.outfit_id FROM photo_reviews JOIN outfit_look_photos ON outfit_look_photos.photo_review_id = photo_reviews.id
			WHERE outfit_look_photos.look_id = ? ORDER BY photo_reviews.seq`, lookID)
		if err != nil {
			return run, err
		}
		reviewMoves := []pointerMove{}
		for reviewRows.Next() {
			var move pointerMove
			var old sql.NullString
			if err := reviewRows.Scan(&move.ID, &old); err != nil {
				reviewRows.Close()
				return run, err
			}
			if old.Valid {
				move.OldOutfitID = &old.String
			}
			move.NewOutfitID = target
			reviewMoves = append(reviewMoves, move)
		}
		reviewRows.Close()
		if err := reviewRows.Err(); err != nil {
			return run, err
		}
		for _, move := range reviewMoves {
			if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ?`, target, move.ID); err != nil {
				return run, err
			}
			moves.Reviews = append(moves.Reviews, move)
			if move.OldOutfitID != nil && *move.OldOutfitID != target {
				if takenFrom[*move.OldOutfitID] == nil {
					takenFrom[*move.OldOutfitID] = map[string]int{}
				}
				takenFrom[*move.OldOutfitID][target]++
			}
		}
		run.Actions = append(run.Actions, AuditAction{Action: "placed", Records: []string{lookID, target}})
	}
	oldOutfits := []string{}
	for outfitID := range takenFrom {
		oldOutfits = append(oldOutfits, outfitID)
	}
	sort.Strings(oldOutfits)
	for _, outfitID := range oldOutfits {
		var remaining int
		if err := transaction.QueryRow(`SELECT COUNT(*) FROM photo_reviews WHERE outfit_id = ?`, outfitID).Scan(&remaining); err != nil {
			return run, err
		}
		var deletedAt int64
		if err := transaction.QueryRow(`SELECT deleted_at FROM outfits WHERE id = ?`, outfitID).Scan(&deletedAt); err != nil {
			return run, err
		}
		if remaining > 0 || deletedAt != 0 || contains(moves.OutfitsMade, outfitID) || outfitOfSetHolds(outfitOfSet, outfitID) {
			continue
		}
		most, mostCount := "", 0
		for target, count := range takenFrom[outfitID] {
			if count > mostCount || (count == mostCount && target < most) {
				most, mostCount = target, count
			}
		}
		if err := saveRevision(transaction, "outfits", "id", outfitID, actor, now); err != nil {
			return run, err
		}
		if _, err := transaction.Exec(`UPDATE outfits SET deleted_at = ?, merged_into_id = ?, updated_at = ? WHERE id = ?`, now, most, now, outfitID); err != nil {
			return run, err
		}
		moves.OutfitsReplaced = append(moves.OutfitsReplaced, outfitID)
		run.Counts["outfits_replaced"]++
	}
	encoded, err := json.Marshal(moves)
	if err != nil {
		return run, err
	}
	if _, err := transaction.Exec(`INSERT INTO outfit_rebuilds (id, seq, rebuilt_by, moved, rebuilt_at) VALUES (?, ?, ?, ?, ?)`, rebuildID, sequence, actor, string(encoded), now); err != nil {
		return run, err
	}
	if err := transaction.Commit(); err != nil {
		return run, err
	}
	run.Actions = append(run.Actions, AuditAction{Action: "rebuilt", Records: []string{rebuildID}})
	return run, nil
}

func outfitOfSetHolds(outfitOfSet map[string]string, outfitID string) bool {
	for _, target := range outfitOfSet {
		if target == outfitID {
			return true
		}
	}
	return false
}

// rebuiltOutfitWithExactly finds a live outfit an earlier rebuild made that
// holds exactly these pieces.
func rebuiltOutfitWithExactly(q querier, pieces []string) (string, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(pieces)), ", ")
	arguments := []any{}
	for _, piece := range pieces {
		arguments = append(arguments, piece)
	}
	arguments = append(arguments, len(pieces), len(pieces))
	var outfitID string
	err := q.QueryRow(`SELECT outfits.id FROM outfits JOIN outfit_possessions ON outfit_possessions.outfit_id = outfits.id
		WHERE outfits.deleted_at = 0 AND outfits.created_by LIKE 'outfit_rebuild_%'
		GROUP BY outfits.id
		HAVING SUM(outfit_possessions.possession_id IN (`+placeholders+`)) = ? AND COUNT(*) = ?
		ORDER BY outfits.seq LIMIT 1`, arguments...).Scan(&outfitID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return outfitID, err
}

// UndoOutfitRebuild puts back what a rebuild changed. A later rebuild not
// undone, a look the rebuild placed that has moved since (a slot answer),
// or a merge of an outfit it made is a conflict.
func (s *Store) UndoOutfitRebuild(ctx context.Context, rebuildID, actor string) (OutfitRebuild, error) {
	if actor == "" {
		return OutfitRebuild{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OutfitRebuild{}, err
	}
	defer transaction.Rollback()
	rebuild, err := outfitRebuildByID(transaction, rebuildID)
	if err != nil {
		return OutfitRebuild{}, err
	}
	if rebuild.UndoneAt != 0 {
		return OutfitRebuild{}, fmt.Errorf("%w: %s was undone already", ErrConflict, rebuildID)
	}
	later, err := stringColumn(transaction, `SELECT id FROM outfit_rebuilds WHERE seq > (SELECT seq FROM outfit_rebuilds WHERE id = ?) AND undone_at = 0`, rebuildID)
	if err != nil {
		return OutfitRebuild{}, err
	}
	if len(later) > 0 {
		return OutfitRebuild{}, fmt.Errorf("%w: undo %s first", ErrConflict, strings.Join(later, ", "))
	}
	for _, look := range rebuild.Moved.Looks {
		var current sql.NullString
		if err := transaction.QueryRow(`SELECT outfit_id FROM outfit_looks WHERE id = ?`, look.ID).Scan(&current); err != nil {
			return OutfitRebuild{}, err
		}
		if !current.Valid || current.String != look.NewOutfitID {
			return OutfitRebuild{}, fmt.Errorf("%w: %s has moved to %s since the rebuild; a slot answer changed it", ErrConflict, look.ID, current.String)
		}
	}
	for _, outfitID := range rebuild.Moved.OutfitsMade {
		merges, err := stringColumn(transaction, `SELECT id FROM outfit_merges WHERE undone_at = 0 AND (duplicate_outfit_id = ? OR kept_outfit_id = ?)`, outfitID, outfitID)
		if err != nil {
			return OutfitRebuild{}, err
		}
		if len(merges) > 0 {
			return OutfitRebuild{}, fmt.Errorf("%w: undo %s first; it combined %s", ErrConflict, strings.Join(merges, ", "), outfitID)
		}
	}
	now := s.unixNow()
	for _, move := range rebuild.Moved.Reviews {
		if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ?`, move.OldOutfitID, move.ID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, move := range rebuild.Moved.Looks {
		if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = NULL, updated_at = ? WHERE id = ?`, now, move.ID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, wearing := range rebuild.Moved.WearingsAdded {
		if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ? AND worn_on = ?`, wearing.OutfitID, wearing.WornOn); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, outfitID := range rebuild.Moved.OutfitsMade {
		if err := saveRevision(transaction, "outfits", "id", outfitID, actor, now); err != nil {
			return OutfitRebuild{}, err
		}
		if _, err := transaction.Exec(`UPDATE outfits SET deleted_at = ?, updated_at = ? WHERE id = ?`, now, now, outfitID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, outfitID := range rebuild.Moved.OutfitsReplaced {
		if err := saveRevision(transaction, "outfits", "id", outfitID, actor, now); err != nil {
			return OutfitRebuild{}, err
		}
		if _, err := transaction.Exec(`UPDATE outfits SET deleted_at = 0, merged_into_id = NULL, updated_at = ? WHERE id = ?`, now, outfitID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	if _, err := transaction.Exec(`UPDATE outfit_rebuilds SET undone_by = ?, undone_at = ? WHERE id = ?`, actor, now, rebuildID); err != nil {
		return OutfitRebuild{}, err
	}
	if err := transaction.Commit(); err != nil {
		return OutfitRebuild{}, err
	}
	return outfitRebuildByID(s.db, rebuildID)
}
