package possessions

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
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

type clusterItem struct {
	Label        string
	SlotID       string
	PossessionID string
	Status       string // a recorded item's status
	Text         string
	SortKey      string // the name, lowercased: batches put look-alikes together
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
	if err := store.unlinkUnpickedBestGuesses(ctx, actor, &run); err != nil {
		return run, err
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
			if possessionGroup(possession) != group || (!slices.Contains(ownedStatuses, status) && status != "unconfirmed") {
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
				items = append(items, clusterItem{Label: fmt.Sprintf("R%d", index+1), PossessionID: id, Status: possession["status"].(string), Text: describeWearable(possession),
					SortKey: strings.ToLower(possession["name"].(string)), Crop: crop})
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
			items = append(items, clusterItem{Label: fmt.Sprintf("S%d", index+1), SlotID: slot.ID, Text: describeSlot(slot), SortKey: strings.ToLower(slot.Proposed.Name), Crop: crop})
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
	// A group left holding more pieces than the owner's expected high gets
	// one more sorting round, over one picture of each piece.
	for _, facts := range groupFacts {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		if facts.ExpectedHigh == nil {
			continue
		}
		pullIn, err := reviewer.pullGroupIntoRange(ctx, facts, actor, &run)
		if err != nil {
			run.fail(facts.Group, fmt.Errorf("pull it toward its expected range: %w", err))
			continue
		}
		if pullIn == nil {
			continue
		}
		found := false
		for index := range reportGroups {
			if reportGroups[index].Group == facts.Group {
				reportGroups[index].PullIn, found = pullIn, true
			}
		}
		if !found {
			reportGroups = append(reportGroups, GroupCount{Group: facts.Group, PullIn: pullIn})
		}
	}
	sort.Slice(reportGroups, func(i, j int) bool { return reportGroups[i].Group < reportGroups[j].Group })
	// Counts after every group is done, against the owner's range.
	wearables, err = store.liveWearables()
	if err != nil {
		return run, err
	}
	for index := range reportGroups {
		for _, possession := range wearables {
			status := possession["status"].(string)
			if possessionGroup(possession) == reportGroups[index].Group && (slices.Contains(ownedStatuses, status) || status == "unconfirmed") {
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

// sortingBrief is what a sorting call tells a reader about the pictures:
// whole when the call holds every picture being sorted, part when it holds
// some of them.
type sortingBrief struct {
	whole string
	part  string
}

// identificationBrief tells the readers what identification shows them.
func identificationBrief(facts WearableGroup) sortingBrief {
	group := strings.ReplaceAll(facts.Group, ":", " ")
	brief := sortingBrief{
		whole: "These pictures are every " + group + " seen on the owner, and the ones already recorded. " + expectedText(facts),
		part:  "These pictures are part of a larger set of the owner's " + group + ".",
	}
	if facts.Closed {
		closedText := " The owner says every one they own is recorded: every S picture belongs with an R picture of an owned piece."
		brief.whole += closedText
		brief.part += closedText
	}
	return brief
}

// picturesPerSortingCall is how many pictures one sorting call of a group
// holds (see the constants above).
func picturesPerSortingCall(facts WearableGroup) int {
	if facts.ExpectedHigh != nil && *facts.ExpectedHigh <= smallGroupHighestCount {
		return smallGroupPicturesPerCall
	}
	return largeGroupPicturesPerCall
}

// sortItems has one reader sort items into pieces. Items that fit one call
// are sorted in it. More are sorted in batches of similar words first; then
// one picture of each piece found so far (a recorded one where the piece has
// one), every piece of one picture included, is sorted again across the
// batches: in one call when they fit, otherwise in calls that each hold two
// chunks of them, so that every two pieces are seen side by side once.
// Pieces that never shared a batch can so end as one.
func (reviewer *Reviewer) sortItems(ctx context.Context, role string, facts WearableGroup, brief sortingBrief, items []clusterItem, run *AuditRun) (sorting, error) {
	perCall := picturesPerSortingCall(facts)
	if len(items) <= perCall {
		return reviewer.sortInOneCall(ctx, role, brief.whole, items, run)
	}
	// Batches of similar words together, so a batch holds look-alikes.
	order := make([]int, len(items))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool { return items[order[i]].SortKey < items[order[j]].SortKey })
	result := sorting{piece: make([]int, len(items)), alternatives: make([][]int, len(items))}
	representatives := []clusterItem{}
	representativeItem := []int{} // representative index (= piece) → the item it is
	for start := 0; start < len(order); start += perCall {
		batch := order[start:min(start+perCall, len(order))]
		batchItems := []clusterItem{}
		for _, index := range batch {
			batchItems = append(batchItems, items[index])
		}
		sorted, err := reviewer.sortInOneCall(ctx, role, brief.part, batchItems, run)
		if err != nil {
			return sorting{}, err
		}
		seen := map[int]int{} // batch piece → piece across batches
		for position, index := range batch {
			piece, known := seen[sorted.piece[position]]
			if !known {
				piece = len(representatives)
				seen[sorted.piece[position]] = piece
				representatives = append(representatives, items[index])
				representativeItem = append(representativeItem, index)
			} else if items[index].PossessionID != "" && representatives[piece].PossessionID == "" {
				representatives[piece] = items[index] // a recorded piece speaks for its batch piece
				representativeItem[piece] = index
			}
			result.piece[index] = piece
			for _, alternative := range sorted.alternatives[position] {
				result.alternatives[index] = append(result.alternatives[index], batch[alternative])
			}
		}
	}
	var across sorting
	var err error
	if len(representatives) <= perCall {
		across, err = reviewer.sortInOneCall(ctx, role, brief.whole, representatives, run)
	} else {
		across, err = reviewer.sortEveryPairOfChunks(ctx, role, brief, representatives, perCall, run)
	}
	if err != nil {
		return sorting{}, err
	}
	for index := range result.piece {
		result.piece[index] = across.piece[result.piece[index]]
	}
	for representative, alternatives := range across.alternatives {
		item := representativeItem[representative]
		for _, alternative := range alternatives {
			result.alternatives[item] = append(result.alternatives[item], representativeItem[alternative])
		}
	}
	return result, nil
}

// sortEveryPairOfChunks sorts more items than one call holds: they are cut
// into chunks of half a call, and every two chunks are sorted together, so
// every two items are seen side by side in at least one call. Two items are
// one piece when every call that showed both put them together; pieces
// join only when no call that showed a member of each kept those two apart.
func (reviewer *Reviewer) sortEveryPairOfChunks(ctx context.Context, role string, brief sortingBrief, items []clusterItem, perCall int, run *AuditRun) (sorting, error) {
	chunkSize := max(perCall/2, 1)
	chunks := [][]int{}
	for start := 0; start < len(items); start += chunkSize {
		chunk := []int{}
		for index := start; index < min(start+chunkSize, len(items)); index++ {
			chunk = append(chunk, index)
		}
		chunks = append(chunks, chunk)
	}
	type pair struct{ first, second int }
	pairOf := func(first, second int) pair {
		if first > second {
			first, second = second, first
		}
		return pair{first, second}
	}
	together, apart := map[pair]bool{}, map[pair]bool{}
	alternatives := make([][]int, len(items))
	for first := 0; first < len(chunks); first++ {
		for second := first + 1; second < len(chunks); second++ {
			shown := append(append([]int{}, chunks[first]...), chunks[second]...)
			callItems := []clusterItem{}
			for _, index := range shown {
				callItems = append(callItems, items[index])
			}
			sorted, err := reviewer.sortInOneCall(ctx, role, brief.part, callItems, run)
			if err != nil {
				return sorting{}, err
			}
			for position, index := range shown {
				for other := position + 1; other < len(shown); other++ {
					if sorted.piece[position] == sorted.piece[other] {
						together[pairOf(index, shown[other])] = true
					} else {
						apart[pairOf(index, shown[other])] = true
					}
				}
				for _, alternative := range sorted.alternatives[position] {
					if !slices.Contains(alternatives[index], shown[alternative]) {
						alternatives[index] = append(alternatives[index], shown[alternative])
					}
				}
			}
		}
	}
	pairs := []pair{}
	for joined := range together {
		if !apart[joined] {
			pairs = append(pairs, joined)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].first != pairs[j].first {
			return pairs[i].first < pairs[j].first
		}
		return pairs[i].second < pairs[j].second
	})
	result := sorting{piece: make([]int, len(items)), alternatives: alternatives}
	membersOf := map[int][]int{}
	for index := range items {
		result.piece[index] = index
		membersOf[index] = []int{index}
	}
	for _, joined := range pairs {
		kept, folded := result.piece[joined.first], result.piece[joined.second]
		if kept == folded {
			continue
		}
		keptApart := false
		for _, member := range membersOf[kept] {
			for _, other := range membersOf[folded] {
				keptApart = keptApart || apart[pairOf(member, other)]
			}
		}
		if keptApart {
			continue
		}
		for _, other := range membersOf[folded] {
			result.piece[other] = kept
		}
		membersOf[kept] = append(membersOf[kept], membersOf[folded]...)
		delete(membersOf, folded)
	}
	return result, nil
}

func (reviewer *Reviewer) sortInOneCall(ctx context.Context, role string, scope string, items []clusterItem, run *AuditRun) (sorting, error) {
	images := []msg.OneShotImage{}
	listing := []string{}
	labelIndex := map[string]int{}
	for index, item := range items {
		images = append(images, jpegImage(item.Crop))
		labelIndex[item.Label] = index
		listing = append(listing, fmt.Sprintf("%s (picture %d): %s", item.Label, index+1, item.Text))
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
	first, err := reviewer.sortItems(ctx, reviewer.FirstReaderRole, facts, identificationBrief(facts), items, run)
	if err != nil {
		return count, err
	}
	second, err := reviewer.sortItems(ctx, reviewer.SecondReaderRole, facts, identificationBrief(facts), items, run)
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
		if closed && !slices.Contains(ownedStatuses, views[keeper]["status"].(string)) {
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
		picked := map[string]bool{} // pieces a reader put the slot with, not only named as a maybe
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
					if slices.Contains(pieceMembers, alternative) {
						score += 0.15
						break
					}
				}
			}
			if score == 0 {
				continue
			}
			readerPicked := first.piece[item] == piece || jaccard(secondMates, pieceMembers) > 0
			scores[possessionID] = math.Max(scores[possessionID], score)
			picked[possessionID] = picked[possessionID] || readerPicked
			for _, extra := range extraOf[piece] {
				scores[extra] = math.Max(scores[extra], score*0.5)
				picked[extra] = picked[extra] || readerPicked
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
		if !picked[candidates[0].PossessionID] {
			// No reader put the slot with any recorded piece: in a closed
			// group every member is only a choice. Leave it unlinked, its
			// candidates and question kept.
			questionID, err := store.linkSlotBestGuess(ctx, slots[slotID], candidates, closed, false)
			if err != nil {
				run.fail(slotID, err)
				continue
			}
			run.Counts["unpicked"]++
			run.Actions = append(run.Actions, AuditAction{Action: "unpicked", Records: []string{slotID}, QuestionID: questionID, Reason: "no reader put it with a recorded piece"})
			continue
		}
		questionID, err := store.linkSlotBestGuess(ctx, slots[slotID], candidates, closed, true)
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

// groupMembersToCount answers the live owned and unconfirmed possessions of
// a group: the pieces its count against the owner's range is made of.
func (s *Store) groupMembersToCount(group string) ([]map[string]any, error) {
	wearables, err := s.liveWearables()
	if err != nil {
		return nil, err
	}
	members := []map[string]any{}
	for _, possession := range wearables {
		status := possession["status"].(string)
		if possessionGroup(possession) == group && (slices.Contains(ownedStatuses, status) || status == "unconfirmed") {
			members = append(members, possession)
		}
	}
	return members, nil
}

// pullInBrief tells the readers that the pieces they see were split too
// finely.
func pullInBrief(facts WearableGroup, count int) sortingBrief {
	group := strings.ReplaceAll(facts.Group, ":", " ")
	overSplit := fmt.Sprintf(" %s Sorting so far made %d separate pieces, more than that, so it split pieces that are one: put together the pictures that show one physical piece seen from another side, in other light or worn another way. Keep apart only pieces you can see differ.", expectedText(facts), count)
	return sortingBrief{
		whole: fmt.Sprintf("These pictures are one of each of the %d %s pieces recorded so far; every one is an R picture.", count, group) + overSplit,
		part:  fmt.Sprintf("These pictures are some of the %d %s pieces recorded so far; every one is an R picture.", count, group) + overSplit,
	}
}

// pullGroupIntoRange runs one more sorting round on a group that holds more
// pieces than the owner's expected high: both readers see one picture of
// each piece (the largest slot crop linked to it, else its cover), told the
// range and that the sorting split too finely, and every set of pieces both
// put together is merged, undoably, into the one keeperOf keeps (owned
// before unconfirmed). Since both must agree, two owned pieces merge only
// on agreement. It answers nil when the group is within its range.
func (reviewer *Reviewer) pullGroupIntoRange(ctx context.Context, facts WearableGroup, actor string, run *AuditRun) (*GroupPullIn, error) {
	store := reviewer.Store
	members, err := store.groupMembersToCount(facts.Group)
	if err != nil {
		return nil, err
	}
	if facts.ExpectedHigh == nil || len(members) <= *facts.ExpectedHigh {
		return nil, nil
	}
	pullIn := &GroupPullIn{Before: len(members)}
	requests := []cropRequest{}
	for _, possession := range members {
		id := possession["id"].(string)
		slotIDs, err := stringColumn(store.db, `SELECT id FROM outfit_slots WHERE possession_id = ? ORDER BY seq`, id)
		if err != nil {
			return nil, err
		}
		best, bestArea := cropRequest{}, -1.0
		for _, slotID := range slotIDs {
			slot, err := slotByID(store.db, slotID)
			if err != nil {
				return nil, err
			}
			if len(slot.Photos) == 0 {
				continue
			}
			if photo := slot.largestPhoto(); photo.Box.area() > bestArea {
				best, bestArea = cropRequest{key: id, photoID: photo.PhotoID, box: photo.Box, small: smallPieceGroup(facts.Group)}, photo.Box.area()
			}
		}
		if bestArea < 0 {
			for _, link := range possession["photos"].([]PhotoLink) {
				if !link.IsCover {
					continue
				}
				box := Box{0, 0, 1, 1}
				if link.BoxLeft != nil {
					box = Box{*link.BoxLeft, *link.BoxTop, *link.BoxRight, *link.BoxBottom}
				}
				best, bestArea = cropRequest{key: id, photoID: link.PhotoID, box: box, small: smallPieceGroup(facts.Group)}, 0
			}
		}
		if bestArea < 0 {
			run.leave(id, "no picture of it to show when pulling "+facts.Group+" toward its expected range")
			continue
		}
		requests = append(requests, best)
	}
	crops, cropFailures := reviewer.cutCrops(ctx, requests)
	items := []clusterItem{}
	for _, possession := range members {
		id := possession["id"].(string)
		crop, cut := crops[id]
		if !cut {
			if failure, failed := cropFailures[id]; failed {
				run.fail(id, fmt.Errorf("cut its picture: %w", failure))
			}
			continue
		}
		items = append(items, clusterItem{Label: fmt.Sprintf("R%d", len(items)+1), PossessionID: id, Status: possession["status"].(string),
			Text: describeWearable(possession), SortKey: strings.ToLower(possession["name"].(string)), Crop: crop})
	}
	pullIn.Shown = len(items)
	if len(items) < 2 {
		pullIn.After = len(members)
		return pullIn, nil
	}
	brief := pullInBrief(facts, len(members))
	first, err := reviewer.sortItems(ctx, reviewer.FirstReaderRole, facts, brief, items, run)
	if err != nil {
		return nil, err
	}
	second, err := reviewer.sortItems(ctx, reviewer.SecondReaderRole, facts, brief, items, run)
	if err != nil {
		return nil, err
	}
	// Pieces both readers put together: the same piece in both sortings.
	type bothPieces struct{ first, second int }
	together := map[bothPieces][]int{}
	order := []bothPieces{}
	for item := range items {
		key := bothPieces{first.piece[item], second.piece[item]}
		if _, seen := together[key]; !seen {
			order = append(order, key)
		}
		together[key] = append(together[key], item)
	}
	for _, key := range order {
		set := together[key]
		if len(set) < 2 {
			continue
		}
		views := []map[string]any{}
		for _, item := range set {
			view, err := store.GetPossession(items[item].PossessionID, false)
			if err != nil {
				return nil, err
			}
			views = append(views, view)
		}
		keeper := views[0]
		for _, view := range views[1:] {
			keeper, _ = keeperOf(keeper, view)
		}
		keptID := keeper["id"].(string)
		for _, view := range views {
			duplicateID := view["id"].(string)
			if duplicateID == keptID {
				continue
			}
			merged, err := store.MergePossession(ctx, duplicateID, keptID, actor, fmt.Sprintf("identification of %s: the group held %d pieces, over the owner's %s, and both readers saw one piece", facts.Group, len(members), strings.TrimSuffix(expectedText(facts), ".")))
			if err != nil {
				run.fail(duplicateID, fmt.Errorf("merge into %s: %w", keptID, err))
				continue
			}
			pullIn.Merged++
			run.Counts["merged_toward_range"]++
			run.Actions = append(run.Actions, AuditAction{Action: "merged", Records: []string{duplicateID, keptID}, MergeID: merged["merge_id"].(string),
				Reason: "pulled toward the owner's range; both readers agreed"})
		}
	}
	after, err := store.groupMembersToCount(facts.Group)
	if err != nil {
		return nil, err
	}
	pullIn.After = len(after)
	return pullIn, nil
}

// unlinkUnpickedBestGuesses unlinks each slot an earlier identification
// linked though no reader picked a piece for it: in a closed group every
// member was a choice at the same score, and the first by id was linked.
// Its candidates, all alike, say so. The slot keeps them and its question,
// decided_by no_pick.
func (s *Store) unlinkUnpickedBestGuesses(ctx context.Context, actor string, run *AuditRun) error {
	slotIDs, err := stringColumn(s.db, `SELECT id FROM outfit_slots WHERE decided_by = 'best_guess' ORDER BY seq`)
	if err != nil {
		return err
	}
	for _, slotID := range slotIDs {
		slot, err := slotByID(s.db, slotID)
		if err != nil {
			return err
		}
		if len(slot.Candidates) < 2 {
			continue
		}
		alike := true
		for _, candidate := range slot.Candidates[1:] {
			alike = alike && candidate.Likelihood == slot.Candidates[0].Likelihood
		}
		if !alike {
			continue
		}
		if err := s.unlinkSlot(ctx, slot, actor); err != nil {
			return err
		}
		run.Counts["unlinked_unpicked"]++
		run.Actions = append(run.Actions, AuditAction{Action: "unlinked", Records: []string{slotID, *slot.PossessionID}, Reason: "an earlier best guess no reader picked: every candidate had the same likelihood"})
	}
	return nil
}
