package possessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// Outfit consolidation: outfit readings made one outfit per photo whenever
// pieces were seen differently, so one day's clothes became several
// outfits. ConsolidateOutfits finds outfits that may be one and has both
// readers compare them piece by piece.

// Two outfits worn on different days are compared only when their pieces
// overlap at least this much (shared pieces over all pieces).
const outfitOverlapToCompare = 0.5

// selfies of each outfit shown in one comparison
const outfitPhotosPerSide = 3

var outfitComparisonSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["same_outfit","reason","pieces"],"properties":{
	"same_outfit":{"type":"boolean","description":"true when both sets of photos show the owner in the same clothes and accessories"},
	"reason":{"type":"string","description":"A few words: what shows it, or what differs"},
	"pieces":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["piece","match","reason"],"properties":{
		"piece":{"type":"string","description":"A possession id from outfit A's pieces"},
		"match":{"type":"string","description":"The possession id of outfit B's piece that is the same physical item; none when B's photos show where it would be and it is not there or a different item is; not_visible when B's photos do not show where it would be"},
		"reason":{"type":"string"}}}}}}`)

type pieceAnswer struct {
	Piece  string `json:"piece"`
	Match  string `json:"match"`
	Reason string `json:"reason"`
}

type outfitComparison struct {
	SameOutfit bool          `json:"same_outfit"`
	Reason     string        `json:"reason"`
	Pieces     []pieceAnswer `json:"pieces"`
}

// liveOutfits answers every live outfit that is not rejected, oldest first.
func (s *Store) liveOutfits() ([]Outfit, error) {
	ids, err := stringColumn(s.db, `SELECT id FROM outfits WHERE deleted_at = 0 AND status != 'rejected' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	outfits := make([]Outfit, 0, len(ids))
	for _, id := range ids {
		outfit, err := outfitByID(s.db, id, false)
		if err != nil {
			return nil, err
		}
		outfits = append(outfits, outfit)
	}
	return outfits, nil
}

func pieceSetKey(outfit Outfit) string {
	pieces := append([]string{}, outfit.PossessionIDs...)
	sort.Strings(pieces)
	return strings.Join(pieces, ",")
}

// outfitKeeper picks which of two outfits a combine keeps: the one worn on
// more days, then the older.
func outfitKeeper(first, second Outfit) (kept, duplicate Outfit) {
	if len(first.WornOn) != len(second.WornOn) {
		if len(first.WornOn) > len(second.WornOn) {
			return first, second
		}
		return second, first
	}
	if first.CreatedAt != second.CreatedAt {
		if first.CreatedAt < second.CreatedAt {
			return first, second
		}
		return second, first
	}
	if first.ID < second.ID {
		return first, second
	}
	return second, first
}

func overlap(first, second []string) float64 {
	union := map[string]bool{}
	shared := 0
	for _, piece := range first {
		union[piece] = true
	}
	for _, piece := range second {
		if union[piece] {
			shared++
		}
		union[piece] = true
	}
	if len(union) == 0 {
		return 0
	}
	return float64(shared) / float64(len(union))
}

func difference(first, second []string) []string {
	left := []string{}
	for _, piece := range first {
		if !contains(second, piece) {
			left = append(left, piece)
		}
	}
	return left
}

// ConsolidateOutfits combines outfits that are one outfit recorded more
// than once. Outfits holding exactly the same pieces are combined without
// asking. Then each pair worn on a common day, or whose pieces overlap by
// half or more on different days, is shown to both readers: the selfies of
// each and pictures of the pieces only one holds. Only when both say it is
// the same outfit and map every such piece the same way (to the same piece
// of the other, or not visible there) are the outfits combined, and pieces
// both mapped to one another merged first; every combine and merge is
// undoable. Any disagreement is a decision question on a sighting of one of
// them. A pair already asked about is left to the owner's answer.
func (reviewer *Reviewer) ConsolidateOutfits(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	cache := newPictureCache(reviewer.Photos)
	examined := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		outfits, err := store.liveOutfits()
		if err != nil {
			return run, err
		}
		if combined, err := store.combineIdenticalOutfits(ctx, outfits, actor, &run); err != nil {
			return run, err
		} else if combined {
			continue
		}
		type candidate struct {
			first, second Outfit
			sameDay       bool
			key           string
		}
		candidates := []candidate{}
		for i := range outfits {
			for j := i + 1; j < len(outfits); j++ {
				first, second := outfits[i], outfits[j]
				sameDay := len(difference(first.WornOn, second.WornOn)) < len(first.WornOn)
				if !sameDay && overlap(first.PossessionIDs, second.PossessionIDs) < outfitOverlapToCompare {
					continue
				}
				key := first.ID + "=" + pieceSetKey(first) + "|" + second.ID + "=" + pieceSetKey(second)
				if examined[key] {
					continue
				}
				candidates = append(candidates, candidate{first, second, sameDay, key})
			}
		}
		if len(candidates) == 0 {
			return run, nil
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].sameDay && !candidates[j].sameDay })
		next := candidates[0]
		examined[next.key] = true
		run.Looked++
		if err := reviewer.compareOutfits(ctx, cache, next.first, next.second, actor, &run); err != nil {
			run.fail(next.first.ID+" "+next.second.ID, err)
		}
	}
}

