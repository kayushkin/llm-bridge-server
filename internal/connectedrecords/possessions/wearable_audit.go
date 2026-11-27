package possessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kayushkin/llm-bridge/msg"
)

// The wearable audit: both readers look at pictures of the wearables and
// outfits already recorded and settle three things — whether each photo
// linked to a piece shows that piece (CheckWearableLinks), which recorded
// pieces are one piece recorded twice (CheckWearableDuplicates), and which
// outfits are one outfit recorded twice (ConsolidateOutfits, in
// outfit_consolidation.go). The operator chose on 2026-11-07: when both
// readers agree, act at once, every merge undoable; when they disagree, ask.

// AuditRun says what one audit pass did.
type AuditRun struct {
	Looked   int                 `json:"looked"`
	Counts   map[string]int      `json:"counts"`
	Actions  []AuditAction       `json:"actions"`
	Left     []map[string]string `json:"left"`     // what was not looked at, and why
	Failures []map[string]string `json:"failures"` // what went wrong, with the error in full
	CostUSD  float64             `json:"cost_usd"`
	Groups   []GroupCount        `json:"groups,omitempty"` // identification: each group's count against the owner's range
}

// AuditAction is one change or question an audit made.
type AuditAction struct {
	Action     string   `json:"action"`  // unlinked, asked, merged, combined
	Records    []string `json:"records"` // the ids it is about
	QuestionID string   `json:"question_id,omitempty"`
	MergeID    string   `json:"merge_id,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func newAuditRun() AuditRun {
	return AuditRun{Counts: map[string]int{}, Actions: []AuditAction{}, Left: []map[string]string{}, Failures: []map[string]string{}}
}

func (run *AuditRun) fail(about string, err error) {
	run.Failures = append(run.Failures, map[string]string{"about": about, "error": err.Error()})
}

func (run *AuditRun) leave(about, why string) {
	run.Left = append(run.Left, map[string]string{"about": about, "why": why})
}

// pictureCache reads each photo-store thumbnail, and each photo as the
// readers are shown it, once per pass.
type pictureCache struct {
	photos       PhotoSource
	thumbnails   map[string][]byte
	reviewImages map[string][]byte
}

func newPictureCache(photos PhotoSource) *pictureCache {
	return &pictureCache{photos: photos, thumbnails: map[string][]byte{}, reviewImages: map[string][]byte{}}
}

// reviewImage is the photo as an outfit reading showed it to the reader
// (the original scaled, or the thumbnail without one), which a sighting's
// box was drawn on.
func (cache *pictureCache) reviewImage(ctx context.Context, photoID string) ([]byte, error) {
	if picture, cached := cache.reviewImages[photoID]; cached {
		return picture, nil
	}
	picture, _, err := cache.photos.ReviewImage(ctx, photoID)
	if err != nil {
		return nil, err
	}
	cache.reviewImages[photoID] = picture
	return picture, nil
}

func (cache *pictureCache) thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	if picture, cached := cache.thumbnails[photoID]; cached {
		return picture, nil
	}
	picture, err := cache.photos.Thumbnail(ctx, photoID)
	if err != nil {
		return nil, err
	}
	cache.thumbnails[photoID] = picture
	return picture, nil
}

// linkCrop cuts a linked photo by its stored box; the whole thumbnail when
// it has none.
func (cache *pictureCache) linkCrop(ctx context.Context, link PhotoLink) ([]byte, error) {
	picture, err := cache.thumbnail(ctx, link.PhotoID)
	if err != nil {
		return nil, err
	}
	if link.BoxLeft == nil {
		return picture, nil
	}
	return cropJPEG(picture, Box{*link.BoxLeft, *link.BoxTop, *link.BoxRight, *link.BoxBottom})
}

// coverCrop is a possession cut from its cover photo; ErrNotFound when it
// has no photo.
func (cache *pictureCache) coverCrop(ctx context.Context, q querier, possessionID string) ([]byte, error) {
	photos, err := possessionPhotos(q, possessionID)
	if err != nil {
		return nil, err
	}
	for _, link := range photos {
		if link.IsCover {
			return cache.linkCrop(ctx, link)
		}
	}
	return nil, fmt.Errorf("%w: %s has no photo", ErrNotFound, possessionID)
}

func jpegImage(picture []byte) msg.OneShotImage {
	return msg.OneShotImage{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(picture)}
}

// askBothReaders asks the two readers the same question with the same
// pictures, at once.
func (reviewer *Reviewer) askBothReaders(ctx context.Context, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) ([2]ModelAnswer, error) {
	return reviewer.askBothPrompts(ctx, systemPrompt, [2]string{prompt, prompt}, schema, images, maxTokens)
}

func (reviewer *Reviewer) askBothPrompts(ctx context.Context, systemPrompt string, prompts [2]string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) ([2]ModelAnswer, error) {
	roles := [2]string{reviewer.FirstReaderRole, reviewer.SecondReaderRole}
	results := [2]readerResult{}
	var group sync.WaitGroup
	for index := range roles {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			answer, err := reviewer.Models.Ask(ctx, roles[index], systemPrompt, prompts[index], schema, images, maxTokens)
			results[index] = readerResult{answer, err}
		}(index)
	}
	group.Wait()
	answers := [2]ModelAnswer{}
	for index, result := range results {
		if result.err != nil {
			return answers, fmt.Errorf("reader %s: %w", roles[index], result.err)
		}
		answers[index] = result.answer
	}
	return answers, nil
}

const auditSystemPrompt = `You help a person keep an exact inventory of the clothes and accessories they own, from photos. Two readers, made by different companies, answer each question on their own; where they agree the inventory changes at once, so say "same" only when the pictures show it. Judge the physical piece, not the kind of thing: color, cut, pattern, fabric, logos, prints, hardware, wear marks. Light, angle, folds and how it is worn differ between photos; a black t-shirt is not every black t-shirt.`

// wearableGroup is the group a wearable is compared and counted within, as
// the owner thinks of them (2026-11-07): a garment's category (shirts are
// tops, pants bottoms, jackets outerwear), except the garment types in
// GroupedGarmentTypes, which are groups of their own (scarves); a jewelry
// type, with pendants and chains counted as necklaces; a bag type; watches
// and eyewear one group each. Socks are in no group. "" for a possession
// that is not in a group.
func wearableGroup(kind string, fields func(name string) string) string {
	switch kind {
	case "garment":
		garmentType := fields("garment_type")
		if contains(ungroupedGarmentTypes, garmentType) {
			return ""
		}
		if contains(GroupedGarmentTypes, garmentType) {
			return "garment:" + garmentType
		}
		return "garment:" + fields("category")
	case "jewelry":
		jewelryType := fields("jewelry_type")
		if contains(NecklaceJewelryTypes, jewelryType) {
			jewelryType = "necklace"
		}
		return "jewelry:" + jewelryType
	case "bag_or_case":
		return "bag_or_case:" + fields("bag_type")
	case "watch", "eyewear":
		return kind
	}
	return ""
}

func possessionGroup(possession map[string]any) string {
	kind := possession["kind"].(string)
	if !contains(WearableKinds, kind) {
		return ""
	}
	record := possession[kind].(map[string]any)
	return wearableGroup(kind, func(name string) string { value, _ := record[name].(string); return value })
}

func proposalGroup(proposal Proposal) string {
	return wearableGroup(proposal.Kind, func(name string) string {
		var value string
		_ = json.Unmarshal(proposal.KindFields[name], &value)
		return value
	})
}

// liveWearables answers every live possession of a wearable kind, oldest
// first.
func (s *Store) liveWearables() ([]map[string]any, error) {
	all := []map[string]any{}
	for _, kind := range WearableKinds {
		possessions, err := s.ListPossessions(PossessionFilter{Kind: kind, Limit: 1000})
		if err != nil {
			return nil, err
		}
		if len(possessions) == 1000 {
			return nil, fmt.Errorf("there are 1000 or more %s possessions; the audit reads at most 1000 of a kind", kind)
		}
		all = append(all, possessions...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i]["id"].(string) < all[j]["id"].(string) })
	return all, nil
}

func describeWearable(possession map[string]any) string {
	kind := possession["kind"].(string)
	summary := kind
	if record, isRecord := possession[kind].(map[string]any); isRecord {
		summary += " " + summarizeKindRecord(record)
	}
	return fmt.Sprintf("%s: %s | %s | brand %q | %s", possession["id"], possession["name"], summary, possession["brand"], truncateWords(possession["description"].(string), 25))
}

func photoCount(possession map[string]any) int { return len(possession["photos"].([]PhotoLink)) }

// keeperOf picks which of two possessions a merge keeps: one the owner
// confirmed owning over one only seen in a photo (unconfirmed), then the one
// with more photos, then the older.
func keeperOf(first, second map[string]any) (kept, duplicate map[string]any) {
	if confirmed := func(possession map[string]any) bool { return possession["status"] != "unconfirmed" }; confirmed(first) != confirmed(second) {
		if confirmed(first) {
			return first, second
		}
		return second, first
	}
	if photoCount(first) != photoCount(second) {
		if photoCount(first) > photoCount(second) {
			return first, second
		}
		return second, first
	}
	if first["created_at"].(int64) != second["created_at"].(int64) {
		if first["created_at"].(int64) < second["created_at"].(int64) {
			return first, second
		}
		return second, first
	}
	if first["id"].(string) < second["id"].(string) {
		return first, second
	}
	return second, first
}

// ---------------------------------------------------------------------------
// Link check.
// ---------------------------------------------------------------------------

var linkCheckSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["crops"],"properties":{"crops":{"type":"array","items":{
	"type":"object","additionalProperties":false,"required":["crop","same_piece","reason","other_possession_ids"],"properties":{
	"crop":{"type":"string","description":"The crop's label: C1, C2…"},
	"same_piece":{"type":"boolean","description":"true when the crop shows the same physical piece as the reference"},
	"reason":{"type":"string","description":"A few words: what tells them apart or shows they are one"},
	"other_possession_ids":{"type":"array","items":{"type":"string"},"description":"When it is not the same piece: ids from the list of other recorded pieces it could be; otherwise empty"}}}}}}`)

