package possessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// Identification: for each wearable group, both readers see a tight crop of
// every slot not yet identified, from the full-size original, and the
// covers of the pieces already recorded, and each sorts them into
// individual pieces, told how many the owner expects. The first reader's
// pieces are the best guess; the second reader's sorting and both readers'
// "could also be" hints rank the alternatives for each slot. A piece with a
// recorded possession in it becomes that possession (duplicates merged,
// undoably, when both readers put them together or one of them is only
// unconfirmed); a piece with none becomes a new unconfirmed possession,
// except in a closed group. Each slot is linked to its most likely piece
// (decided_by best_guess) and gets one optional question whose choices are
// its ranked pieces.

// A group whose owner expects at most smallGroupHighestCount pieces is
// sorted in calls of up to smallGroupPicturesPerCall pictures, so a handful
// of watches is seen all at once; others in calls of
// largeGroupPicturesPerCall. A group larger than its call is sorted batch
// by batch, then the batches' pieces (one picture each) are sorted
// together, and so on up until one call holds them.
const (
	smallGroupHighestCount    = 12
	smallGroupPicturesPerCall = 80
	largeGroupPicturesPerCall = 30
)

// GroupCount says how one group came out of identification.
type GroupCount struct {
	Group        string `json:"group"`
	ExpectedLow  *int   `json:"expected_low"`
	ExpectedHigh *int   `json:"expected_high"`
	Slots        int    `json:"slots"`        // slots identified in this pass
	Pieces       int    `json:"pieces"`       // pieces the first reader sorted them into, recorded ones included
	Identified   int    `json:"identified"`   // live owned and unconfirmed possessions in the group after the pass
	WithinRange  *bool  `json:"within_range"` // Identified against the expected range; null when none is set
}

type clusterItem struct {
	Label        string
	SlotID       string
	PossessionID string
	Status       string // a recorded item's status
	Text         string
	Crop         []byte
}

var clusterSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["pieces","uncertain"],"properties":{
	"pieces":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["members","reason"],"properties":{
		"members":{"type":"array","items":{"type":"string"},"description":"Labels of the pictures that show this one physical piece"},
		"reason":{"type":"string"}}}},
	"uncertain":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["item","could_be_with"],"properties":{
		"item":{"type":"string"},
		"could_be_with":{"type":"array","items":{"type":"string"},"description":"Labels in other pieces it could belong with instead"}}}}}}`)

type cropRequest struct {
	key     string
	photoID string
	box     Box
	small   bool
}

// cutCrops cuts every request from the full-size photos, opening each
// photo once.
func (reviewer *Reviewer) cutCrops(ctx context.Context, requests []cropRequest) (map[string][]byte, map[string]error) {
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].photoID < requests[j].photoID })
	crops, failures := map[string][]byte{}, map[string]error{}
	for start := 0; start < len(requests); {
		end := start
		for end < len(requests) && requests[end].photoID == requests[start].photoID {
			end++
		}
		picture, _, err := reviewer.Photos.DetailImage(ctx, requests[start].photoID)
		for _, request := range requests[start:end] {
			if err != nil {
				failures[request.key] = err
				continue
			}
			crop, cropErr := detailCrop(picture, request.box, request.small)
			if cropErr != nil {
				failures[request.key] = fmt.Errorf("cut %s: %w", request.photoID, cropErr)
				continue
			}
			crops[request.key] = crop
		}
		start = end
	}
	return crops, failures
}

// largestPhoto answers the slot photo with the largest box.
func (slot Slot) largestPhoto() SlotPhoto {
	best := slot.Photos[0]
	for _, photo := range slot.Photos[1:] {
		if photo.Box.area() > best.Box.area() {
			best = photo
		}
	}
	return best
}

func describeSlot(slot Slot) string {
	return fmt.Sprintf("worn as %q: %s | %s", slot.Slot, slot.Proposed.Name, truncateWords(slot.Proposed.Description, 25))
}

func expectedText(group WearableGroup) string {
	switch {
	case group.ExpectedLow != nil && group.ExpectedHigh != nil:
		return fmt.Sprintf("The owner says they own between %d and %d of these.", *group.ExpectedLow, *group.ExpectedHigh)
	case group.ExpectedHigh != nil:
		return fmt.Sprintf("The owner says they own at most %d of these.", *group.ExpectedHigh)
	case group.ExpectedLow != nil:
		return fmt.Sprintf("The owner says they own at least %d of these.", *group.ExpectedLow)
	}
	return ""
}

// IdentifySlots identifies every slot not yet identified, group by group.
func (reviewer *Reviewer) IdentifySlots(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	slotIDs, err := stringColumn(store.db, `SELECT id FROM outfit_slots WHERE decided_by = '' ORDER BY seq`)
	if err != nil {
		return run, err
	}
	byGroup := map[string][]Slot{}
	for _, id := range slotIDs {
		slot, err := slotByID(store.db, id)
		if err != nil {
			return run, err
		}
		if len(slot.Photos) == 0 {
			run.leave(id, "no box on any photo")
			continue
		}
		byGroup[slot.Group] = append(byGroup[slot.Group], slot)
	}
	wearables, err := store.liveWearables()
	if err != nil {
		return run, err
	}
	groupNames := []string{}
	for group := range byGroup {
		groupNames = append(groupNames, group)
	}
	sort.Strings(groupNames)

	// Every crop, cut in one sweep over the photos.
	requests := []cropRequest{}
	recordedByGroup := map[string][]map[string]any{}
	for _, group := range groupNames {
		for _, slot := range byGroup[group] {
			photo := slot.largestPhoto()
			requests = append(requests, cropRequest{key: slot.ID, photoID: photo.PhotoID, box: photo.Box, small: smallPieceGroup(group)})
		}
		for _, possession := range wearables {
			status := possession["status"].(string)
			if possessionGroup(possession) != group || (!contains(ownedStatuses, status) && status != "unconfirmed") {
				continue
			}
			for _, link := range possession["photos"].([]PhotoLink) {
				if !link.IsCover {
					continue
				}
				box := Box{0, 0, 1, 1}
				if link.BoxLeft != nil {
					box = Box{*link.BoxLeft, *link.BoxTop, *link.BoxRight, *link.BoxBottom}
				}
				requests = append(requests, cropRequest{key: possession["id"].(string), photoID: link.PhotoID, box: box, small: smallPieceGroup(group)})
				recordedByGroup[group] = append(recordedByGroup[group], possession)
			}
		}
	}
	crops, cropFailures := reviewer.cutCrops(ctx, requests)

	groupFacts, err := store.ListWearableGroups()
	if err != nil {
		return run, err
	}
	factsOf := func(group string) WearableGroup {
		for _, facts := range groupFacts {
			if facts.Group == group {
				return facts
			}
		}
		return WearableGroup{Group: group}
	}
	reportGroups := []GroupCount{}
	for _, group := range groupNames {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		facts := factsOf(group)
		items := []clusterItem{}
		for index, possession := range recordedByGroup[group] {
			id := possession["id"].(string)
			if crop, cut := crops[id]; cut {
				items = append(items, clusterItem{Label: fmt.Sprintf("R%d", index+1), PossessionID: id, Status: possession["status"].(string), Text: describeWearable(possession), Crop: crop})
			} else {
				run.fail(id, fmt.Errorf("cut its cover: %w", cropFailures[id]))
			}
		}
		slotsByID := map[string]Slot{}
		slotCount := 0
		for index, slot := range byGroup[group] {
			crop, cut := crops[slot.ID]
			if !cut {
				run.fail(slot.ID, cropFailures[slot.ID])
				continue
			}
			slotsByID[slot.ID] = slot
			items = append(items, clusterItem{Label: fmt.Sprintf("S%d", index+1), SlotID: slot.ID, Text: describeSlot(slot), Crop: crop})
			slotCount++
		}
		if slotCount == 0 {
			continue
		}
		run.Looked += slotCount
		count, err := reviewer.identifyGroup(ctx, facts, items, slotsByID, actor, &run)
		if err != nil {
			run.fail(group, err)
			continue
		}
		count.Slots = slotCount
		reportGroups = append(reportGroups, count)
	}
	// Counts after every group is done, against the owner's range.
	wearables, err = store.liveWearables()
	if err != nil {
		return run, err
	}
	for index := range reportGroups {
		for _, possession := range wearables {
			status := possession["status"].(string)
			if possessionGroup(possession) == reportGroups[index].Group && (contains(ownedStatuses, status) || status == "unconfirmed") {
				reportGroups[index].Identified++
			}
		}
		facts := factsOf(reportGroups[index].Group)
		reportGroups[index].ExpectedLow, reportGroups[index].ExpectedHigh = facts.ExpectedLow, facts.ExpectedHigh
		if facts.ExpectedLow != nil || facts.ExpectedHigh != nil {
			within := (facts.ExpectedLow == nil || reportGroups[index].Identified >= *facts.ExpectedLow) && (facts.ExpectedHigh == nil || reportGroups[index].Identified <= *facts.ExpectedHigh)
			reportGroups[index].WithinRange = &within
		}
	}
	run.Groups = reportGroups
	return run, nil
}

// sorting is one reader's sorting of items into pieces: piece[i] is the
// piece of item i; alternatives[i] the items it could belong with instead.
type sorting struct {
	piece        []int
	alternatives [][]int
}

func (sorted sorting) members(piece int) []int {
	members := []int{}
	for item, assigned := range sorted.piece {
		if assigned == piece {
			members = append(members, item)
		}
	}
	return members
}

// sortItems has one reader sort items into pieces, in batches when they do
// not fit one call (see the constants above).
func (reviewer *Reviewer) sortItems(ctx context.Context, role string, facts WearableGroup, closed bool, items []clusterItem, wholeGroup bool, run *AuditRun) (sorting, error) {
	perCall := largeGroupPicturesPerCall
	if facts.ExpectedHigh != nil && *facts.ExpectedHigh <= smallGroupHighestCount {
		perCall = smallGroupPicturesPerCall
	}
	if len(items) <= perCall {
		return reviewer.sortInOneCall(ctx, role, facts, closed, items, wholeGroup, run)
	}
	// Batches of similar words together, so a batch holds look-alikes.
	order := make([]int, len(items))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool { return items[order[i]].Text < items[order[j]].Text })
	result := sorting{piece: make([]int, len(items)), alternatives: make([][]int, len(items))}
	representatives := []clusterItem{}
	representativeOf := []int{} // representative index → first piece id
	pieceCount := 0
	for start := 0; start < len(order); start += perCall {
		batch := order[start:min(start+perCall, len(order))]
		batchItems := []clusterItem{}
		for _, index := range batch {
			batchItems = append(batchItems, items[index])
		}
		sorted, err := reviewer.sortInOneCall(ctx, role, facts, closed, batchItems, false, run)
		if err != nil {
			return sorting{}, err
		}
		seen := map[int]int{} // batch piece → global piece
		for position, index := range batch {
			piece, known := seen[sorted.piece[position]]
			if !known {
				piece = pieceCount
				pieceCount++
				seen[sorted.piece[position]] = piece
				representatives = append(representatives, items[index])
				representativeOf = append(representativeOf, piece)
			} else if items[index].PossessionID != "" && representatives[piece].PossessionID == "" {
				representatives[piece] = items[index] // a recorded piece speaks for its batch piece
			}
			result.piece[index] = piece
			for _, alternative := range sorted.alternatives[position] {
				result.alternatives[index] = append(result.alternatives[index], batch[alternative])
			}
		}
	}
	if len(representatives) >= len(items) {
		run.leave(facts.Group, fmt.Sprintf("%s found no two of %d pictures alike within a batch, so the batches were not sorted together", role, len(items)))
		return result, nil
	}
	top, err := reviewer.sortItems(ctx, role, facts, closed, representatives, wholeGroup, run)
	if err != nil {
		return sorting{}, err
	}
	for index := range result.piece {
		result.piece[index] = top.piece[result.piece[index]]
	}
	return result, nil
}

func (reviewer *Reviewer) sortInOneCall(ctx context.Context, role string, facts WearableGroup, closed bool, items []clusterItem, wholeGroup bool, run *AuditRun) (sorting, error) {
	images := []msg.OneShotImage{}
	listing := []string{}
	labelIndex := map[string]int{}
	for index, item := range items {
		images = append(images, jpegImage(item.Crop))
		labelIndex[item.Label] = index
		listing = append(listing, fmt.Sprintf("%s (picture %d): %s", item.Label, index+1, item.Text))
	}
	group := strings.ReplaceAll(facts.Group, ":", " ")
	scope := "These pictures are part of a larger set of the owner's " + group + "."
	if wholeGroup {
		scope = "These pictures are every " + group + " seen on the owner, and the ones already recorded. " + expectedText(facts)
	}
	if closed {
		scope += " The owner says every one they own is recorded: every S picture belongs with an R picture of an owned piece."
	}
	prompt := fmt.Sprintf(`Sort these pictures into the individual physical pieces they show. S pictures are cut from photos of the owner wearing the piece; R pictures are pieces already recorded in the inventory, and two R pictures may be one piece recorded twice. %s

