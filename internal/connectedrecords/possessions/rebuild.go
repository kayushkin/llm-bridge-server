package possessions

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
// are not part of an outfit's identity. A look is an outfit when no main
// piece visible in both differs (in each main group both show, one's pieces
// are all among the other's; a piece out of view never counts against), and
// they share a top, one-piece or outerwear, or two main pieces; a look
// showing only one main piece also matches on it the outfit worn that same
// day. The outfit holds the union of the pieces its looks were linked to;
// each look keeps its own slots. Every rebuild places every look again, as
// one transaction and one outfit_rebuild record, which undo reverses.

// outfitIdentityGroups are the wearable groups whose pieces say which
// outfit a look is.
var outfitIdentityGroups = []string{"garment:top", "garment:bottom", "garment:one_piece", "garment:outerwear"}

// outfitAnchorGroups are the main groups one shared piece of which makes two
// looks one outfit on any day, when nothing visible in both differs.
var outfitAnchorGroups = []string{"garment:top", "garment:one_piece", "garment:outerwear"}

// leastMatchingIdentityPieces is how many main pieces two looks sharing no
// top, one-piece or outerwear must share to be one outfit.
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
// with those, and how many main pieces they share. Nothing visible in both
// may differ: in every main group both show, one's pieces must all be among
// the other's. Then they are one outfit when they share a top, one-piece or
// outerwear, or two main pieces; or, for a look showing only one main
// piece, that one when the outfit was worn the same day (sameDay).
func identityMatch(look, outfit outfitIdentity, sameDay bool) (shared int, same bool) {
	anchored := false
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
		anchored = anchored || (inBoth > 0 && contains(outfitAnchorGroups, group))
	}
	if anchored || shared >= leastMatchingIdentityPieces {
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
func addOutfitPieces(transaction *databaseTransaction, outfitID string, pieces []string) ([]string, error) {
	added := []string{}
	for _, piece := range pieces {
		result, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position)
			VALUES (?, ?, (SELECT COALESCE(MAX(position) + 1, 0) FROM outfit_possessions WHERE outfit_id = ?)) ON CONFLICT DO NOTHING`, outfitID, piece, outfitID)
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
		if contains(upperBodyGroups, slot.Group) && (slot.PossessionID != nil || slot.DecidedBy == "" || slot.DecidedBy == "best_guess" || slot.DecidedBy == "no_pick") {
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
	// OutfitsBefore holds every existing outfit the rebuild changed, as it
	// was; undo restores each whole.
	OutfitsBefore []outfitSnapshot `json:"outfits_before"`
	Reviews       []pointerMove    `json:"reviews"`
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

// RebuildOutfitsFromLooks places every look again, by the rules above, as
// one transaction: looks that are not outfits leave theirs, the rest are
// grouped into outfits, and each group keeps the live outfit most of its
// looks were in (or a new one). Outfits left with no look and no reading
// are deleted, merged_into_id naming the outfit that took most of their
// looks. copies names the looks whose every photo is a copy (a screenshot)
// with why: they join their outfit but add no day of their own.
func (s *Store) RebuildOutfitsFromLooks(ctx context.Context, actor string, copies map[string]string) (AuditRun, error) {
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
		WearingsRemoved: []outfitWearing{}, WearingsAdded: []outfitWearing{}, Looks: []pointerMove{}, Reviews: []pointerMove{}, OutfitsBefore: []outfitSnapshot{}}
	snapshotted := map[string]bool{}
	snapshot := func(outfitID string) error {
		if snapshotted[outfitID] {
			return nil
		}
		taken, err := takeOutfitSnapshot(transaction, outfitID)
		if err != nil {
			return err
		}
		if err := saveRevision(transaction, "outfits", "id", outfitID, actor, now); err != nil {
			return err
		}
		snapshotted[outfitID] = true
		moves.OutfitsBefore = append(moves.OutfitsBefore, taken)
		return nil
	}

	// Every look, a day at a time; within a day the looks showing more main
	// pieces first, so a look showing only one finds its day's outfit.
	lookIDs, err := stringColumn(transaction, `SELECT id FROM outfit_looks ORDER BY worn_on,
		(SELECT COUNT(DISTINCT possession_id) FROM outfit_slots WHERE look_id = outfit_looks.id AND possession_id IS NOT NULL
			AND group_key IN (`+placeholders(len(outfitIdentityGroups))+`)) DESC, seq`, stringsToAny(outfitIdentityGroups)...)
	if err != nil {
		return run, err
	}
	type group struct {
		candidate *identityCandidate
		looks     []Look
		pieces    []string
	}
	groups := []*group{}
	candidates := []*identityCandidate{}
	groupOf := map[*identityCandidate]*group{}
	targetOf := map[string]string{} // look → outfit, "" for none
	looksByID := map[string]Look{}
	for _, lookID := range lookIDs {
		run.Looked++
		look, err := lookByID(transaction, lookID)
		if err != nil {
			return run, err
		}
		looksByID[lookID] = look
		targetOf[lookID] = ""
		reason := whyNotAnOutfit(look)
		if reason != look.NotAnOutfit {
			if _, err := transaction.Exec(`UPDATE outfit_looks SET not_an_outfit = ?, updated_at = ? WHERE id = ?`, reason, now, lookID); err != nil {
				return run, err
			}
			moves.LooksMarked = append(moves.LooksMarked, lookMark{LookID: lookID, Before: look.NotAnOutfit, After: reason})
			if reason != "" {
				run.Counts["looks_not_outfits"]++
				run.Actions = append(run.Actions, AuditAction{Action: "not_an_outfit", Records: []string{lookID}, Reason: reason})
			}
		}
		if reason != "" {
			continue
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
		day := look.WornOn
		if copies[lookID] != "" {
			day = "" // a copy is no day of its own
			run.Counts["looks_from_copies"]++
		}
		joined := bestIdentityMatch(identity, day, candidates)
		if joined == nil {
			joined = &identityCandidate{identity: outfitIdentity{}, days: []string{}}
			candidates = append(candidates, joined)
			made := &group{candidate: joined}
			groups = append(groups, made)
			groupOf[joined] = made
		}
		joined.identity.add(identity)
		joined.looks++
		if day != "" && !contains(joined.days, day) {
			joined.days = append(joined.days, day)
		}
		placed := groupOf[joined]
		placed.looks = append(placed.looks, look)
		for _, piece := range pieces {
			if !contains(placed.pieces, piece) {
				placed.pieces = append(placed.pieces, piece)
			}
		}
	}

	// Each group keeps the live outfit most of its looks were in, unless an
	// earlier group kept it.
	kept := map[string]bool{}
	takenFrom := map[string]map[string]int{} // old outfit → new outfit → looks moved
	for _, placed := range groups {
		counts := map[string]int{}
		for _, look := range placed.looks {
			if look.OutfitID != nil {
				counts[*look.OutfitID]++
			}
		}
		target, best, bestSequence := "", 0, int64(0)
		for outfitID, count := range counts {
			if kept[outfitID] {
				continue
			}
			var deletedAt, outfitSequence int64
			if err := transaction.QueryRow(`SELECT deleted_at, seq FROM outfits WHERE id = ?`, outfitID).Scan(&deletedAt, &outfitSequence); err != nil {
				return run, err
			}
			if deletedAt != 0 {
				continue
			}
			if count > best || (count == best && outfitSequence < bestSequence) {
				target, best, bestSequence = outfitID, count, outfitSequence
			}
		}
		if target == "" {
			var outfitSequence int64
			if target, outfitSequence, err = nextID(transaction, "outfits", "outfit"); err != nil {
				return run, err
			}
			if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at) VALUES (?, ?, '', '', '', 'kept', 'Rebuilt from the looks read from photos', '', ?, ?, ?)`,
				target, outfitSequence, rebuildID, now, now); err != nil {
				return run, err
			}
			moves.OutfitsMade = append(moves.OutfitsMade, target)
			run.Counts["outfits_made"]++
		} else {
			if err := snapshot(target); err != nil {
				return run, err
			}
			run.Counts["outfits_kept"]++
		}
		kept[target] = true
		placed.candidate.outfitID = target
		for _, look := range placed.looks {
			targetOf[look.ID] = target
			if look.OutfitID != nil && *look.OutfitID != target {
				if takenFrom[*look.OutfitID] == nil {
					takenFrom[*look.OutfitID] = map[string]int{}
				}
				takenFrom[*look.OutfitID][target]++
			}
		}
	}

	// Looks and their readings point at their outfits.
	for _, lookID := range lookIDs {
		look, target := looksByID[lookID], targetOf[lookID]
		var old *string
		if look.OutfitID != nil {
			old = look.OutfitID
			if err := snapshot(*old); err != nil {
				return run, err
			}
		}
		var newOutfit any
		if target != "" {
			newOutfit = target
		}
		if (old == nil && target != "") || (old != nil && *old != target) {
			if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = ?, updated_at = ? WHERE id = ?`, newOutfit, now, lookID); err != nil {
				return run, err
			}
			moves.Looks = append(moves.Looks, pointerMove{ID: lookID, OldOutfitID: old, NewOutfitID: target})
			run.Counts["looks_moved"]++
		}
		reviewMoves, err := lookReviewMoves(transaction, lookID, target)
		if err != nil {
			return run, err
		}
		for _, move := range reviewMoves {
			if (move.OldOutfitID == nil && target == "") || (move.OldOutfitID != nil && *move.OldOutfitID == target) {
				continue
			}
			if move.OldOutfitID != nil {
				if err := snapshot(*move.OldOutfitID); err != nil {
					return run, err
				}
			}
			if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ?`, newOutfit, move.ID); err != nil {
				return run, err
			}
			moves.Reviews = append(moves.Reviews, move)
		}
	}

	// Each outfit holds its looks' pieces, is worn on their days (a copy's
	// day aside), and carries the owner's marks of the outfits its looks
	// came from.
	for _, placed := range groups {
		target := placed.candidate.outfitID
		if err := replaceOutfitPossessions(transaction, target, placed.pieces); err != nil {
			return run, err
		}
		if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ?`, target); err != nil {
			return run, err
		}
		photoIDs := []string{}
		from := []string{}
		for _, look := range placed.looks {
			photoIDs = append(photoIDs, look.PhotoIDs...)
			if look.OutfitID != nil && *look.OutfitID != target && !contains(from, *look.OutfitID) {
				from = append(from, *look.OutfitID)
			}
			if copies[look.ID] != "" {
				continue
			}
			if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?) ON CONFLICT DO NOTHING`, target, look.WornOn); err != nil {
				return run, err
			}
		}
		if err := carryOwnerMarks(transaction, target, from, photoIDs); err != nil {
			return run, err
		}
	}

	// Outfits left with no look and no reading go.
	touched := []string{}
	for outfitID := range snapshotted {
		touched = append(touched, outfitID)
	}
	sort.Strings(touched)
	for _, outfitID := range touched {
		var deletedAt int64
		var looks, readings int
		if err := transaction.QueryRow(`SELECT deleted_at, (SELECT COUNT(*) FROM outfit_looks WHERE outfit_id = outfits.id), (SELECT COUNT(*) FROM photo_reviews WHERE outfit_id = outfits.id)
			FROM outfits WHERE id = ?`, outfitID).Scan(&deletedAt, &looks, &readings); err != nil {
			return run, err
		}
		if deletedAt != 0 || looks > 0 || readings > 0 {
			continue
		}
		most, mostCount := "", 0
		for target, count := range takenFrom[outfitID] {
			if count > mostCount || (count == mostCount && target < most) {
				most, mostCount = target, count
			}
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
	run.Counts["outfits"] = len(groups)
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

// RebuildOutfitsFromLooks asks photo-store which looks are made only of
// copies (screenshots), then rebuilds.
func (reviewer *Reviewer) RebuildOutfitsFromLooks(ctx context.Context, actor string) (AuditRun, error) {
	copies, err := reviewer.looksOfCopies(ctx)
	if err != nil {
		return newAuditRun(), err
	}
	return reviewer.Store.RebuildOutfitsFromLooks(ctx, actor, copies)
}

// looksOfCopies answers each look whose every photo is a copy, with why.
func (reviewer *Reviewer) looksOfCopies(ctx context.Context) (map[string]string, error) {
	rows, err := reviewer.Store.db.Query(`SELECT look_id, photo_id FROM outfit_look_photos ORDER BY look_id, photo_id`)
	if err != nil {
		return nil, err
	}
	photosOf := map[string][]string{}
	order := []string{}
	for rows.Next() {
		var lookID, photoID string
		if err := rows.Scan(&lookID, &photoID); err != nil {
			rows.Close()
			return nil, err
		}
		if _, seen := photosOf[lookID]; !seen {
			order = append(order, lookID)
		}
		photosOf[lookID] = append(photosOf[lookID], photoID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	why := map[string]string{}
	copies := map[string]string{}
	for _, lookID := range order {
		reason := ""
		for _, photoID := range photosOf[lookID] {
			if _, asked := why[photoID]; !asked {
				answer, err := reviewer.Photos.WhyACopy(ctx, photoID)
				if err != nil {
					return nil, fmt.Errorf("is %s a copy: %w", photoID, err)
				}
				why[photoID] = answer
			}
			if why[photoID] == "" {
				reason = ""
				break
			}
			reason = why[photoID]
		}
		if reason != "" {
			copies[lookID] = reason
		}
	}
	return copies, nil
}

// outfitSnapshot is an outfit as it was before a rebuild changed it, so
// undo can put it back whole.
type outfitSnapshot struct {
	OutfitID     string     `json:"outfit_id"`
	Pieces       []string   `json:"pieces"` // in position order
	Wearings     []string   `json:"wearings"`
	Favorite     bool       `json:"favorite"`
	Tags         []string   `json:"tags"`
	CoverPick    *coverPick `json:"cover_pick"`
	DeletedAt    int64      `json:"deleted_at"`
	MergedIntoID *string    `json:"merged_into_id"`
}

type coverPick struct {
	PhotoID  string `json:"photo_id"`
	ChosenBy string `json:"chosen_by"`
	ChosenAt int64  `json:"chosen_at"`
}

func takeOutfitSnapshot(q querier, outfitID string) (outfitSnapshot, error) {
	taken := outfitSnapshot{OutfitID: outfitID}
	var tags string
	var mergedInto sql.NullString
	if err := q.QueryRow(`SELECT favorite, tags, deleted_at, merged_into_id FROM outfits WHERE id = ?`, outfitID).Scan(&taken.Favorite, &tags, &taken.DeletedAt, &mergedInto); err != nil {
		return taken, fmt.Errorf("snapshot %s: %w", outfitID, err)
	}
	if err := json.Unmarshal([]byte(tags), &taken.Tags); err != nil {
		return taken, fmt.Errorf("%s holds tags that are not a JSON list: %w", outfitID, err)
	}
	if mergedInto.Valid {
		taken.MergedIntoID = &mergedInto.String
	}
	var err error
	if taken.Pieces, err = stringColumn(q, `SELECT possession_id FROM outfit_possessions WHERE outfit_id = ? ORDER BY position`, outfitID); err != nil {
		return taken, err
	}
	if taken.Wearings, err = stringColumn(q, `SELECT worn_on FROM outfit_wearings WHERE outfit_id = ? ORDER BY worn_on`, outfitID); err != nil {
		return taken, err
	}
	var pick coverPick
	err = q.QueryRow(`SELECT photo_id, chosen_by, chosen_at FROM outfit_cover_choices WHERE outfit_id = ?`, outfitID).Scan(&pick.PhotoID, &pick.ChosenBy, &pick.ChosenAt)
	if err == nil {
		taken.CoverPick = &pick
	} else if !errors.Is(err, sql.ErrNoRows) {
		return taken, err
	}
	return taken, nil
}

func restoreOutfitSnapshot(transaction *databaseTransaction, taken outfitSnapshot, now int64) error {
	if err := replaceOutfitPossessions(transaction, taken.OutfitID, taken.Pieces); err != nil {
		return err
	}
	if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ?`, taken.OutfitID); err != nil {
		return err
	}
	for _, day := range taken.Wearings {
		if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, taken.OutfitID, day); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`DELETE FROM outfit_cover_choices WHERE outfit_id = ?`, taken.OutfitID); err != nil {
		return err
	}
	if pick := taken.CoverPick; pick != nil {
		if _, err := transaction.Exec(`INSERT INTO outfit_cover_choices (outfit_id, photo_id, chosen_by, chosen_at) VALUES (?, ?, ?, ?)`, taken.OutfitID, pick.PhotoID, pick.ChosenBy, pick.ChosenAt); err != nil {
			return err
		}
	}
	tags := taken.Tags
	if tags == nil {
		tags = []string{}
	}
	_, err := transaction.Exec(`UPDATE outfits SET favorite = ?, tags = ?, deleted_at = ?, merged_into_id = ?, updated_at = ? WHERE id = ?`,
		taken.Favorite, mustJSON(tags), taken.DeletedAt, taken.MergedIntoID, now, taken.OutfitID)
	return err
}

// carryOwnerMarks gives an outfit the owner's marks of the outfits its
// looks came from: favorite when any was, the union of their tags, and the
// owner's cover pick when it has none and a pick's photo is one of its
// looks' photos.
func carryOwnerMarks(transaction *databaseTransaction, target string, from []string, photoIDs []string) error {
	if len(from) == 0 {
		return nil
	}
	mine, err := takeOutfitSnapshot(transaction, target)
	if err != nil {
		return err
	}
	favorite, tags, pick := mine.Favorite, append([]string{}, mine.Tags...), mine.CoverPick
	for _, outfitID := range from {
		theirs, err := takeOutfitSnapshot(transaction, outfitID)
		if err != nil {
			return err
		}
		favorite = favorite || theirs.Favorite
		for _, tag := range theirs.Tags {
			if !contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
		if pick == nil && theirs.CoverPick != nil && contains(photoIDs, theirs.CoverPick.PhotoID) {
			pick = theirs.CoverPick
			if _, err := transaction.Exec(`INSERT INTO outfit_cover_choices (outfit_id, photo_id, chosen_by, chosen_at) VALUES (?, ?, ?, ?)`, target, pick.PhotoID, pick.ChosenBy, pick.ChosenAt); err != nil {
				return err
			}
		}
	}
	_, err = transaction.Exec(`UPDATE outfits SET favorite = ?, tags = ? WHERE id = ?`, favorite, mustJSON(tags), target)
	return err
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
		if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?) ON CONFLICT DO NOTHING`, wearing.OutfitID, wearing.WornOn); err != nil {
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
	for _, taken := range rebuild.Moved.OutfitsBefore {
		if err := saveRevision(transaction, "outfits", "id", taken.OutfitID, actor, now); err != nil {
			return OutfitRebuild{}, err
		}
		if err := restoreOutfitSnapshot(transaction, taken, now); err != nil {
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