// crops per call besides the reference picture
const linkCheckCropsPerCall = 19

type linkVerdict struct {
	SamePiece bool     `json:"same_piece"`
	Reason    string   `json:"reason"`
	Others    []string `json:"other_possession_ids"`
}

// CheckWearableLinks asks both readers, for each live wearable with two or
// more photos, whether each linked photo shows the same physical piece as
// its cover. Both say no: the link is undone (photo, sighting, the piece's
// place in that reading's outfit) and a decision question asks what the
// sighting is. One says no: a decision question asks whether the link is
// right, keeping it until answered. A link a person made (no sighting) and
// a sighting already asked about are left alone.
func (reviewer *Reviewer) CheckWearableLinks(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	wearables, err := store.liveWearables()
	if err != nil {
		return run, err
	}
	groups := map[string][]map[string]any{}
	for _, possession := range wearables {
		groups[possessionGroup(possession)] = append(groups[possessionGroup(possession)], possession)
	}
	cache := newPictureCache(reviewer.Photos)
	for _, possession := range wearables {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		possessionID := possession["id"].(string)
		links := possession["photos"].([]PhotoLink)
		if len(links) < 2 {
			run.Counts["one_photo_or_none"]++
			continue
		}
		var cover PhotoLink
		toCheck := []PhotoLink{}
		for _, link := range links {
			switch {
			case link.IsCover:
				cover = link
			case link.SightingID == nil:
				run.leave(possessionID+" "+link.PhotoID, "a person linked it")
			default:
				var asked int
				if err := store.db.QueryRow(`SELECT COUNT(*) FROM owner_questions WHERE sighting_id = ? AND strpos(choices, '"keep_link"') > 0`, *link.SightingID).Scan(&asked); err != nil {
					return run, err
				}
				if asked > 0 {
					run.leave(*link.SightingID, "already asked about")
					continue
				}
				toCheck = append(toCheck, link)
			}
		}
		if len(toCheck) == 0 {
			continue
		}
		reference, err := cache.linkCrop(ctx, cover)
		if err != nil {
			run.fail(possessionID, fmt.Errorf("cut its cover %s: %w", cover.PhotoID, err))
			continue
		}
		others := []string{}
		for _, other := range groups[possessionGroup(possession)] {
			if other["id"] != possessionID && len(others) < 40 {
				others = append(others, "- "+describeWearable(other))
			}
		}
		for start := 0; start < len(toCheck); start += linkCheckCropsPerCall {
			batch := toCheck[start:min(start+linkCheckCropsPerCall, len(toCheck))]
			images := []msg.OneShotImage{jpegImage(reference)}
			labels := map[string]PhotoLink{}
			listing := []string{}
			for _, link := range batch {
				crop, err := cache.linkCrop(ctx, link)
				if err != nil {
					run.fail(*link.SightingID, fmt.Errorf("cut %s: %w", link.PhotoID, err))
					continue
				}
				images = append(images, jpegImage(crop))
				label := fmt.Sprintf("C%d", len(images)-1)
				labels[label] = link
				listing = append(listing, fmt.Sprintf("%s is picture %d", label, len(images)))
			}
			if len(labels) == 0 {
				continue
			}
			prompt := fmt.Sprintf(`Picture 1 is the reference: %s

The other pictures are crops from other photos that the inventory says show this same piece: %s.

For each crop, say whether it shows this same physical piece. When it does not, put in other_possession_ids any of these other recorded pieces of the same kind it could be (or leave it empty):
%s`, describeWearable(possession), strings.Join(listing, ", "), strings.Join(others, "\n"))
			answers, err := reviewer.askBothReaders(ctx, auditSystemPrompt, prompt, linkCheckSchema, images, 8000)
			if err != nil {
				run.fail(possessionID, err)
				continue
			}
			verdicts := [2]map[string]linkVerdict{{}, {}}
			for side, answer := range answers {
				run.CostUSD += answer.CostUSD
				var parsed struct {
					Crops []struct {
						Crop string `json:"crop"`
						linkVerdict
					} `json:"crops"`
				}
				if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
					run.fail(possessionID, fmt.Errorf("%s's link check is not one: %w", answer.Model, err))
					continue
				}
				for _, crop := range parsed.Crops {
					verdicts[side][strings.TrimSpace(crop.Crop)] = crop.linkVerdict
				}
			}
			models := [2]string{answers[0].Model, answers[1].Model}
			for label, link := range labels {
				run.Looked++
				first, answeredFirst := verdicts[0][label]
				second, answeredSecond := verdicts[1][label]
				if !answeredFirst || !answeredSecond {
					run.fail(*link.SightingID, fmt.Errorf("a reader did not answer for %s (%s)", label, link.PhotoID))
					continue
				}
				if first.SamePiece && second.SamePiece {
					run.Counts["confirmed"]++
					continue
				}
				closed, err := groupClosed(store.db, possessionGroup(possession))
				if err != nil {
					return run, err
				}
				action, err := store.settleLinkCheck(ctx, possession, *link.SightingID, models, [2]linkVerdict{first, second}, closed, actor)
				if err != nil {
					run.fail(*link.SightingID, err)
					continue
				}
				run.Counts[action.Action]++
				run.Actions = append(run.Actions, action)
			}
		}
	}
	return run, nil
}

