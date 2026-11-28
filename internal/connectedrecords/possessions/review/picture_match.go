package review

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge/msg"
)

// The picture match: both readers see a piece cut from a photo of the owner
// beside pictures of the recorded pieces it could be, and each picks one.
// In an open group they see the covers of up to pieceMatchCandidates
// nearest pieces and may say none (a new piece). In a closed group
// (possessions/wearable_groups.go) they see every owned member, with up to
// closedMemberPictures photos of each, and must pick one or say they cannot
// tell: nothing in a closed group is recorded as new.

// at most this many recorded pieces are shown beside a piece in an open group
const pieceMatchCandidates = 12

// at most this many pictures of each member of a closed group are shown
const closedMemberPictures = 6

// at most this many pictures go in one call; a closed group with more is
// shown in batches, each with the piece again
const pictureMatchPicturesPerCall = 30

var pieceMatchSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["match","reason"],"properties":{
	"match":{"type":"string","description":"The possession id of the recorded piece that is the same physical item; otherwise the word the question allows (none, cannot_tell or not_here)"},
	"reason":{"type":"string","description":"A few words: what shows it"}}}`)

// matchCandidate is one recorded piece shown in a picture match.
type matchCandidate struct {
	ID       string
	Name     string
	Text     string   // describeWearable
	Pictures [][]byte // its cover first
}

// PieceMatch is what the two readers said about which recorded wearable a
// piece is.
type PieceMatch struct {
	Asked      bool
	Group      string
	Closed     bool
	Candidates []matchCandidate
	Models     [2]string
	Picks      [2]string // a candidate id, none (open group), cannot_tell (closed group) or unreadable
	Reasons    [2]string
	CostUSD    float64
}

func (match PieceMatch) candidate(id string) (matchCandidate, bool) {
	for _, candidate := range match.Candidates {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return matchCandidate{}, false
}

func (match PieceMatch) said() string {
	return fmt.Sprintf("%s says %s (%s); %s says %s (%s)", match.Models[0], match.Picks[0], match.Reasons[0], match.Models[1], match.Picks[1], match.Reasons[1])
}

// agreedResolution answers what both readers agree the piece is: a member
// they both picked, or, in an open group, a new piece (or the earlier
// sighting the reading named) when both say none. ok is false otherwise.
func (match PieceMatch) agreedResolution(version Proposal) (Resolution, bool) {
	if !match.Asked || match.Picks[0] != match.Picks[1] {
		return Resolution{}, false
	}
	pick := match.Picks[0]
	if _, shown := match.candidate(pick); shown {
		return Resolution{Action: "same_as", PossessionID: pick}, true
	}
	if pick != "none" || match.Closed {
		return Resolution{}, false
	}
	if version.ExistingSightingID != "" {
		return Resolution{Action: "same_as_sighting", SightingID: version.ExistingSightingID}, true
	}
	fresh := version
	fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
	return VersionChoice("", fresh).Resolution, true
}

// questionChoices are the answers offered when the readers do not agree.
// A closed group offers each member and "not mine", never a new piece.
func (match PieceMatch) questionChoices(version Proposal) []Choice {
	choices := []Choice{}
	add := func(candidate matchCandidate) {
		for _, choice := range choices {
			if choice.Resolution.PossessionID == candidate.ID {
				return
			}
		}
		choices = append(choices, Choice{Label: fmt.Sprintf("It is %s (%s)", candidate.ID, candidate.Name), Resolution: Resolution{Action: "same_as", PossessionID: candidate.ID}})
	}
	if match.Closed {
		for _, candidate := range match.Candidates {
			add(candidate)
		}
		return append(choices, RejectChoice)
	}
	for _, pick := range match.Picks {
		if candidate, shown := match.candidate(pick); shown {
			add(candidate)
		}
	}
	fresh := version
	fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
	return append(choices, VersionChoice("A piece not recorded yet", fresh), RejectChoice)
}

// questionText says what the readers could not settle.
func (match PieceMatch) questionText(label, summary string) string {
	if match.Closed {
		pictures := []string{}
		for _, candidate := range match.Candidates {
			pictures = append(pictures, "/possessions/"+candidate.ID+"/image")
		}
		return fmt.Sprintf("%s: %s. You said every %s you own is recorded, so it is one of them or not yours; comparing pictures, the readers could not settle which: %s. Pictures: %s.",
			label, summary, strings.ReplaceAll(match.Group, ":", " "), match.said(), strings.Join(pictures, ", "))
	}
	return fmt.Sprintf("%s: %s. Comparing pictures, the readers differ: %s.", label, summary, match.said())
}

// memberPictures cuts up to limit photos of a possession, its cover first.
func (cache *PictureCache) memberPictures(ctx context.Context, q possessions.Querier, possessionID string, limit int) ([][]byte, error) {
	links, err := possessions.PossessionPhotos(q, possessionID)
	if err != nil {
		return nil, err
	}
	pictures := [][]byte{}
	for _, link := range links {
		if len(pictures) == limit {
			break
		}
		picture, err := cache.linkCrop(ctx, link)
		if err != nil {
			return nil, fmt.Errorf("cut %s of %s: %w", link.PhotoID, possessionID, err)
		}
		pictures = append(pictures, picture)
	}
	return pictures, nil
}

// matchCandidatesFor answers the pieces a piece of this proposal's group is
// compared with, and whether the group is closed. exclude leaves out pieces
// the piece is known not to be.
func (reviewer *Reviewer) matchCandidatesFor(ctx context.Context, cache *PictureCache, proposal Proposal, label string, exclude []string) ([]matchCandidate, bool, error) {
	store := reviewer.Store
	group := ProposalGroup(proposal)
	if group == "" {
		return nil, false, nil
	}
	closed, err := possessions.GroupClosed(store.DB, group)
	if err != nil {
		return nil, false, err
	}
	var recorded []map[string]any
	limit := 1
	if closed {
		if recorded, err = store.ClosedGroupMembers(group, exclude...); err != nil {
			return nil, true, err
		}
		limit = closedMemberPictures
	} else if recorded, err = store.nearestRecorded(proposal, label, exclude); err != nil {
		return nil, false, err
	}
	candidates := []matchCandidate{}
	for _, possession := range recorded {
		id := possession["id"].(string)
		pictures, err := cache.memberPictures(ctx, store.DB, id, limit)
		if err != nil {
			return nil, closed, err
		}
		candidates = append(candidates, matchCandidate{ID: id, Name: possession["name"].(string), Text: DescribeWearable(possession), Pictures: pictures})
	}
	return candidates, closed, nil
}

// nearestRecorded answers up to pieceMatchCandidates recorded wearables of
// the proposal's group that have a picture, nearest first by their words;
// the reader's own match is always among them.
func (s *Store) nearestRecorded(proposal Proposal, label string, exclude []string) ([]map[string]any, error) {
	group := ProposalGroup(proposal)
	ofKind, err := s.ListPossessions(possessions.PossessionFilter{Kind: proposal.Kind, Limit: 1000})
	if err != nil {
		return nil, err
	}
	wanted := strings.Fields(strings.ToLower(strings.Join([]string{proposal.Name, proposal.Brand, proposal.Model, label, proposal.Description}, " ")))
	for _, raw := range proposal.KindFields {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			wanted = append(wanted, strings.ToLower(value))
		}
	}
	type scored struct {
		possession map[string]any
		score      int
	}
	candidates := []scored{}
	for _, possession := range ofKind {
		status := possession["status"].(string)
		if (status != "owned" && status != "unconfirmed" && status != "lent_out") || possession["cover_photo_id"] == nil || possessions.PossessionGroup(possession) != group || slices.Contains(exclude, possession["id"].(string)) {
			continue
		}
		words := strings.ToLower(DescribeWearable(possession))
		score := 0
		for _, word := range wanted {
			if len(word) > 2 && strings.Contains(words, word) {
				score++
			}
		}
		if possession["id"] == proposal.ExistingPossessionID {
			score += 1000
		}
		candidates = append(candidates, scored{possession, score})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	chosen := []map[string]any{}
	for _, candidate := range candidates {
		if len(chosen) == pieceMatchCandidates {
			break
		}
		chosen = append(chosen, candidate.possession)
	}
	return chosen, nil
}

// comparePictures asks both readers which candidate the piece is. piece
// says in words what the piece pictures show. It answers the match and
// every call's answers, so the caller can save them.
func (reviewer *Reviewer) comparePictures(ctx context.Context, pieceCrops [][]byte, piece, group string, closed bool, candidates []matchCandidate) (PieceMatch, [][2]possessions.ModelAnswer, error) {
	match := PieceMatch{Asked: true, Group: group, Closed: closed, Candidates: candidates}
	if len(candidates) == 0 {
		match.Asked = closed // a closed group with no owned member can only ask
		match.Picks = [2]string{"cannot_tell", "cannot_tell"}
		match.Reasons = [2]string{"no owned member to compare", "no owned member to compare"}
		return match, nil, nil
	}
	// Batches of candidates, each fitting in one call beside the piece.
	batches := [][]matchCandidate{}
	room := pictureMatchPicturesPerCall - len(pieceCrops)
	current, used := []matchCandidate{}, 0
	for _, candidate := range candidates {
		pictures := max(len(candidate.Pictures), 1)
		if len(current) > 0 && used+pictures > room {
			batches = append(batches, current)
			current, used = []matchCandidate{}, 0
		}
		current = append(current, candidate)
		used += pictures
	}
	batches = append(batches, current)
	allAnswers := [][2]possessions.ModelAnswer{}
	named := [2][]string{}
	reasons := [2][]string{}
	unreadable := [2]bool{}
	for _, batch := range batches {
		images := []msg.OneShotImage{}
		for _, crop := range pieceCrops {
			images = append(images, JPEGImage(crop))
		}
		pieceText := "Picture 1 shows"
		if len(pieceCrops) > 1 {
			pieceText = fmt.Sprintf("Pictures 1 to %d show", len(pieceCrops))
		}
		listing := []string{}
		for _, candidate := range batch {
			first := len(images) + 1
			for _, picture := range candidate.Pictures {
				images = append(images, JPEGImage(picture))
			}
			where := "no picture"
			switch {
			case len(candidate.Pictures) == 1:
				where = fmt.Sprintf("picture %d", first)
			case len(candidate.Pictures) > 1:
				where = fmt.Sprintf("pictures %d to %d", first, len(images))
			}
			listing = append(listing, fmt.Sprintf("- %s — %s", where, candidate.Text))
		}
		var ask string
		switch {
		case !closed:
			ask = "Is it the same physical piece as one of them? Answer its possession id, or none."
		case len(batches) == 1:
			ask = fmt.Sprintf("The owner says these are every %s they own, so it is one of them. Which? Answer its possession id, or cannot_tell when the pictures do not show which.", strings.ReplaceAll(group, ":", " "))
		default:
			ask = fmt.Sprintf("The owner says every %s they own is recorded, and these are some of them. Is it one of these? Answer its possession id, not_here when it is none of these, or cannot_tell.", strings.ReplaceAll(group, ":", " "))
		}
		prompt := fmt.Sprintf("%s a piece the owner has on %s\n\nThe other pictures are pieces already recorded in the inventory:\n%s\n\n%s", pieceText, piece, strings.Join(listing, "\n"), ask)
		answers, err := reviewer.AskBothReaders(ctx, AuditSystemPrompt, prompt, pieceMatchSchema, images, 4000)
		if err != nil {
			return match, allAnswers, err
		}
		allAnswers = append(allAnswers, answers)
		for side, answer := range answers {
			match.CostUSD += answer.CostUSD
			match.Models[side] = answer.Model
			var parsed struct {
				Match  string `json:"match"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
				return match, allAnswers, fmt.Errorf("%s's match is not one: %w", answer.Model, err)
			}
			pick := strings.TrimSpace(parsed.Match)
			reasons[side] = append(reasons[side], parsed.Reason)
			inBatch := false
			for _, candidate := range batch {
				inBatch = inBatch || candidate.ID == pick
			}
			switch {
			case inBatch:
				named[side] = append(named[side], pick)
			case pick == "none" && !closed, (pick == "cannot_tell" || pick == "not_here") && closed:
			default:
				unreadable[side] = true
				reasons[side] = append(reasons[side], fmt.Sprintf("answered %q, which the question does not allow", pick))
			}
		}
	}
	for side := range named {
		match.Reasons[side] = strings.Join(reasons[side], "; ")
		switch {
		case unreadable[side]:
			match.Picks[side] = "unreadable"
		case len(named[side]) == 1:
			match.Picks[side] = named[side][0]
		case len(named[side]) > 1:
			match.Picks[side] = "cannot_tell"
			match.Reasons[side] += "; named " + strings.Join(named[side], " and ")
		case closed:
			match.Picks[side] = "cannot_tell"
		default:
			match.Picks[side] = "none"
		}
	}
	return match, allAnswers, nil
}