// combineIdenticalOutfits combines the first set of live outfits holding
// exactly the same pieces it finds, and says whether it combined any.
func (s *Store) combineIdenticalOutfits(ctx context.Context, outfits []Outfit, actor string, run *AuditRun) (bool, error) {
	bySet := map[string][]Outfit{}
	keys := []string{}
	for _, outfit := range outfits {
		if len(outfit.PossessionIDs) == 0 {
			continue
		}
		key := pieceSetKey(outfit)
		if _, seen := bySet[key]; !seen {
			keys = append(keys, key)
		}
		bySet[key] = append(bySet[key], outfit)
	}
	for _, key := range keys {
		same := bySet[key]
		if len(same) < 2 {
			continue
		}
		kept := same[0]
		for _, other := range same[1:] {
			kept, _ = outfitKeeper(kept, other)
		}
		for _, other := range same {
			if other.ID == kept.ID {
				continue
			}
			merge, err := s.CombineOutfits(ctx, other.ID, kept.ID, actor, "it holds exactly the same pieces")
			if err != nil {
				return false, fmt.Errorf("combine %s into %s: %w", other.ID, kept.ID, err)
			}
			run.Counts["combined_identical"]++
			run.Actions = append(run.Actions, AuditAction{Action: "combined", Records: []string{other.ID, kept.ID}, MergeID: merge.ID, Reason: merge.Reason})
		}
		return true, nil
	}
	return false, nil
}

// questionAboutOutfitPair answers a question, open or answered, already
// asked about combining these two outfits; "" when none.
func (s *Store) questionAboutOutfitPair(first, second string) (string, error) {
	for _, pair := range [][2]string{{first, second}, {second, first}} {
		ids, err := stringColumn(s.db, `SELECT id FROM owner_questions WHERE strpos(choices, '"combine_outfits"') > 0 AND strpos(choices, ?) > 0 AND strpos(choices, ?) > 0 ORDER BY seq LIMIT 1`,
			jsonField("outfit_id", pair[0]), jsonField("into_outfit_id", pair[1]))
		if err != nil {
			return "", err
		}
		if len(ids) > 0 {
			return ids[0], nil
		}
	}
	return "", nil
}