// settleLinkCheck acts on a link at least one reader says is wrong. In a
// closed group the question offers every other owned member and never a
// new piece.
func (s *Store) settleLinkCheck(ctx context.Context, possession map[string]any, sightingID string, models [2]string, verdicts [2]linkVerdict, closed bool, actor string) (AuditAction, error) {
	possessionID := possession["id"].(string)
	name := possession["name"].(string)
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuditAction{}, err
	}
	defer transaction.Rollback()
	sighting, err := sightingByID(transaction, sightingID)
	if err != nil {
		return AuditAction{}, err
	}
	if sighting.Status != "accepted" || sighting.PossessionID == nil || *sighting.PossessionID != possessionID {
		return AuditAction{}, fmt.Errorf("%w: %s is no longer recorded as %s", ErrConflict, sightingID, possessionID)
	}
	suggested := []string{}
	for _, verdict := range verdicts {
		if verdict.SamePiece {
			continue
		}
		for _, other := range verdict.Others {
			other = strings.TrimSpace(other)
			if other == possessionID || contains(suggested, other) {
				continue
			}
			view, err := possessionView(transaction, other, false)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return AuditAction{}, err
			}
			if contains(WearableKinds, view["kind"].(string)) {
				suggested = append(suggested, other)
			}
		}
	}
	if closed {
		members, err := s.closedGroupMembers(possessionGroup(possession), possessionID)
		if err != nil {
			return AuditAction{}, err
		}
		suggested = []string{}
		for _, member := range members {
			suggested = append(suggested, member["id"].(string))
		}
	}
	fresh := sighting.Proposed
	fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
	if fresh.Name == "" {
		fresh.Name = sighting.Label
	}
	names := map[string]string{}
	for _, other := range suggested {
		view, err := possessionView(transaction, other, false)
		if err != nil {
			return AuditAction{}, err
		}
		names[other] = view["name"].(string)
	}
	now := s.unixNow()
	said := func(side int) string {
		verdict := "no"
		if verdicts[side].SamePiece {
			verdict = "yes"
		}
		return fmt.Sprintf("%s says %s (%s)", models[side], verdict, verdicts[side].Reason)
	}
	action := AuditAction{Records: []string{possessionID, sightingID}, Reason: said(0) + "; " + said(1)}
	var question string
	var choices []Choice
	if !verdicts[0].SamePiece && !verdicts[1].SamePiece {
		if err := s.unlinkSighting(transaction, sighting, possessionID, actor, now); err != nil {
			return AuditAction{}, err
		}
		action.Action = "unlinked"
		question = fmt.Sprintf("Both readers say the %s in this photo is not %s (%s): %s. It is no longer linked. Which piece is it?", sighting.Label, name, possessionID, action.Reason)
		accept := fresh
		choices = []Choice{}
		if !closed {
			choices = append(choices, Choice{Label: "A piece not recorded yet: " + fresh.summary(), Resolution: Resolution{Action: "accept", Proposal: &accept}})
		}
		for _, other := range suggested {
			choices = append(choices, Choice{Label: fmt.Sprintf("It is %s (%s)", other, names[other]), Resolution: Resolution{Action: "same_as", PossessionID: other}})
		}
		choices = append(choices, rejectChoice)
	} else {
		action.Action = "asked"
		question = fmt.Sprintf("Is the %s in this photo %s (%s)? %s.", sighting.Label, name, possessionID, action.Reason)
		accept := fresh
		choices = []Choice{{Label: fmt.Sprintf("Yes, it is %s", name), Resolution: Resolution{Action: "keep_link", PossessionID: possessionID}}}
		if !closed {
			choices = append(choices, Choice{Label: "A different piece, not recorded yet: " + fresh.summary(), Resolution: Resolution{Action: "accept", Proposal: &accept, UnlinkPossessionID: possessionID}})
		}
		for _, other := range suggested {
			choices = append(choices, Choice{Label: fmt.Sprintf("A different piece: %s (%s)", other, names[other]), Resolution: Resolution{Action: "same_as", PossessionID: other, UnlinkPossessionID: possessionID}})
		}
		choices = append(choices, Choice{Label: rejectChoice.Label, Resolution: Resolution{Action: "reject", UnlinkPossessionID: possessionID}})
	}
	if action.QuestionID, err = askAuditQuestion(transaction, "link_check", sightingID, question, choices, now); err != nil {
		return AuditAction{}, err
	}
	return action, transaction.Commit()
}