%s

Put every label in exactly one piece; a piece seen once is a piece of one. Judge by color, cut, print, logo, metal, stones, dial and strap, not by how it is worn. In uncertain, name each label that could as well belong with another piece, and labels of that piece.`, scope, strings.Join(listing, "\n"))
	answer, err := reviewer.Models.Ask(ctx, role, auditSystemPrompt, prompt, clusterSchema, images, 8000)
	if err != nil {
		return sorting{}, fmt.Errorf("reader %s: %w", role, err)
	}
	run.CostUSD += answer.CostUSD
	var parsed struct {
		Pieces []struct {
			Members []string `json:"members"`
		} `json:"pieces"`
		Uncertain []struct {
			Item        string   `json:"item"`
			CouldBeWith []string `json:"could_be_with"`
		} `json:"uncertain"`
	}
	if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
		return sorting{}, fmt.Errorf("%s's sorting is not one: %w", answer.Model, err)
	}
	result := sorting{piece: make([]int, len(items)), alternatives: make([][]int, len(items))}
	for index := range result.piece {
		result.piece[index] = -1
	}
	pieceCount := 0
	for _, piece := range parsed.Pieces {
		placed := false
		for _, label := range piece.Members {
			index, known := labelIndex[strings.TrimSpace(label)]
			if !known || result.piece[index] != -1 {
				continue
			}
			result.piece[index] = pieceCount
			placed = true
		}
		if placed {
			pieceCount++
		}
	}
	for index := range result.piece {
		if result.piece[index] == -1 {
			result.piece[index] = pieceCount
			pieceCount++
		}
	}
	for _, uncertain := range parsed.Uncertain {
		index, known := labelIndex[strings.TrimSpace(uncertain.Item)]
		if !known {
			continue
		}
		for _, label := range uncertain.CouldBeWith {
			if other, known := labelIndex[strings.TrimSpace(label)]; known && other != index {
				result.alternatives[index] = append(result.alternatives[index], other)
			}
		}
	}
	return result, nil
}

// identifyGroup sorts one group with both readers and links its slots.
func (reviewer *Reviewer) identifyGroup(ctx context.Context, facts WearableGroup, items []clusterItem, slots map[string]Slot, actor string, run *AuditRun) (GroupCount, error) {
	store := reviewer.Store
	count := GroupCount{Group: facts.Group}
	closed := facts.Closed
	first, err := reviewer.sortItems(ctx, reviewer.FirstReaderRole, facts, closed, items, true, run)
	if err != nil {
		return count, err
	}
	second, err := reviewer.sortItems(ctx, reviewer.SecondReaderRole, facts, closed, items, true, run)
	if err != nil {
		return count, err
	}
	pieces := map[int][]int{}
	pieceOrder := []int{}
	for item, piece := range first.piece {
		if _, seen := pieces[piece]; !seen {
			pieceOrder = append(pieceOrder, piece)
		}
		pieces[piece] = append(pieces[piece], item)
	}
	count.Pieces = len(pieceOrder)

	// Each of the first reader's pieces becomes a possession.
	possessionOf := map[int]string{}
	extraOf := map[int][]string{} // owned pieces the second reader kept apart from the piece's possession
	for _, piece := range pieceOrder {
		members := pieces[piece]
		recorded := []int{}
		for _, item := range members {
			if items[item].PossessionID != "" {
				recorded = append(recorded, item)
			}
		}
		if len(recorded) == 0 {
			if closed {
				continue // nothing new in a closed group: its slots are ranked against the owned pieces
			}
			created, err := store.createPieceFromSlot(ctx, slots[items[largestSlotItem(items, members, slots)].SlotID], actor)
			if err != nil {
				run.fail(facts.Group, fmt.Errorf("record a new piece: %w", err))
				continue
			}
			possessionOf[piece] = created
			run.Counts["new_pieces"]++
			run.Actions = append(run.Actions, AuditAction{Action: "created", Records: []string{created}, Reason: "a piece no recorded possession matched"})
			continue
		}
		views := map[int]map[string]any{}
		keeper := -1
		for _, item := range recorded {
			view, err := store.GetPossession(items[item].PossessionID, false)
			if err != nil {
				return count, err
			}
			views[item] = view
			if keeper == -1 {
				keeper = item
				continue
			}
			kept, _ := keeperOf(views[keeper], view)
			if kept["id"] == view["id"] {
				keeper = item
			}
		}
		if closed && !contains(ownedStatuses, views[keeper]["status"].(string)) {
			continue // only unconfirmed pieces: not a member of the closed group
		}
		possessionOf[piece] = items[keeper].PossessionID
		for _, item := range recorded {
			if item == keeper {
				continue
			}
			bothSay := second.piece[item] == second.piece[keeper]
			if items[item].Status != "unconfirmed" && !bothSay {
				extraOf[piece] = append(extraOf[piece], items[item].PossessionID)
				continue
			}
			merged, err := store.MergePossession(ctx, items[item].PossessionID, items[keeper].PossessionID, actor, "identification of "+facts.Group+": the first reader saw one piece"+map[bool]string{true: ", and so did the second", false: ""}[bothSay])
			if err != nil {
				run.fail(items[item].PossessionID, fmt.Errorf("merge into %s: %w", items[keeper].PossessionID, err))
				continue
			}
			run.Counts["merged"]++
			run.Actions = append(run.Actions, AuditAction{Action: "merged", Records: []string{items[item].PossessionID, items[keeper].PossessionID}, MergeID: merged["merge_id"].(string)})
		}
	}
	members := []string{}
	if closed {
		owned, err := store.closedGroupMembers(facts.Group)
		if err != nil {
			return count, err
		}
		for _, member := range owned {
			members = append(members, member["id"].(string))
		}
	}

	// Each slot: rank the pieces, link the most likely.
	for item := range items {
		slotID := items[item].SlotID
		if slotID == "" {
			continue
		}
		scores := map[string]float64{}
		secondMates := second.members(second.piece[item])
		for _, piece := range pieceOrder {
			possessionID, known := possessionOf[piece]
			if !known {
				continue
			}
			pieceMembers := pieces[piece]
			score := 0.0
			if first.piece[item] == piece {
				score += 0.5
			}
			score += 0.5 * jaccard(secondMates, pieceMembers)
			for _, sorted := range []sorting{first, second} {
				for _, alternative := range sorted.alternatives[item] {
					if containsInt(pieceMembers, alternative) {
						score += 0.15
						break
					}
				}
			}
			if score == 0 {
				continue
			}
			scores[possessionID] = math.Max(scores[possessionID], score)
			for _, extra := range extraOf[piece] {
				scores[extra] = math.Max(scores[extra], score*0.5)
			}
		}
		for _, member := range members {
			if _, scored := scores[member]; !scored {
				scores[member] = 0.01 // every member of a closed group stays a choice
			}
		}
		candidates := rankCandidates(scores, !closed)
		if len(candidates) == 0 {
			run.leave(slotID, "no piece to link it to")
			continue
		}
		questionID, err := store.linkSlotBestGuess(ctx, slots[slotID], candidates, closed)
		if err != nil {
			run.fail(slotID, err)
			continue
		}
		run.Counts["linked"]++
		run.Actions = append(run.Actions, AuditAction{Action: "linked", Records: []string{slotID, candidates[0].PossessionID}, QuestionID: questionID,
			Reason: fmt.Sprintf("best guess, likelihood %.2f", candidates[0].Likelihood)})
	}
	return count, nil
}

func largestSlotItem(items []clusterItem, members []int, slots map[string]Slot) int {
	best, bestArea := members[0], -1.0
	for _, item := range members {
		if slot, isSlot := slots[items[item].SlotID]; isSlot {
			if area := slot.largestPhoto().Box.area(); area > bestArea {
				best, bestArea = item, area
			}
		}
	}
	return best
}

func jaccard(first, second []int) float64 {
	union := map[int]bool{}
	shared := 0
	for _, value := range first {
		union[value] = true
	}
	for _, value := range second {
		if union[value] {
			shared++
		}
		union[value] = true
	}
	if len(union) == 0 {
		return 0
	}
	return float64(shared) / float64(len(union))
}

// rankCandidates turns scores into candidates whose likelihoods sum to 1
// over all scored pieces, most likely first, ties by id; at most five when
// capped (an open group), every one otherwise (a closed group's members).
func rankCandidates(scores map[string]float64, capped bool) []SlotCandidate {
	total := 0.0
	for _, score := range scores {
		total += score
	}
	candidates := []SlotCandidate{}
	for id, score := range scores {
		candidates = append(candidates, SlotCandidate{PossessionID: id, Likelihood: math.Round(score/total*100) / 100})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Likelihood != candidates[j].Likelihood {
			return candidates[i].Likelihood > candidates[j].Likelihood
		}
		return candidates[i].PossessionID < candidates[j].PossessionID
	})
	if capped && len(candidates) > 5 {
		candidates = candidates[:5]
	}
	return candidates
}