// saveMatchReadings saves a match's calls as readings of a review, each
// call one round after the last saved.
func (s *Store) saveMatchReadings(reviewID string, roles [2]string, answers [][2]possessions.ModelAnswer) error {
	for _, pair := range answers {
		var round int
		if err := s.DB.QueryRow(`SELECT COALESCE(MAX(round), 0) + 1 FROM model_readings WHERE photo_review_id = ?`, reviewID).Scan(&round); err != nil {
			return err
		}
		for side, answer := range pair {
			if err := s.SaveReading(reviewID, round, roles[side], answer); err != nil {
				return err
			}
		}
	}
	return nil
}

// MatchPieces has both readers compare each wearable piece of an outfit
// reading, cut from the photo, with the recorded pieces it could be.
func (reviewer *Reviewer) MatchPieces(ctx context.Context, reviewID string, photo []byte, reading FinalReading) ([]PieceMatch, error) {
	store := reviewer.Store
	matches := make([]PieceMatch, len(reading.Items))
	cache := NewPictureCache(reviewer.Photos)
	roles := [2]string{reviewer.FirstReaderRole, reviewer.SecondReaderRole}
	for index := range reading.Items {
		item := &reading.Items[index]
		version := store.VersionFor(item)
		if !slices.Contains(possessions.WearableKinds, version.Kind) {
			continue
		}
		candidates, closed, err := reviewer.matchCandidatesFor(ctx, cache, version, item.Label, nil)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 && !closed {
			continue
		}
		box := item.Box.Clamped()
		if !box.Valid() {
			box = possessions.Box{Left: 0, Top: 0, Right: 1, Bottom: 1}
		}
		crop, err := possessions.CropJPEG(photo, box)
		if err != nil {
			return nil, fmt.Errorf("cut the %s from the photo: %w", item.Label, err)
		}
		match, answers, err := reviewer.comparePictures(ctx, [][]byte{crop}, fmt.Sprintf("in a new photo: %s (%s).", item.Label, version.Summary()), ProposalGroup(version), closed, candidates)
		if err != nil {
			return nil, fmt.Errorf("match the %s: %w", item.Label, err)
		}
		if err := store.saveMatchReadings(reviewID, roles, answers); err != nil {
			return nil, err
		}
		matches[index] = match
	}
	return matches, nil
}