// ---------------------------------------------------------------------------
// Duplicate check.
// ---------------------------------------------------------------------------

var duplicateClustersSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["clusters"],"properties":{"clusters":{"type":"array","items":{
	"type":"object","additionalProperties":false,"required":["members","reason"],"properties":{
	"members":{"type":"array","items":{"type":"string"},"description":"Labels (I1, I2…) of pictures that show one and the same physical piece; two or more"},
	"reason":{"type":"string","description":"A few words: what shows they are one piece"}}}}}}`)

// A group larger than this many pictures is split into chunks of half of
// it, and every pair of chunks is shown together in one call, so every two
// members are seen side by side at least once and no call holds more than
// duplicateCheckPicturesPerCall pictures. Calls grow with the square of the
// group's size: 31 members make 4 chunks and 6 calls a reader.
const duplicateCheckPicturesPerCall = 20

// duplicateCheckCalls splits members into the calls that cover every pair.
func duplicateCheckCalls(memberCount int) [][]int {
	all := make([]int, memberCount)
	for index := range all {
		all[index] = index
	}
	if memberCount <= duplicateCheckPicturesPerCall {
		return [][]int{all}
	}
	chunkSize := duplicateCheckPicturesPerCall / 2
	chunks := [][]int{}
	for start := 0; start < memberCount; start += chunkSize {
		chunks = append(chunks, all[start:min(start+chunkSize, memberCount)])
	}
	calls := [][]int{}
	for first := 0; first < len(chunks); first++ {
		for second := first + 1; second < len(chunks); second++ {
			calls = append(calls, append(append([]int{}, chunks[first]...), chunks[second]...))
		}
	}
	return calls
}

type memberPair [2]int

func orderedPair(first, second int) memberPair {
	if first > second {
		first, second = second, first
	}
	return memberPair{first, second}
}

// CheckWearableDuplicates asks both readers, group by group (kind and
// sub-type), which recorded wearables are one physical piece. A pair both
// readers put together in every call that showed them both is merged
// (undoably) into the one with more photos, then the older. A pair one
// reader put together and the other did not is a decision question. A pair
// already asked about is left to the owner's answer.
func (reviewer *Reviewer) CheckWearableDuplicates(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := newAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	wearables, err := store.liveWearables()
	if err != nil {
		return run, err
	}
	groups := map[string][]map[string]any{}
	groupNames := []string{}
	for _, possession := range wearables {
		if possession["cover_photo_id"] == nil {
			run.leave(possession["id"].(string), "no picture to compare")
			continue
		}
		group := possessionGroup(possession)
		if group == "" {
			continue
		}
		if _, seen := groups[group]; !seen {
			groupNames = append(groupNames, group)
		}
		groups[group] = append(groups[group], possession)
	}
	sort.Strings(groupNames)
	cache := newPictureCache(reviewer.Photos)
	for _, group := range groupNames {
		closed, err := groupClosed(store.db, group)
		if err != nil {
			return run, err
		}
		if closed {
			// Every unconfirmed member of a closed group is one of its owned
			// members; settleUnconfirmedMembers (after the clustering) finds
			// which. Only the rest are clustered.
			owned := []map[string]any{}
			for _, member := range groups[group] {
				if member["status"] != "unconfirmed" {
					owned = append(owned, member)
				}
			}
			groups[group] = owned
		}
	}
	for _, group := range groupNames {
		members := groups[group]
		if len(members) < 2 {
			continue
		}
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		pictures := make([][]byte, len(members))
		usable := []map[string]any{}
		for _, member := range members {
			picture, err := cache.coverCrop(ctx, store.db, member["id"].(string))
			if err != nil {
				run.fail(member["id"].(string), fmt.Errorf("cut its cover: %w", err))
				continue
			}
			pictures[len(usable)] = picture
			usable = append(usable, member)
		}
		members = usable
		run.Looked += len(members)
		seenTogether := map[memberPair]int{}
		clustered := [2]map[memberPair]int{{}, {}}
		reasons := [2]map[memberPair]string{{}, {}}
		models := [2]string{}
		failed := false
		for _, call := range duplicateCheckCalls(len(members)) {
			images := []msg.OneShotImage{}
			listing := []string{}
			labelIndex := map[string]int{}
			for position, memberIndex := range call {
				images = append(images, jpegImage(pictures[memberIndex]))
				label := fmt.Sprintf("I%d", position+1)
				labelIndex[label] = memberIndex
				listing = append(listing, fmt.Sprintf("%s (picture %d) — %s", label, position+1, describeWearable(members[memberIndex])))
			}
			prompt := fmt.Sprintf(`These are pictures of %d pieces recorded in the inventory, all %s. Some may be one physical piece recorded twice from different photos.