// compareOutfits shows one pair to both readers and acts on what they say.
func (reviewer *Reviewer) compareOutfits(ctx context.Context, cache *pictureCache, first, second Outfit, actor string, run *AuditRun) error {
	store := reviewer.Store
	if questionID, err := store.questionAboutOutfitPair(first.ID, second.ID); err != nil {
		return err
	} else if questionID != "" {
		run.leave(first.ID+" "+second.ID, "already asked in "+questionID)
		return nil
	}
	// A is the outfit with fewer pieces of its own, so the readers map the
	// shorter list.
	onlyFirst, onlySecond := difference(first.PossessionIDs, second.PossessionIDs), difference(second.PossessionIDs, first.PossessionIDs)
	outfitA, outfitB, onlyA, onlyB := first, second, onlyFirst, onlySecond
	if len(onlySecond) < len(onlyFirst) {
		outfitA, outfitB, onlyA, onlyB = second, first, onlySecond, onlyFirst
	}
	if len(outfitA.PhotoIDs) == 0 || len(outfitB.PhotoIDs) == 0 {
		run.leave(first.ID+" "+second.ID, "one has no photo it was read from")
		return nil
	}
	shared := []string{}
	for _, piece := range outfitA.PossessionIDs {
		if contains(outfitB.PossessionIDs, piece) {
			shared = append(shared, piece)
		}
	}
	images := []msg.OneShotImage{}
	var listing strings.Builder
	addPhotos := func(name string, outfit Outfit) error {
		for index, photoID := range outfit.PhotoIDs {
			if index == outfitPhotosPerSide {
				break
			}
			picture, err := cache.thumbnail(ctx, photoID)
			if err != nil {
				return fmt.Errorf("read %s: %w", photoID, err)
			}
			images = append(images, jpegImage(picture))
			fmt.Fprintf(&listing, "Picture %d: a photo of the owner in outfit %s (%s)\n", len(images), name, outfit.ID)
		}
		return nil
	}
	if err := addPhotos("A", outfitA); err != nil {
		return err
	}
	if err := addPhotos("B", outfitB); err != nil {
		return err
	}
	kinds := map[string]string{}
	views := map[string]map[string]any{}
	describe := func(name string, pieces []string) error {
		for _, pieceID := range pieces {
			view, err := store.GetPossession(pieceID, false)
			if err != nil {
				return err
			}
			views[pieceID] = view
			kinds[pieceID] = view["kind"].(string)
			picture, err := cache.coverCrop(ctx, store.db, pieceID)
			where := "no picture"
			if err == nil {
				images = append(images, jpegImage(picture))
				where = fmt.Sprintf("picture %d", len(images))
			}
			fmt.Fprintf(&listing, "Outfit %s's piece (%s): %s\n", name, where, describeWearable(view))
		}
		return nil
	}
	if err := describe("A", onlyA); err != nil {
		return err
	}
	if err := describe("B", onlyB); err != nil {
		return err
	}
	sharedText := "none"
	if len(shared) > 0 {
		sharedText = strings.Join(shared, ", ")
	}
	prompt := fmt.Sprintf(`Two outfits are recorded from photos of the owner. A: %s, worn on %s. B: %s, worn on %s. They may be one outfit recorded twice, because a piece was seen differently in each photo.

%s
Pieces both outfits already hold: %s.

Say whether the photos of A and of B show the owner in the same outfit. Then, for each of outfit A's own pieces listed above (and no others), say which of outfit B's own pieces is the same physical item, or none, or not_visible.`,
		outfitA.ID, strings.Join(outfitA.WornOn, ", "), outfitB.ID, strings.Join(outfitB.WornOn, ", "), listing.String(), sharedText)
	answers, err := reviewer.askBothReaders(ctx, auditSystemPrompt, prompt, outfitComparisonSchema, images, 8000)
	if err != nil {
		return err
	}
	comparisons := [2]outfitComparison{}
	models := [2]string{}
	for side, answer := range answers {
		run.CostUSD += answer.CostUSD
		models[side] = answer.Model
		if err := json.Unmarshal(answer.Parsed, &comparisons[side]); err != nil {
			return fmt.Errorf("%s's comparison is not one: %w", answer.Model, err)
		}
	}
	if !comparisons[0].SameOutfit && !comparisons[1].SameOutfit {
		run.Counts["different"]++
		return nil
	}
	maps := [2]map[string]pieceAnswer{{}, {}}
	for side, comparison := range comparisons {
		for _, piece := range comparison.Pieces {
			if contains(onlyA, strings.TrimSpace(piece.Piece)) {
				piece.Piece, piece.Match = strings.TrimSpace(piece.Piece), strings.TrimSpace(piece.Match)
				maps[side][piece.Piece] = piece
			}
		}
	}
	differences := []string{}
	if comparisons[0].SameOutfit != comparisons[1].SameOutfit {
		differences = append(differences, fmt.Sprintf("the same outfit? %s says %v (%s); %s says %v (%s)", models[0], comparisons[0].SameOutfit, comparisons[0].Reason, models[1], comparisons[1].SameOutfit, comparisons[1].Reason))
	}
	pieceMerges := []PieceMerge{}
	targets := map[string]string{}
	for _, pieceID := range onlyA {
		first, answeredFirst := maps[0][pieceID]
		second, answeredSecond := maps[1][pieceID]
		name := views[pieceID]["name"]
		switch {
		case !answeredFirst || !answeredSecond:
			differences = append(differences, fmt.Sprintf("%s (%s): a reader did not say", name, pieceID))
		case first.Match != second.Match:
			differences = append(differences, fmt.Sprintf("%s (%s): %s says %s (%s); %s says %s (%s)", name, pieceID, models[0], first.Match, first.Reason, models[1], second.Match, second.Reason))
		case first.Match == "none":
			differences = append(differences, fmt.Sprintf("%s (%s): both say outfit B does not have it (%s; %s)", name, pieceID, first.Reason, second.Reason))
		case first.Match == "not_visible":
		case !contains(onlyB, first.Match):
			differences = append(differences, fmt.Sprintf("%s (%s): both name %s, which is not one of outfit B's own pieces", name, pieceID, first.Match))
		case kinds[first.Match] != kinds[pieceID]:
			differences = append(differences, fmt.Sprintf("%s (%s) is a %s and %s a %s", name, pieceID, kinds[pieceID], first.Match, kinds[first.Match]))
		case targets[first.Match] != "":
			differences = append(differences, fmt.Sprintf("both map %s and %s to %s", targets[first.Match], pieceID, first.Match))
		default:
			targets[first.Match] = pieceID
			kept, duplicate := keeperOf(views[pieceID], views[first.Match])
			pieceMerges = append(pieceMerges, PieceMerge{DuplicatePossessionID: duplicate["id"].(string), KeptPossessionID: kept["id"].(string)})
		}
	}
	keptOutfit, duplicateOutfit := outfitKeeper(outfitA, outfitB)
	reason := fmt.Sprintf("%s: %s; %s: %s", models[0], comparisons[0].Reason, models[1], comparisons[1].Reason)
	if len(differences) == 0 {
		return store.combineCheckedOutfits(ctx, duplicateOutfit.ID, keptOutfit.ID, pieceMerges, actor, reason, run)
	}
	action, err := store.askAboutOutfits(ctx, duplicateOutfit, keptOutfit, pieceMerges, differences)
	if err != nil {
		return err
	}
	if action.Action == "" {
		run.leave(first.ID+" "+second.ID, action.Reason)
		return nil
	}
	run.Counts["asked"]++
	run.Actions = append(run.Actions, action)
	return nil
}