// RecordMatchedPiece records one piece of a new outfit reading as the
// readers' match says: what both agree on, or a decision question. The
// reader's doubt, if any, follows a recorded piece as a detail question. It
// says whether it asked a decision question.
func (s *Store) RecordMatchedPiece(ctx context.Context, transaction *possessions.DatabaseTransaction, sightingID, reviewID, label string, version Proposal, doubt string, match PieceMatch, now int64) (bool, error) {
	summary := version.Summary()
	if resolution, agreed := match.agreedResolution(version); agreed {
		if err := s.ApplyResolution(ctx, transaction, sightingID, resolution, "agreement", reviewID, now); err != nil {
			return false, err
		}
		if doubt != "" {
			return false, InsertQuestion(transaction, sightingID, fmt.Sprintf("Recorded as %s (both readers: %s). To add: %s", summary, match.said(), doubt), []Choice{KeepChoice}, "detail", now)
		}
		return false, nil
	}
	question := match.questionText(label, summary)
	if doubt != "" {
		question += " The reader also asks: " + doubt
	}
	questionID, err := InsertQuestionAnsweringID(transaction, sightingID, question, match.questionChoices(version), "decision", now)
	if err != nil {
		return false, err
	}
	_, err = transaction.Exec(`UPDATE owner_questions SET asked_by = 'picture_match' WHERE id = ?`, questionID)
	return true, err
}