%s

List each set of labels that show one and the same physical piece. Leave out every piece that is only itself.`, len(call), strings.ReplaceAll(group, ":", " "), strings.Join(listing, "\n"))
			answers, err := reviewer.askBothReaders(ctx, auditSystemPrompt, prompt, duplicateClustersSchema, images, 8000)
			if err != nil {
				run.fail(group, err)
				failed = true
				break
			}
			for first := 0; first < len(call); first++ {
				for second := first + 1; second < len(call); second++ {
					seenTogether[orderedPair(call[first], call[second])]++
				}
			}
			for side, answer := range answers {
				run.CostUSD += answer.CostUSD
				models[side] = answer.Model
				var parsed struct {
					Clusters []struct {
						Members []string `json:"members"`
						Reason  string   `json:"reason"`
					} `json:"clusters"`
				}
				if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
					run.fail(group, fmt.Errorf("%s's clusters are not a list: %w", answer.Model, err))
					failed = true
					continue
				}
				for _, cluster := range parsed.Clusters {
					indexes := []int{}
					for _, label := range cluster.Members {
						if index, known := labelIndex[strings.TrimSpace(label)]; known && !containsInt(indexes, index) {
							indexes = append(indexes, index)
						}
					}
					for first := 0; first < len(indexes); first++ {
						for second := first + 1; second < len(indexes); second++ {
							pair := orderedPair(indexes[first], indexes[second])
							clustered[side][pair]++
							reasons[side][pair] = cluster.Reason
						}
					}
				}
			}
		}
		if failed {
			continue // a group read only in part is not acted on
		}
		agreed := map[memberPair]bool{}
		pairs := []memberPair{}
		for pair := range seenTogether {
			pairs = append(pairs, pair)
		}
		sort.Slice(pairs, func(i, j int) bool {
			return pairs[i][0] < pairs[j][0] || (pairs[i][0] == pairs[j][0] && pairs[i][1] < pairs[j][1])
		})
		for _, pair := range pairs {
			agreed[pair] = clustered[0][pair] == seenTogether[pair] && clustered[1][pair] == seenTogether[pair]
		}
		// Sets of members to merge: a pair joins two sets only when every
		// pair across them was agreed, so a chain a~b~c with a≁c is not one
		// piece.
		setOf := make([]int, len(members))
		for index := range setOf {
			setOf[index] = index
		}
		joinable := func(first, second int) bool {
			for a := range members {
				for b := range members {
					if setOf[a] == setOf[first] && setOf[b] == setOf[second] && !agreed[orderedPair(a, b)] {
						return false
					}
				}
			}
			return true
		}
		questionPairs := []memberPair{}
		for _, pair := range pairs {
			switch {
			case agreed[pair] && setOf[pair[0]] != setOf[pair[1]] && joinable(pair[0], pair[1]):
				from, to := setOf[pair[1]], setOf[pair[0]]
				for index := range setOf {
					if setOf[index] == from {
						setOf[index] = to
					}
				}
			case agreed[pair] && setOf[pair[0]] == setOf[pair[1]]:
			case agreed[pair] || clustered[0][pair] > 0 || clustered[1][pair] > 0:
				questionPairs = append(questionPairs, pair)
			}
		}
		mergedInto := map[int]int{}
		sets := map[int][]int{}
		for index, set := range setOf {
			sets[set] = append(sets[set], index)
		}
		setKeys := []int{}
		for key := range sets {
			setKeys = append(setKeys, key)
		}
		sort.Ints(setKeys)
		for _, key := range setKeys {
			set := sets[key]
			if len(set) < 2 {
				continue
			}
			keeper := set[0]
			for _, index := range set[1:] {
				kept, _ := keeperOf(members[keeper], members[index])
				if kept["id"] == members[index]["id"] {
					keeper = index
				}
			}
			for _, index := range set {
				if index == keeper {
					continue
				}
				duplicateID, keptID := members[index]["id"].(string), members[keeper]["id"].(string)
				if questionID, err := store.questionAboutPossessionPair(duplicateID, keptID); err != nil {
					return run, err
				} else if questionID != "" {
					run.leave(duplicateID+" "+keptID, "already asked in "+questionID)
					continue
				}
				pair := orderedPair(index, keeper)
				reason := fmt.Sprintf("both readers saw one piece: %s (%s); %s (%s)", reasons[0][pair], models[0], reasons[1][pair], models[1])
				merged, err := store.MergePossession(ctx, duplicateID, keptID, actor, reason)
				if err != nil {
					run.fail(duplicateID, fmt.Errorf("merge into %s: %w", keptID, err))
					continue
				}
				mergedInto[index] = keeper
				run.Counts["merged"]++
				run.Actions = append(run.Actions, AuditAction{Action: "merged", Records: []string{duplicateID, keptID}, MergeID: merged["merge_id"].(string), Reason: reason})
			}
		}
		asked := map[memberPair]bool{}
		for _, pair := range questionPairs {
			first, second := pair[0], pair[1]
			if keeper, merged := mergedInto[first]; merged {
				first = keeper
			}
			if keeper, merged := mergedInto[second]; merged {
				second = keeper
			}
			if first == second || asked[orderedPair(first, second)] {
				continue
			}
			asked[orderedPair(first, second)] = true
			action, err := store.askAboutDuplicate(ctx, members[first]["id"].(string), members[second]["id"].(string), models, [2]string{reasonOrNo(clustered[0][pair], reasons[0][pair]), reasonOrNo(clustered[1][pair], reasons[1][pair])})
			if err != nil {
				run.fail(members[first]["id"].(string)+" "+members[second]["id"].(string), err)
				continue
			}
			if action.Action == "" {
				run.leave(members[first]["id"].(string)+" "+members[second]["id"].(string), action.Reason)
				continue
			}
			run.Counts["asked"]++
			run.Actions = append(run.Actions, action)
		}
	}
	allGroups, err := store.ListWearableGroups()
	if err != nil {
		return run, err
	}
	for _, group := range allGroups {
		if group.Closed && group.Unconfirmed > 0 {
			if err := reviewer.settleUnconfirmedMembers(ctx, cache, group.Group, actor, &run); err != nil {
				return run, err
			}
		}
	}
	return run, nil
}

func reasonOrNo(count int, reason string) string {
	if count == 0 {
		return "different pieces"
	}
	return "the same piece: " + reason
}

func containsInt(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// questionAboutPossessionPair answers a question, open or answered, already
// asked about merging these two, in either direction; "" when none.
func (s *Store) questionAboutPossessionPair(first, second string) (string, error) {
	for _, pair := range [][2]string{{first, second}, {second, first}} {
		ids, err := stringColumn(s.db, `SELECT id FROM owner_questions WHERE strpos(choices, '"merge_possessions"') > 0 AND strpos(choices, ?) > 0 AND strpos(choices, ?) > 0 ORDER BY seq LIMIT 1`,
			jsonField("possession_id", pair[0]), jsonField("into_possession_id", pair[1]))
		if err != nil {
			return "", err
		}
		if len(ids) > 0 {
			return ids[0], nil
		}
	}
	return "", nil
}

// askAboutDuplicate asks whether two pieces are one, on a sighting of
// either. An AuditAction with no Action says why nothing was asked.
func (s *Store) askAboutDuplicate(ctx context.Context, firstID, secondID string, models, said [2]string) (AuditAction, error) {
	if questionID, err := s.questionAboutPossessionPair(firstID, secondID); err != nil || questionID != "" {
		return AuditAction{Reason: "already asked in " + questionID}, err
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuditAction{}, err
	}
	defer transaction.Rollback()
	first, err := possessionView(transaction, firstID, false)
	if err != nil {
		return AuditAction{}, err
	}
	second, err := possessionView(transaction, secondID, false)
	if err != nil {
		return AuditAction{}, err
	}
	kept, duplicate := keeperOf(first, second)
	keptID, duplicateID := kept["id"].(string), duplicate["id"].(string)
	sightingID, err := sightingShowing(transaction, duplicateID)
	if err != nil {
		return AuditAction{}, err
	}
	if sightingID == "" {
		if sightingID, err = sightingShowing(transaction, keptID); err != nil {
			return AuditAction{}, err
		}
	}
	if sightingID == "" {
		return AuditAction{Reason: "neither was recorded from a photo review, so there is no sighting to ask on"}, nil
	}
	question := fmt.Sprintf("Are %s (%s) and %s (%s) one piece recorded twice? %s says %s; %s says %s. Pictures: /possessions/%s/image and /possessions/%s/image.",
		duplicate["name"], duplicateID, kept["name"], keptID, models[0], said[0], models[1], said[1], duplicateID, keptID)
	choices := []Choice{
		{Label: fmt.Sprintf("The same piece: keep %s (%s)", keptID, kept["name"]), Resolution: Resolution{Action: "merge_possessions", PossessionID: duplicateID, IntoPossessionID: keptID}},
		{Label: "Different pieces", Resolution: Resolution{Action: "keep_apart", PossessionID: duplicateID, IntoPossessionID: keptID}},
	}
	questionID, err := askAuditQuestion(transaction, "duplicate_check", sightingID, question, choices, s.unixNow())
	if err != nil {
		return AuditAction{}, err
	}
	return AuditAction{Action: "asked", Records: []string{duplicateID, keptID, sightingID}, QuestionID: questionID, Reason: question}, transaction.Commit()
}
