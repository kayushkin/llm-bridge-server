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

// Rebuilding outfits from looks. An outfit is known by its main pieces:
// its top, bottom, one-piece and outerwear (outfitIdentityGroups).
// Accessories (jewelry, watches, belts, scarves, headwear, bags, eyewear,
// footwear and the rest) change from day to day and photo to photo, so they
// are not part of an outfit's identity. A look joins an outfit when, in each
// main group both show, one's pieces hold the other's (a piece not visible
// in one look does not count against it), and at least two main pieces
// match. The outfit holds the union of the pieces its looks were linked to;
// each look keeps its own slots. A look matching no outfit makes a new one.
// The outfits the looks' readings pointed at are replaced: deleted, with
// merged_into_id naming the outfit that took most of their readings, once
// no reading points at them. The whole rebuild is one transaction and one
// outfit_rebuild record, which undo reverses.

// outfitIdentityGroups are the wearable groups whose pieces say which
// outfit a look is.
var outfitIdentityGroups = []string{"garment:top", "garment:bottom", "garment:one_piece", "garment:outerwear"}

// leastMatchingIdentityPieces is how many main pieces two looks must share
// to be one outfit.
const leastMatchingIdentityPieces = 2

// outfitIdentity is the main pieces of a look or an outfit: group → the
// possession ids, sorted.
type outfitIdentity map[string][]string

func (identity outfitIdentity) add(other outfitIdentity) {
	for group, pieces := range other {
		for _, piece := range pieces {
			if !contains(identity[group], piece) {
				identity[group] = append(identity[group], piece)
			}
		}
		sort.Strings(identity[group])
	}
}

// identityMatch says whether a look with these main pieces is the outfit
// with those, and how many main pieces they share. In every main group
// both show, one's pieces must all be among the other's, and at least two
// main pieces must match; a look that shows only one main piece also
// matches on that one when the outfit was worn the same day (sameDay: a
// look of that day is already in it). Across days one piece is not enough.
func identityMatch(look, outfit outfitIdentity, sameDay bool) (shared int, same bool) {
	for _, group := range outfitIdentityGroups {
		lookPieces, outfitPieces := look[group], outfit[group]
		if len(lookPieces) == 0 || len(outfitPieces) == 0 {
			continue
		}
		inBoth := 0
		for _, piece := range lookPieces {
			if contains(outfitPieces, piece) {
				inBoth++
			}
		}
		if inBoth != min(len(lookPieces), len(outfitPieces)) {
			return 0, false
		}
		shared += inBoth
	}
	if shared >= leastMatchingIdentityPieces {
		return shared, true
	}
	return shared, sameDay && shared == 1 && look.pieceCount() == 1
}

func (identity outfitIdentity) pieceCount() int {
	count := 0
	for _, pieces := range identity {
		count += len(pieces)
	}
	return count
}