// ---------------------------------------------------------------------------
// Rematch.
// ---------------------------------------------------------------------------

// RematchOutfitPieces runs the picture match again where it never settled
// anything. First, each outfit-reading sighting still proposed behind an
// open decision question: when both readers agree, the sighting is settled
// as they say, its open decision questions are answered by picture_match
// with that resolution, and the piece joins its outfit; otherwise the
// question stays. Then each unconfirmed possession in a closed group: when
// both readers pick one owned member from its photos, it is merged into it
// (undoably); otherwise a question lists the members.
func (reviewer *Reviewer) RematchOutfitPieces(ctx context.Context, actor string) (AuditRun, error) {
	store := reviewer.Store
	run := NewAuditRun()
	if actor == "" {
		return run, fmt.Errorf("%w: actor is required", possessions.ErrInvalid)
	}
	sightingIDs, err := possessions.StringColumn(store.DB, `SELECT sightings.id FROM sightings JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id
		JOIN owner_questions ON owner_questions.sighting_id = sightings.id
		WHERE photo_reviews.reading = 'outfit' AND sightings.status = 'proposed' AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision'
		GROUP BY sightings.id, sightings.seq ORDER BY sightings.seq`)
	if err != nil {
		return run, err
	}
	cache := NewPictureCache(reviewer.Photos)
	roles := [2]string{reviewer.FirstReaderRole, reviewer.SecondReaderRole}
	for _, sightingID := range sightingIDs {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		sighting, err := SightingByID(store.DB, sightingID)
		if err != nil {
			return run, err
		}
		if sighting.Status != "proposed" {
			run.Counts["settled_meanwhile"]++
			continue
		}
		if !slices.Contains(possessions.WearableKinds, sighting.Proposed.Kind) || ProposalGroup(sighting.Proposed) == "" {
			run.Leave(sightingID, fmt.Sprintf("its proposal is a %q, not a wearable", sighting.Proposed.Kind))
			continue
		}
		run.Looked++
		exclude, err := unlinkedPossessions(store.DB, sightingID)
		if err != nil {
			return run, err
		}
		candidates, closed, err := reviewer.matchCandidatesFor(ctx, cache, sighting.Proposed, sighting.Label, exclude)
		if err != nil {
			run.Fail(sightingID, err)
			continue
		}
		if len(candidates) == 0 && !closed {
			run.Leave(sightingID, "no recorded piece of its group to compare")
			continue
		}
		photo, err := cache.reviewImage(ctx, sighting.PhotoID)
		if err != nil {
			run.Fail(sightingID, err)
			continue
		}
		crop, err := possessions.CropJPEG(photo, sighting.Box)
		if err != nil {
			run.Fail(sightingID, fmt.Errorf("cut it from %s: %w", sighting.PhotoID, err))
			continue
		}
		match, answers, err := reviewer.comparePictures(ctx, [][]byte{crop}, fmt.Sprintf("in a new photo: %s (%s).", sighting.Label, sighting.Proposed.Summary()), ProposalGroup(sighting.Proposed), closed, candidates)
		run.CostUSD += match.CostUSD
		if saveErr := store.saveMatchReadings(sighting.PhotoReviewID, roles, answers); saveErr != nil {
			return run, saveErr
		}
		if err != nil {
			run.Fail(sightingID, err)
			continue
		}
		resolution, agreed := match.agreedResolution(sighting.Proposed)
		if !agreed {
			run.Counts["disagreed"]++
			continue
		}
		if err := store.settleByPictureMatch(ctx, sightingID, resolution, match); err != nil {
			run.Fail(sightingID, err)
			continue
		}
		settled, err := SightingByID(store.DB, sightingID)
		if err != nil {
			return run, err
		}
		records := []string{sightingID}
		if settled.PossessionID != nil {
			records = append(records, *settled.PossessionID)
		}
		run.Counts["matched"]++
		run.Actions = append(run.Actions, AuditAction{Action: "matched", Records: records, Reason: match.said()})
	}
	groups, err := store.ListWearableGroups()
	if err != nil {
		return run, err
	}
	for _, group := range groups {
		if group.Closed && group.Unconfirmed > 0 {
			if err := reviewer.settleUnconfirmedMembers(ctx, cache, group.Group, actor, &run); err != nil {
				return run, err
			}
		}
	}
	return run, nil
}