// combineCheckedOutfits merges the pieces both readers mapped to one
// another, then combines the outfits, in one transaction.
func (s *Store) combineCheckedOutfits(ctx context.Context, duplicateOutfitID, keptOutfitID string, pieceMerges []PieceMerge, actor, reason string, run *AuditRun) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	actions := []AuditAction{}
	for _, piece := range pieceMerges {
		mergeID, err := s.mergePossessionInTransaction(transaction, piece.DuplicatePossessionID, piece.KeptPossessionID, actor, "both readers mapped it to the same piece comparing "+duplicateOutfitID+" and "+keptOutfitID+": "+reason, now)
		if err != nil {
			return fmt.Errorf("merge %s into %s: %w", piece.DuplicatePossessionID, piece.KeptPossessionID, err)
		}
		actions = append(actions, AuditAction{Action: "merged", Records: []string{piece.DuplicatePossessionID, piece.KeptPossessionID}, MergeID: mergeID, Reason: "the same piece in two outfits"})
	}
	mergeID, err := s.combineOutfitsInTransaction(transaction, duplicateOutfitID, keptOutfitID, actor, "both readers saw one outfit: "+reason, now)
	if err != nil {
		return err
	}
	actions = append(actions, AuditAction{Action: "combined", Records: []string{duplicateOutfitID, keptOutfitID}, MergeID: mergeID, Reason: reason})
	if err := transaction.Commit(); err != nil {
		return err
	}
	for _, action := range actions {
		run.Counts[action.Action]++
	}
	run.Actions = append(run.Actions, actions...)
	return nil
}

// askAboutOutfits asks whether two outfits are one, on a sighting of
// either. An AuditAction with no Action says why nothing was asked.
func (s *Store) askAboutOutfits(ctx context.Context, duplicate, kept Outfit, pieceMerges []PieceMerge, differences []string) (AuditAction, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuditAction{}, err
	}
	defer transaction.Rollback()
	sightingID, err := sightingOfOutfit(transaction, duplicate.ID)
	if err != nil {
		return AuditAction{}, err
	}
	if sightingID == "" {
		if sightingID, err = sightingOfOutfit(transaction, kept.ID); err != nil {
			return AuditAction{}, err
		}
	}
	if sightingID == "" {
		return AuditAction{Reason: "neither outfit came from a photo review, so there is no sighting to ask on"}, nil
	}
	mergeText := ""
	if len(pieceMerges) > 0 {
		parts := []string{}
		for _, piece := range pieceMerges {
			parts = append(parts, piece.DuplicatePossessionID+" into "+piece.KeptPossessionID)
		}
		mergeText = ", and merge " + strings.Join(parts, ", ")
	}
	question := fmt.Sprintf("Are outfits %s (worn %s) and %s (worn %s) one outfit? The readers differ: %s.",
		duplicate.ID, strings.Join(duplicate.WornOn, ", "), kept.ID, strings.Join(kept.WornOn, ", "), strings.Join(differences, "; "))
	choices := []Choice{
		{Label: fmt.Sprintf("The same outfit: keep %s%s", kept.ID, mergeText), Resolution: Resolution{Action: "combine_outfits", OutfitID: duplicate.ID, IntoOutfitID: kept.ID, PieceMerges: pieceMerges}},
		{Label: "Different outfits", Resolution: Resolution{Action: "keep_outfits_apart", OutfitID: duplicate.ID, IntoOutfitID: kept.ID}},
	}
	questionID, err := askAuditQuestion(transaction, "outfit_consolidation", sightingID, question, choices, s.unixNow())
	if err != nil {
		return AuditAction{}, err
	}
	return AuditAction{Action: "asked", Records: []string{duplicate.ID, kept.ID, sightingID}, QuestionID: questionID, Reason: question}, transaction.Commit()
}