// looksIdentity answers the main pieces the slots of these looks are
// linked to.
func looksIdentity(q querier, lookIDs ...string) (outfitIdentity, error) {
	identity := outfitIdentity{}
	for _, lookID := range lookIDs {
		rows, err := q.Query(`SELECT group_key, possession_id FROM outfit_slots WHERE look_id = ? AND possession_id IS NOT NULL ORDER BY seq`, lookID)
		if err != nil {
			return nil, err
		}
		found := outfitIdentity{}
		for rows.Next() {
			var group, possessionID string
			if err := rows.Scan(&group, &possessionID); err != nil {
				rows.Close()
				return nil, err
			}
			if contains(outfitIdentityGroups, group) {
				found[group] = append(found[group], possessionID)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		identity.add(found)
	}
	return identity, nil
}

// outfitLooks answers an outfit as a look may join it: the main pieces,
// count and days of its looks, leaving one look out ("" for none).
func outfitLooks(q querier, outfitID, leaveOut string) (*identityCandidate, error) {
	lookIDs, err := stringColumn(q, `SELECT id FROM outfit_looks WHERE outfit_id = ? AND id != ? ORDER BY seq`, outfitID, leaveOut)
	if err != nil {
		return nil, err
	}
	days, err := stringColumn(q, `SELECT DISTINCT worn_on FROM outfit_looks WHERE outfit_id = ? AND id != ? ORDER BY worn_on`, outfitID, leaveOut)
	if err != nil {
		return nil, err
	}
	identity, err := looksIdentity(q, lookIDs...)
	if err != nil {
		return nil, err
	}
	return &identityCandidate{outfitID: outfitID, identity: identity, looks: len(lookIDs), days: days}, nil
}

// outfitPiecesFromLooks answers the union of the pieces an outfit's looks
// are linked to.
func outfitPiecesFromLooks(q querier, outfitID string) ([]string, error) {
	return stringColumn(q, `SELECT DISTINCT outfit_slots.possession_id FROM outfit_slots JOIN outfit_looks ON outfit_looks.id = outfit_slots.look_id
		WHERE outfit_looks.outfit_id = ? AND outfit_slots.possession_id IS NOT NULL ORDER BY outfit_slots.possession_id`, outfitID)
}

// addOutfitPieces adds the pieces an outfit does not hold yet, after the
// ones it holds, and answers those it added.
func addOutfitPieces(transaction *sql.Tx, outfitID string, pieces []string) ([]string, error) {
	added := []string{}
	for _, piece := range pieces {
		result, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_possessions (outfit_id, possession_id, position)
			VALUES (?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM outfit_possessions WHERE outfit_id = ?))`, outfitID, piece, outfitID)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count > 0 {
			added = append(added, piece)
		}
	}
	return added, nil
}

// upperBodyGroups are the groups a look with its top on shows one of.
var upperBodyGroups = []string{"garment:top", "garment:one_piece", "garment:outerwear"}

// whyNotAnOutfit answers why a look is not an outfit, "" when it is one.
// The owner chose on 2026-11-07: a look with no top on is probably a body
// check photo, never an outfit. The second reader says so
// (wearer_has_no_top), or none of the look's slots is a top, one-piece or
// outerwear (a slot the owner answered "not mine" does not count).
func whyNotAnOutfit(look Look) string {
	if look.WearerHasNoTop != nil && *look.WearerHasNoTop {
		return "the readers saw nothing on the owner's upper body: a body check, not an outfit"
	}
	for _, slot := range look.Slots {
		if contains(upperBodyGroups, slot.Group) && (slot.PossessionID != nil || slot.DecidedBy == "" || slot.DecidedBy == "best_guess") {
			return ""
		}
	}
	return "no top, one-piece or outerwear among its slots: a body check, not an outfit"
}

// lookReviewMoves answers the move of each of a look's readings to an
// outfit ("" for none).
func lookReviewMoves(q querier, lookID, target string) ([]pointerMove, error) {
	rows, err := q.Query(`SELECT photo_reviews.id, photo_reviews.outfit_id FROM photo_reviews JOIN outfit_look_photos ON outfit_look_photos.photo_review_id = photo_reviews.id
		WHERE outfit_look_photos.look_id = ? ORDER BY photo_reviews.seq`, lookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviewMoves := []pointerMove{}
	for rows.Next() {
		var move pointerMove
		var old sql.NullString
		if err := rows.Scan(&move.ID, &old); err != nil {
			return nil, err
		}
		if old.Valid {
			move.OldOutfitID = &old.String
		}
		move.NewOutfitID = target
		reviewMoves = append(reviewMoves, move)
	}
	return reviewMoves, rows.Err()
}

// identityCandidate is an outfit a look may join.
type identityCandidate struct {
	outfitID string
	identity outfitIdentity
	looks    int
	days     []string // the days of its looks
}

// bestIdentityMatch answers the candidate a look with these main pieces,
// worn on this day, is: the one sharing the most main pieces, then the one
// with more looks, then the earlier; nil when none is the same outfit.
func bestIdentityMatch(identity outfitIdentity, day string, candidates []*identityCandidate) *identityCandidate {
	var best *identityCandidate
	bestShared := 0
	for _, candidate := range candidates {
		shared, same := identityMatch(identity, candidate.identity, contains(candidate.days, day))
		if !same {
			continue
		}
		if best == nil || shared > bestShared || (shared == bestShared && candidate.looks > best.looks) {
			best, bestShared = candidate, shared
		}
	}
	return best
}

// outfitRebuildMoves is what a rebuild changed.
type outfitRebuildMoves struct {
	OutfitsMade     []string      `json:"outfits_made"`
	OutfitsReplaced []string      `json:"outfits_replaced"`
	PiecesAdded     []outfitPiece `json:"pieces_added"` // to outfits an earlier rebuild made
	// Looks judged not an outfit (whyNotAnOutfit), and looks no longer so,
	// with the mark each had before.
	LooksMarked     []lookMark      `json:"looks_marked"`
	PiecesRemoved   []outfitPiece   `json:"pieces_removed"`   // from earlier outfits a look left
	WearingsRemoved []outfitWearing `json:"wearings_removed"` // from earlier outfits a look left
	WearingsAdded   []outfitWearing `json:"wearings_added"`   // on outfits an earlier rebuild made
	Looks           []pointerMove   `json:"looks"`
	Reviews         []pointerMove   `json:"reviews"`
}

type lookMark struct {
	LookID string `json:"look_id"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type outfitPiece struct {
	OutfitID     string `json:"outfit_id"`
	PossessionID string `json:"possession_id"`
}

type outfitWearing struct {
	OutfitID string `json:"outfit_id"`
	WornOn   string `json:"worn_on"`
}

// pointerMove is one row whose outfit_id the rebuild changed; a new
// outfit "" means none.
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
	moves := outfitRebuildMoves{OutfitsMade: []string{}, OutfitsReplaced: []string{}, PiecesAdded: []outfitPiece{}, LooksMarked: []lookMark{}, PiecesRemoved: []outfitPiece{},
		WearingsRemoved: []outfitWearing{}, WearingsAdded: []outfitWearing{}, Looks: []pointerMove{}, Reviews: []pointerMove{}}
	takenFrom := map[string]map[string]int{} // old outfit → new outfit → readings moved
	emptied := map[string]bool{}             // outfits that lost readings to looks that are not outfits
	mark := func(look Look, reason string) error {
		if look.NotAnOutfit == reason {
			return nil
		}
		if _, err := transaction.Exec(`UPDATE outfit_looks SET not_an_outfit = ?, updated_at = ? WHERE id = ?`, reason, now, look.ID); err != nil {
			return err
		}
		moves.LooksMarked = append(moves.LooksMarked, lookMark{LookID: look.ID, Before: look.NotAnOutfit, After: reason})
		return nil
	}
	// detachReadings takes a look's readings off their outfits.
	detachReadings := func(lookID string) error {
		reviewMoves, err := lookReviewMoves(transaction, lookID, "")
		if err != nil {
			return err
		}
		for _, move := range reviewMoves {
			if move.OldOutfitID == nil {
				continue
			}
			if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = NULL WHERE id = ?`, move.ID); err != nil {
				return err
			}
			moves.Reviews = append(moves.Reviews, move)
			emptied[*move.OldOutfitID] = true
		}
		return nil
	}
	// A look already in an outfit that is not an outfit leaves it.
	placedLooks, err := stringColumn(transaction, `SELECT id FROM outfit_looks WHERE outfit_id IS NOT NULL ORDER BY worn_on, seq`)
	if err != nil {
		return run, err
	}
	for _, lookID := range placedLooks {
		look, err := lookByID(transaction, lookID)
		if err != nil {
			return run, err
		}
		reason := whyNotAnOutfit(look)
		if reason == "" {
			continue
		}
		run.Looked++
		left := *look.OutfitID
		if err := mark(look, reason); err != nil {
			return run, err
		}
		if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = NULL, updated_at = ? WHERE id = ?`, now, lookID); err != nil {
			return run, err
		}
		moves.Looks = append(moves.Looks, pointerMove{ID: lookID, OldOutfitID: &left, NewOutfitID: ""})
		if err := detachReadings(lookID); err != nil {
			return run, err
		}
		result, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ? AND worn_on = ? AND NOT EXISTS (SELECT 1 FROM outfit_looks WHERE outfit_id = ? AND worn_on = ?)`, left, look.WornOn, left, look.WornOn)
		if err != nil {
			return run, err
		}
		if removed, _ := result.RowsAffected(); removed > 0 {
			moves.WearingsRemoved = append(moves.WearingsRemoved, outfitWearing{OutfitID: left, WornOn: look.WornOn})
		}
		held, err := stringColumn(transaction, `SELECT possession_id FROM outfit_possessions WHERE outfit_id = ? ORDER BY position`, left)
		if err != nil {
			return run, err
		}
		kept, err := outfitPiecesFromLooks(transaction, left)
		if err != nil {
			return run, err
		}
		for _, piece := range held {
			if contains(kept, piece) {
				continue
			}
			if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ? AND possession_id = ?`, left, piece); err != nil {
				return run, err
			}
			moves.PiecesRemoved = append(moves.PiecesRemoved, outfitPiece{OutfitID: left, PossessionID: piece})
		}
		run.Counts["looks_not_outfits"]++
		run.Actions = append(run.Actions, AuditAction{Action: "not_an_outfit", Records: []string{lookID, left}, Reason: reason})
	}
	// Within a day, the looks showing more main pieces go first, so a look
	// showing only one finds the outfit of that day it belongs to.
	lookIDs, err := stringColumn(transaction, `SELECT id FROM outfit_looks WHERE outfit_id IS NULL ORDER BY worn_on,
		(SELECT COUNT(DISTINCT possession_id) FROM outfit_slots WHERE look_id = outfit_looks.id AND possession_id IS NOT NULL
			AND group_key IN (`+placeholders(len(outfitIdentityGroups))+`)) DESC, seq`, stringsToAny(outfitIdentityGroups)...)
	if err != nil {
		return run, err
	}
	// Outfits a look may join: those earlier rebuilds made, then those this
	// one makes.
	candidates := []*identityCandidate{}
	earlier, err := stringColumn(transaction, `SELECT id FROM outfits WHERE deleted_at = 0 AND created_by LIKE 'outfit_rebuild_%' ORDER BY seq`)
	if err != nil {
		return run, err
	}
	for _, outfitID := range earlier {
		candidate, err := outfitLooks(transaction, outfitID, "")
		if err != nil {
			return run, err
		}
		candidates = append(candidates, candidate)
	}
	for _, lookID := range lookIDs {
		run.Looked++
		look, err := lookByID(transaction, lookID)
		if err != nil {
			return run, err
		}
		if reason := whyNotAnOutfit(look); reason != "" {
			if err := detachReadings(lookID); err != nil {
				return run, err
			}
			if look.NotAnOutfit == reason {
				run.Looked--
				continue // judged so before, or just now taken out of its outfit
			}
			if err := mark(look, reason); err != nil {
				return run, err
			}
			run.Counts["looks_not_outfits"]++
			run.Actions = append(run.Actions, AuditAction{Action: "not_an_outfit", Records: []string{lookID}, Reason: reason})
			continue
		}
		if err := mark(look, ""); err != nil {
			return run, err
		}
		pieces, err := lookPieces(transaction, lookID)
		if err != nil {
			return run, err
		}
		if len(pieces) == 0 {
			run.leave(lookID, "none of its slots is linked to a piece yet")
			continue
		}
		identity, err := looksIdentity(transaction, lookID)
		if err != nil {
			return run, err
		}
		var target string
		if joined := bestIdentityMatch(identity, look.WornOn, candidates); joined != nil {
			target = joined.outfitID
			added, err := addOutfitPieces(transaction, target, pieces)
			if err != nil {
				return run, err
			}
			if !contains(moves.OutfitsMade, target) {
				for _, piece := range added {
					moves.PiecesAdded = append(moves.PiecesAdded, outfitPiece{OutfitID: target, PossessionID: piece})
				}
				run.Counts["looks_joined_earlier_outfits"]++
			} else {
				run.Counts["looks_joined"]++
			}
			joined.identity.add(identity)
			joined.looks++
			if !contains(joined.days, look.WornOn) {
				joined.days = append(joined.days, look.WornOn)
			}
		} else {
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
			made := &identityCandidate{outfitID: target, identity: outfitIdentity{}, looks: 1, days: []string{look.WornOn}}
			made.identity.add(identity)
			candidates = append(candidates, made)
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
		reviewMoves, err := lookReviewMoves(transaction, lookID, target)
		if err != nil {
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
	for outfitID := range emptied {
		if _, counted := takenFrom[outfitID]; !counted {
			oldOutfits = append(oldOutfits, outfitID)
		}
	}
	for _, look := range moves.Looks {
		if look.NewOutfitID == "" && look.OldOutfitID != nil && !contains(oldOutfits, *look.OldOutfitID) {
			oldOutfits = append(oldOutfits, *look.OldOutfitID)
		}
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
		var looks int
		if err := transaction.QueryRow(`SELECT COUNT(*) FROM outfit_looks WHERE outfit_id = ?`, outfitID).Scan(&looks); err != nil {
			return run, err
		}
		if remaining > 0 || looks > 0 || deletedAt != 0 {
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
		var mergedInto any // none when only looks that are not outfits left it
		if most != "" {
			mergedInto = most
		}
		if _, err := transaction.Exec(`UPDATE outfits SET deleted_at = ?, merged_into_id = ?, updated_at = ? WHERE id = ?`, now, mergedInto, now, outfitID); err != nil {
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
		if current.String != look.NewOutfitID {
			return OutfitRebuild{}, fmt.Errorf("%w: %s has moved to %q since the rebuild; a slot answer changed it", ErrConflict, look.ID, current.String)
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
		if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = ?, updated_at = ? WHERE id = ?`, move.OldOutfitID, now, move.ID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for index := len(rebuild.Moved.LooksMarked) - 1; index >= 0; index-- {
		marked := rebuild.Moved.LooksMarked[index]
		if _, err := transaction.Exec(`UPDATE outfit_looks SET not_an_outfit = ?, updated_at = ? WHERE id = ?`, marked.Before, now, marked.LookID); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, wearing := range rebuild.Moved.WearingsRemoved {
		if _, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, wearing.OutfitID, wearing.WornOn); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, piece := range rebuild.Moved.PiecesRemoved {
		if _, err := addOutfitPieces(transaction, piece.OutfitID, []string{piece.PossessionID}); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, wearing := range rebuild.Moved.WearingsAdded {
		if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ? AND worn_on = ?`, wearing.OutfitID, wearing.WornOn); err != nil {
			return OutfitRebuild{}, err
		}
	}
	for _, piece := range rebuild.Moved.PiecesAdded {
		if _, err := transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ? AND possession_id = ?`, piece.OutfitID, piece.PossessionID); err != nil {
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

func stringsToAny(values []string) []any {
	converted := make([]any, len(values))
	for index, value := range values {
		converted[index] = value
	}
	return converted
}