// unlinkedPossessions answers the possessions a sighting was unlinked from:
// the link check found it is not them.
func unlinkedPossessions(q possessions.Querier, sightingID string) ([]string, error) {
	return possessions.StringColumn(q, `SELECT DISTINCT record::jsonb ->> 'possession_id' FROM revisions
		WHERE record_id = ? AND record::jsonb ->> 'table' = 'sightings' AND record::jsonb ->> 'possession_id' IS NOT NULL`, sightingID)
}

// settleByPictureMatch settles a sighting as both readers agreed and
// answers its open decision questions with that resolution.
func (s *Store) settleByPictureMatch(ctx context.Context, sightingID string, resolution Resolution, match PieceMatch) error {
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	sighting, err := SightingByID(transaction, sightingID)
	if err != nil {
		return err
	}
	if sighting.Status != "proposed" {
		return fmt.Errorf("%w: %s was settled meanwhile (%s)", possessions.ErrConflict, sightingID, sighting.Status)
	}
	now := s.UnixNow()
	if err := s.ApplyResolution(ctx, transaction, sightingID, resolution, "picture_match", sighting.PhotoReviewID, now); err != nil {
		return err
	}
	resolution.Explanation = "both readers matched the picture: " + match.said()
	encoded, _ := json.Marshal(resolution)
	questionIDs, err := possessions.StringColumn(transaction, `SELECT id FROM owner_questions WHERE sighting_id = ? AND answered_at = 0 AND purpose = 'decision' ORDER BY seq`, sightingID)
	if err != nil {
		return err
	}
	for _, questionID := range questionIDs {
		question, err := QuestionByID(transaction, questionID)
		if err != nil {
			return err
		}
		var chosen any
		for index, choice := range question.Choices {
			if choice.Resolution.Action == resolution.Action && choice.Resolution.PossessionID == resolution.PossessionID && choice.Resolution.SightingID == resolution.SightingID &&
				choice.Resolution.UnlinkPossessionID == "" {
				chosen = index
				break
			}
		}
		if err := possessions.SaveRevision(transaction, "owner_questions", "id", questionID, "picture_match", now); err != nil {
			return err
		}
		if _, err := transaction.Exec(`UPDATE owner_questions SET answer_choice = ?, resolution = ?, answered_by = 'picture_match', answered_at = ? WHERE id = ?`, chosen, string(encoded), now, questionID); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'done', finished_at = ? WHERE id = ? AND status = 'waiting_for_owner'
		AND NOT EXISTS (SELECT 1 FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
			WHERE sightings.photo_review_id = photo_reviews.id AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision')`, now, sighting.PhotoReviewID); err != nil {
		return err
	}
	return transaction.Commit()
}

// settleUnconfirmedMembers matches each unconfirmed possession of a closed
// group, by its photos, against the owned members: both readers pick the
// same member, it is merged into that member; otherwise a decision question
// lists the members. One already asked about is left to the owner.
func (reviewer *Reviewer) settleUnconfirmedMembers(ctx context.Context, cache *PictureCache, group, actor string, run *AuditRun) error {
	store := reviewer.Store
	wearables, err := store.LiveWearables()
	if err != nil {
		return err
	}
	for _, possession := range wearables {
		if possessions.PossessionGroup(possession) != group || possession["status"] != "unconfirmed" {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		possessionID := possession["id"].(string)
		if questionID, err := questionAboutMergingAway(store.DB, possessionID); err != nil {
			return err
		} else if questionID != "" {
			run.Leave(possessionID, "already asked in "+questionID)
			continue
		}
		run.Looked++
		crops, err := cache.memberPictures(ctx, store.DB, possessionID, closedMemberPictures)
		if err != nil {
			run.Fail(possessionID, err)
			continue
		}
		if len(crops) == 0 {
			run.Leave(possessionID, "no picture to compare")
			continue
		}
		members, err := store.ClosedGroupMembers(group)
		if err != nil {
			return err
		}
		candidates := []matchCandidate{}
		for _, member := range members {
			pictures, err := cache.memberPictures(ctx, store.DB, member["id"].(string), closedMemberPictures)
			if err != nil {
				return err
			}
			candidates = append(candidates, matchCandidate{ID: member["id"].(string), Name: member["name"].(string), Text: DescribeWearable(member), Pictures: pictures})
		}
		match, _, err := reviewer.comparePictures(ctx, crops, fmt.Sprintf("in photos of them, recorded as %s.", DescribeWearable(possession)), group, true, candidates)
		run.CostUSD += match.CostUSD
		if err != nil {
			run.Fail(possessionID, err)
			continue
		}
		if resolution, agreed := match.agreedResolution(Proposal{}); agreed {
			merged, err := store.MergePossession(ctx, possessionID, resolution.PossessionID, actor, "closed group "+group+"; both readers picked this member: "+match.said())
			if err != nil {
				run.Fail(possessionID, fmt.Errorf("merge into %s: %w", resolution.PossessionID, err))
				continue
			}
			run.Counts["merged"]++
			run.Actions = append(run.Actions, AuditAction{Action: "merged", Records: []string{possessionID, resolution.PossessionID}, MergeID: merged["merge_id"].(string), Reason: match.said()})
			continue
		}
		action, err := store.askWhichMember(ctx, possession, match)
		if err != nil {
			run.Fail(possessionID, err)
			continue
		}
		if action.Action == "" {
			run.Leave(possessionID, action.Reason)
			continue
		}
		run.Counts["asked"]++
		run.Actions = append(run.Actions, action)
	}
	return nil
}

// questionAboutMergingAway answers a question, open or answered, that
// offered to merge this possession into a closed group's member.
func questionAboutMergingAway(q possessions.Querier, possessionID string) (string, error) {
	ids, err := possessions.StringColumn(q, `SELECT id FROM owner_questions WHERE asked_by = 'closed_group' AND strpos(choices, ?) > 0 ORDER BY seq LIMIT 1`, possessions.JSONField("possession_id", possessionID))
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

// askWhichMember asks which owned member an unconfirmed possession of a
// closed group is.
func (s *Store) askWhichMember(ctx context.Context, possession map[string]any, match PieceMatch) (AuditAction, error) {
	possessionID := possession["id"].(string)
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return AuditAction{}, err
	}
	defer transaction.Rollback()
	sightingID, err := sightingShowing(transaction, possessionID)
	if err != nil {
		return AuditAction{}, err
	}
	if sightingID == "" {
		return AuditAction{Reason: "it was not recorded from a photo review, so there is no sighting to ask on"}, nil
	}
	choices := []Choice{}
	for _, candidate := range match.Candidates {
		choices = append(choices, Choice{Label: fmt.Sprintf("It is %s (%s)", candidate.ID, candidate.Name), Resolution: Resolution{Action: "merge_possessions", PossessionID: possessionID, IntoPossessionID: candidate.ID}})
	}
	choices = append(choices, Choice{Label: "None of these: leave it as it is", Resolution: Resolution{Action: "keep_apart", PossessionID: possessionID}})
	question := match.questionText(fmt.Sprintf("%s (%s, unconfirmed)", possession["name"], possessionID), "seen only in photos of you")
	questionID, err := AskAuditQuestion(transaction, "closed_group", sightingID, question, choices, s.UnixNow())
	if err != nil {
		return AuditAction{}, err
	}
	return AuditAction{Action: "asked", Records: []string{possessionID, sightingID}, QuestionID: questionID, Reason: question}, transaction.Commit()
}
