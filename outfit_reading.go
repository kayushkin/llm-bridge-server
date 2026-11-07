package possessionstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The outfit reading: one reader looks at a photo of the wearer and lists
// what they have on, matching each piece to the wearables already recorded.
// A piece it is sure of is recorded at once; a new piece starts unconfirmed,
// since a photo shows the wearer had it then, not that they have it now, and
// asks "do you still have it?". The pieces make an outfit, worn on the
// photo's day; a photo whose pieces match an outfit already recorded adds a
// wearing to that outfit instead of making a second one.

// wearableCandidateList lists, for the reader, every wearable the inventory
// holds and every wearable sighting from another photo still waiting on the
// owner.
func (s *Store) wearableCandidateList(excludeReviewID string) (string, error) {
	var list strings.Builder
	for _, kindName := range WearableKinds {
		possessions, err := s.ListPossessions(PossessionFilter{Kind: kindName, Limit: 1000})
		if err != nil {
			return "", err
		}
		for _, possession := range possessions {
			status := possession["status"].(string)
			if status != "owned" && status != "unconfirmed" && status != "lent_out" {
				continue
			}
			fmt.Fprintf(&list, "- %s: %s | %s %s | brand %q model %q | %s\n", possession["id"], possession["name"], kindName,
				summarizeKindRecord(possession[kindName].(map[string]any)), possession["brand"], possession["model"], truncateWords(possession["description"].(string), 30))
		}
	}
	rows, err := s.db.Query(`SELECT id, label, proposed FROM sightings WHERE status = 'proposed' AND photo_review_id != ? ORDER BY seq`, excludeReviewID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label, proposed string
		if err := rows.Scan(&id, &label, &proposed); err != nil {
			return "", err
		}
		var proposal Proposal
		if err := json.Unmarshal([]byte(proposed), &proposal); err != nil {
			return "", fmt.Errorf("%s holds a proposal that is not JSON: %w", id, err)
		}
		if !contains(WearableKinds, proposal.Kind) {
			continue
		}
		fmt.Fprintf(&list, "- %s (seen in another photo, waiting on the owner): %s | %s | brand %q | %s\n", id, label, proposal.Kind, proposal.Brand, truncateWords(proposal.Description, 30))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if list.Len() == 0 {
		return "(nothing yet)", nil
	}
	return list.String(), nil
}

func outfitPrompt(face Box, candidates string) string {
	return fmt.Sprintf(`This photo shows the owner; their face is in the box left %.2f, top %.2f, right %.2f, bottom %.2f (fractions of the photo from the top left). Other people may be in it: describe only what the owner has on.

List every piece the owner is wearing or carrying that can be seen: clothes, shoes, headwear, jewelry, a watch, glasses or sunglasses, a bag. Use only these kinds: %s. Give each a box. Leave out what cannot be seen well enough to tell apart from other pieces later (a collar at the frame edge); when only the face shows, the list is empty.

The owner's wearables already recorded, and pieces from other photos still waiting on the owner:
%s
Where a piece is clearly one listed above, put its id in existing_id: the same color, cut, pattern and marks, not merely the same kind of thing. Where it might be one of them but you cannot tell, or anything else that matters is unsure, write the one question you would ask the owner in doubt, and say in doubt_kind whether it is about which piece this is (identity) or only a fact about a piece you are sure of (detail). Otherwise leave both empty. from_readings stays empty.`,
		face.Left, face.Top, face.Right, face.Bottom, strings.Join(WearableKinds, ", "), candidates)
}

func (reviewer *Reviewer) reviewOutfit(ctx context.Context, review PhotoReview) error {
	store := reviewer.Store
	photo, imageSource, err := reviewer.Photos.ReviewImage(ctx, review.PhotoID)
	if err != nil {
		return err
	}
	face, err := reviewer.Photos.WearerFace(ctx, review.PhotoID, review.WearerID)
	if err != nil {
		return err
	}
	takenAt, err := reviewer.Photos.TakenAt(ctx, review.PhotoID)
	if err != nil {
		return err
	}
	if takenAt == 0 {
		return fmt.Errorf("%w: photo-store does not know when %s was taken, so the outfit has no day", ErrInvalid, review.PhotoID)
	}
	if reviewer.OwnerLocation == nil {
		return errors.New("the reviewer has no owner time zone")
	}
	wornOn := time.Unix(takenAt, 0).In(reviewer.OwnerLocation).Format("2006-01-02")
	if err := store.setReviewStatus(review.ID, "reading", imageSource, ""); err != nil {
		return err
	}
	candidates, err := store.wearableCandidateList(review.ID)
	if err != nil {
		return err
	}
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, readerSystemPrompt(), outfitPrompt(face, candidates), readingSchema(true),
		[]msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(photo)}}, 16000)
	if err != nil {
		return fmt.Errorf("reader %s: %w", reviewer.FirstReaderRole, err)
	}
	if err := store.saveReading(review.ID, 1, reviewer.FirstReaderRole, answer); err != nil {
		return err
	}
	var reading finalReading
	if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
		return fmt.Errorf("%s's answer is not a reading: %w", answer.Model, err)
	}
	matches, err := reviewer.matchPieces(ctx, review.ID, photo, reading)
	if err != nil {
		return err
	}
	return store.recordOutfitReading(ctx, review.ID, answer.Model, wornOn, reading, matches)
}

// pieceMatch is what the two readers said, comparing pictures, about which
// recorded wearable one piece of an outfit reading is.
type pieceMatch struct {
	Asked      bool
	Candidates map[string]string // possession id → name, of the pictures they were shown
	Models     [2]string
	Picks      [2]string // a possession id, or none
	Reasons    [2]string
}

func (match pieceMatch) agreed() bool { return match.Asked && match.Picks[0] == match.Picks[1] }

// at most this many recorded pieces are shown beside each new piece
const pieceMatchCandidates = 12

var pieceMatchSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["match","reason"],"properties":{
	"match":{"type":"string","description":"The possession id of the recorded piece that is the same physical item as picture 1, or none"},
	"reason":{"type":"string","description":"A few words: what shows it"}}}`)

// matchCandidates answers up to pieceMatchCandidates recorded wearables of
// the piece's kind and sub-type that have a picture, nearest first by their
// words; the reader's own match is always among them.
func (s *Store) matchCandidates(proposal Proposal, label string) ([]map[string]any, error) {
	group := proposalGroup(proposal)
	if group == "" {
		return nil, nil
	}
	possessions, err := s.ListPossessions(PossessionFilter{Kind: proposal.Kind, Limit: 1000})
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
	for _, possession := range possessions {
		status := possession["status"].(string)
		if (status != "owned" && status != "unconfirmed" && status != "lent_out") || possession["cover_photo_id"] == nil || possessionGroup(possession) != group {
			continue
		}
		words := strings.ToLower(describeWearable(possession))
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

// matchPieces has both readers compare each wearable piece of an outfit
// reading, cut from the photo, with pictures of the recorded pieces it
// could be. Each call is saved as a reading, round 2 onward, one per piece.
func (reviewer *Reviewer) matchPieces(ctx context.Context, reviewID string, photo []byte, reading finalReading) ([]pieceMatch, error) {
	store := reviewer.Store
	matches := make([]pieceMatch, len(reading.Items))
	cache := newPictureCache(reviewer.Photos)
	round := 2
	for index := range reading.Items {
		item := &reading.Items[index]
		version := store.versionFor(item)
		if !contains(WearableKinds, version.Kind) {
			continue
		}
		candidates, err := store.matchCandidates(version, item.Label)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			continue
		}
		box := item.Box.clamped()
		if !box.valid() {
			box = Box{0, 0, 1, 1}
		}
		crop, err := cropJPEG(photo, box)
		if err != nil {
			return nil, fmt.Errorf("cut the %s from the photo: %w", item.Label, err)
		}
		images := []msg.OneShotImage{jpegImage(crop)}
		listing := []string{}
		match := pieceMatch{Asked: true, Candidates: map[string]string{}}
		for _, candidate := range candidates {
			id := candidate["id"].(string)
			picture, err := cache.coverCrop(ctx, store.db, id)
			if err != nil {
				return nil, fmt.Errorf("cut %s's cover: %w", id, err)
			}
			images = append(images, jpegImage(picture))
			match.Candidates[id] = candidate["name"].(string)
			listing = append(listing, fmt.Sprintf("Picture %d — %s", len(images), describeWearable(candidate)))
		}
		prompt := fmt.Sprintf(`Picture 1 is a piece the owner has on in a new photo: %s (%s).

The other pictures are pieces already recorded in the inventory:
%s

Is picture 1 the same physical piece as one of them? Answer its possession id, or none.`, item.Label, version.summary(), strings.Join(listing, "\n"))
		answers, err := reviewer.askBothReaders(ctx, auditSystemPrompt, prompt, pieceMatchSchema, images, 4000)
		if err != nil {
			return nil, fmt.Errorf("match the %s: %w", item.Label, err)
		}
		roles := [2]string{reviewer.FirstReaderRole, reviewer.SecondReaderRole}
		for side, answer := range answers {
			if err := store.saveReading(reviewID, round, roles[side], answer); err != nil {
				return nil, err
			}
			var parsed struct {
				Match  string `json:"match"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
				return nil, fmt.Errorf("%s's match is not one: %w", answer.Model, err)
			}
			pick := strings.TrimSpace(parsed.Match)
			if _, shown := match.Candidates[pick]; !shown && pick != "none" {
				parsed.Reason = fmt.Sprintf("answered %q, which is not one of the pieces shown: %s", pick, parsed.Reason)
				pick = "unreadable"
			}
			match.Models[side], match.Picks[side], match.Reasons[side] = answer.Model, pick, parsed.Reason
		}
		matches[index] = match
		round++
	}
	return matches, nil
}

// recordOutfitReading writes the sightings of one outfit reading, records
// the pieces the reader was sure of, and makes or finds the outfit.
func (s *Store) recordOutfitReading(ctx context.Context, reviewID, model, wornOn string, reading finalReading, matches []pieceMatch) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'reconciling', worn_on = ? WHERE id = ?`, wornOn, reviewID); err != nil {
		return err
	}
	openQuestions := 0
	for index := range reading.Items {
		item := &reading.Items[index]
		version := s.versionFor(item)
		if !contains(WearableKinds, version.Kind) {
			continue // the reader was told to list wearables only
		}
		version.Status = "unconfirmed"
		box := item.Box.clamped()
		if !box.valid() {
			box = Box{0, 0, 1, 1}
		}
		sightingID, sequence, err := nextID(transaction, "sightings", "sighting")
		if err != nil {
			return err
		}
		proposed, _ := json.Marshal(version)
		versions, _ := json.Marshal(map[string]Proposal{model: version})
		if _, err := transaction.Exec(`INSERT INTO sightings (id, seq, photo_review_id, label, box_left, box_top, box_right, box_bottom, proposed, versions, agreement, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'single_reader', 'proposed')`,
			sightingID, sequence, reviewID, strings.TrimSpace(item.Label), box.Left, box.Top, box.Right, box.Bottom, string(proposed), string(versions)); err != nil {
			return err
		}
		doubt := strings.TrimSpace(item.Doubt)
		if index < len(matches) && matches[index].Asked {
			asked, err := s.recordMatchedPiece(ctx, transaction, sightingID, reviewID, item.Label, version, doubt, matches[index], now)
			if err != nil {
				return fmt.Errorf("record %s: %w", sightingID, err)
			}
			if asked {
				openQuestions++
			}
			continue
		}
		if doubt == "" || item.DoubtKind == "detail" {
			if err := s.applyResolution(ctx, transaction, sightingID, versionChoice("", version).Resolution, "agreement", reviewID, now); err != nil {
				return fmt.Errorf("record %s: %w", sightingID, err)
			}
			if doubt != "" {
				if err := insertQuestion(transaction, sightingID, fmt.Sprintf("Recorded as %s. To add: %s", version.summary(), doubt), []Choice{keepChoice}, "detail", now); err != nil {
					return err
				}
			}
			continue
		}
		choices := []Choice{versionChoice("As "+model+" says", version)}
		if version.ExistingPossessionID != "" || version.ExistingSightingID != "" {
			fresh := version
			fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
			choices = append(choices, versionChoice("A piece not recorded yet", fresh))
		}
		choices = append(choices, rejectChoice)
		if err := insertQuestion(transaction, sightingID, fmt.Sprintf("%s: %s %s asks: %s", item.Label, version.summary(), model, doubt), choices, "decision", now); err != nil {
			return err
		}
		openQuestions++
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = ?, finished_at = ? WHERE id = ?`, map[bool]string{true: "waiting_for_owner", false: "done"}[openQuestions > 0], now, reviewID); err != nil {
		return err
	}
	if err := s.settleReviewOutfit(transaction, reviewID, now); err != nil {
		return err
	}
	return transaction.Commit()
}

// settleReviewOutfit gives an outfit reading its outfit once its first
// reading is recorded: the outfit already recorded with exactly the same
// pieces, or a new one, and a wearing on the review's day either way.
func (s *Store) settleReviewOutfit(transaction *sql.Tx, reviewID string, now int64) error {
	review, err := photoReviewRow(transaction, reviewID)
	if err != nil {
		return err
	}
	if review.OutfitID != nil {
		return nil
	}
	pieces, err := stringColumn(transaction, `SELECT DISTINCT possession_id FROM sightings WHERE photo_review_id = ? AND status = 'accepted' AND possession_id IS NOT NULL ORDER BY seq`, reviewID)
	if err != nil {
		return err
	}
	if len(pieces) == 0 {
		return nil
	}
	outfitID, err := outfitWithExactly(transaction, pieces)
	if err != nil {
		return err
	}
	if outfitID == "" {
		if outfitID, err = sameDayOutfit(transaction, review.WornOn, pieces); err != nil {
			return err
		}
		if outfitID != "" {
			if err := saveRevision(transaction, "outfits", "id", outfitID, reviewID, now); err != nil {
				return err
			}
			for _, piece := range pieces {
				if _, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_possessions (outfit_id, possession_id, position)
					VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM outfit_possessions WHERE outfit_id = ?))`, outfitID, piece, outfitID); err != nil {
					return err
				}
			}
		}
	}
	if outfitID == "" {
		var sequence int64
		outfitID, sequence, err = nextID(transaction, "outfits", "outfit")
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at) VALUES (?, ?, '', '', '', 'kept', ?, '', ?, ?, ?)`,
			outfitID, sequence, "Worn in "+review.PhotoID, reviewID, now, now); err != nil {
			return err
		}
		if err := replaceOutfitPossessions(transaction, outfitID, pieces); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`INSERT OR IGNORE INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?)`, outfitID, review.WornOn); err != nil {
		return err
	}
	_, err = transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ?`, outfitID, reviewID)
	return err
}

// sameDayOutfit finds the live outfit worn on the same day that shares the
// most pieces with these, at least one; ties go to the older. A same-day
// outfit with nothing in common is a change of clothes, so it is not taken.
func sameDayOutfit(q querier, wornOn string, pieces []string) (string, error) {
	if wornOn == "" {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(pieces)), ", ")
	arguments := []any{wornOn}
	for _, piece := range pieces {
		arguments = append(arguments, piece)
	}
	var outfitID string
	err := q.QueryRow(`SELECT outfits.id FROM outfits JOIN outfit_wearings ON outfit_wearings.outfit_id = outfits.id JOIN outfit_possessions ON outfit_possessions.outfit_id = outfits.id
		WHERE outfits.deleted_at = 0 AND outfits.status != 'rejected' AND outfit_wearings.worn_on = ? AND outfit_possessions.possession_id IN (`+placeholders+`)
		GROUP BY outfits.id ORDER BY COUNT(*) DESC, outfits.seq LIMIT 1`, arguments...).Scan(&outfitID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return outfitID, err
}

// recordMatchedPiece records one piece both readers compared with the
// recorded pieces: the piece they agree on, a new piece when they agree it
// is none of them, or a decision question when they differ. The reader's
// doubt, if any, follows a recorded piece as a detail question. It says
// whether it asked a decision question.
func (s *Store) recordMatchedPiece(ctx context.Context, transaction *sql.Tx, sightingID, reviewID, label string, version Proposal, doubt string, match pieceMatch, now int64) (bool, error) {
	fresh := version
	fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
	said := fmt.Sprintf("%s says %s (%s); %s says %s (%s)", match.Models[0], match.Picks[0], match.Reasons[0], match.Models[1], match.Picks[1], match.Reasons[1])
	if !match.agreed() || match.Picks[0] == "unreadable" {
		choices := []Choice{}
		for _, pick := range match.Picks {
			if _, shown := match.Candidates[pick]; shown && (len(choices) == 0 || choices[0].Resolution.PossessionID != pick) {
				choices = append(choices, Choice{Label: fmt.Sprintf("It is %s (%s)", pick, match.Candidates[pick]), Resolution: Resolution{Action: "same_as", PossessionID: pick}})
			}
		}
		choices = append(choices, versionChoice("A piece not recorded yet", fresh), rejectChoice)
		question := fmt.Sprintf("%s: %s. Comparing pictures, the readers differ: %s.", label, fresh.summary(), said)
		if doubt != "" {
			question += " The reader also asks: " + doubt
		}
		questionID, err := insertQuestionAnsweringID(transaction, sightingID, question, choices, "decision", now)
		if err != nil {
			return false, err
		}
		_, err = transaction.Exec(`UPDATE owner_questions SET asked_by = 'picture_match' WHERE id = ?`, questionID)
		return true, err
	}
	resolution := versionChoice("", fresh).Resolution
	if match.Picks[0] != "none" {
		resolution = Resolution{Action: "same_as", PossessionID: match.Picks[0]}
	} else if version.ExistingSightingID != "" {
		resolution = Resolution{Action: "same_as_sighting", SightingID: version.ExistingSightingID}
	}
	if err := s.applyResolution(ctx, transaction, sightingID, resolution, "agreement", reviewID, now); err != nil {
		return false, err
	}
	if doubt != "" {
		return false, insertQuestion(transaction, sightingID, fmt.Sprintf("Recorded as %s (both readers: %s). To add: %s", fresh.summary(), said, doubt), []Choice{keepChoice}, "detail", now)
	}
	return false, nil
}

// outfitWithExactly finds a live outfit holding exactly these possessions.
func outfitWithExactly(q querier, pieces []string) (string, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(pieces)), ", ")
	arguments := []any{}
	for _, piece := range pieces {
		arguments = append(arguments, piece)
	}
	arguments = append(arguments, len(pieces), len(pieces))
	var outfitID string
	err := q.QueryRow(`SELECT outfits.id FROM outfits JOIN outfit_possessions ON outfit_possessions.outfit_id = outfits.id
		WHERE outfits.deleted_at = 0 AND outfits.status != 'rejected'
		GROUP BY outfits.id
		HAVING SUM(outfit_possessions.possession_id IN (`+placeholders+`)) = ? AND COUNT(*) = ?
		ORDER BY outfits.seq LIMIT 1`, arguments...).Scan(&outfitID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return outfitID, err
}

// addToReviewOutfit runs after any sighting is settled. A piece of an
// outfit reading accepted once the reading is done (an owner's answer, or a
// sighting it waited on being settled) joins that reading's outfit.
func (s *Store) addToReviewOutfit(transaction *sql.Tx, sightingID string, now int64) error {
	sighting, err := sightingByID(transaction, sightingID)
	if err != nil {
		return err
	}
	if sighting.Status != "accepted" || sighting.PossessionID == nil {
		return nil
	}
	review, err := photoReviewRow(transaction, sighting.PhotoReviewID)
	if err != nil {
		return err
	}
	if review.Reading != "outfit" || review.Status == "reconciling" || review.Status == "reading" {
		return nil // settleReviewOutfit gathers the pieces of a reading still being recorded
	}
	if review.OutfitID == nil {
		return s.settleReviewOutfit(transaction, review.ID, now)
	}
	_, err = transaction.Exec(`INSERT OR IGNORE INTO outfit_possessions (outfit_id, possession_id, position)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM outfit_possessions WHERE outfit_id = ?))`, *review.OutfitID, *sighting.PossessionID, *review.OutfitID)
	return err
}

// insertStillHaveItQuestion asks whether a piece seen in a photo is still
// owned. Each choice sets its status.
func insertStillHaveItQuestion(transaction *sql.Tx, sightingID, name string, now int64) error {
	choice := func(label, status string) Choice {
		return Choice{Label: label, Resolution: Resolution{Action: "amend", Proposal: &Proposal{Status: status, KindFields: map[string]json.RawMessage{}}}}
	}
	return insertQuestion(transaction, sightingID, fmt.Sprintf("Do you still have the %s?", name), []Choice{
		choice("Yes, I still have it", "owned"), choice("No, I gave it away", "given_away"), choice("No, I sold it", "sold"),
		choice("No, I lost it", "lost"), choice("No, I threw it out", "discarded"),
	}, "detail", now)
}

// DoubtTriage says what TriageOutfitDoubts did.
type DoubtTriage struct {
	Looked   int                 `json:"looked"`
	Identity int                 `json:"identity"` // left as decision questions
	Detail   int                 `json:"detail"`   // recorded, and the question made a detail question
	Failures []map[string]string `json:"failures"`
	CostUSD  float64             `json:"cost_usd"`
}

var doubtTriageSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","items":{
	"type":"object","additionalProperties":false,"required":["question_id","about"],"properties":{
	"question_id":{"type":"string"},
	"about":{"type":"string","enum":["identity","detail"]}}}}}}`)

// TriageOutfitDoubts sorts the open decision questions of outfit readings
// made before readers labeled their doubts. The first reader reads each
// question's text: one about which piece it is stays a decision; one about
// only a fact of a piece the reader was sure of has its piece recorded as the
// reader proposed, and becomes a detail question.
func (reviewer *Reviewer) TriageOutfitDoubts(ctx context.Context, actor string) (DoubtTriage, error) {
	store := reviewer.Store
	if actor == "" {
		return DoubtTriage{}, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	ids, err := stringColumn(store.db, `SELECT owner_questions.id FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
		JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id
		WHERE owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision' AND owner_questions.asked_by = '' AND photo_reviews.reading = 'outfit' AND sightings.status = 'proposed'
		ORDER BY owner_questions.seq`)
	if err != nil {
		return DoubtTriage{}, err
	}
	result := DoubtTriage{Failures: []map[string]string{}}
	const batchSize = 30
	for start := 0; start < len(ids); start += batchSize {
		batch := ids[start:min(start+batchSize, len(ids))]
		var listing strings.Builder
		questions := map[string]OwnerQuestion{}
		for _, id := range batch {
			question, err := questionByID(store.db, id)
			if err != nil {
				return result, err
			}
			questions[id] = question
			fmt.Fprintf(&listing, "- %s: %s\n", id, question.Question)
		}
		prompt := `Each line is a question a reader asked the owner about a piece of what they wore in a photo. Say for each whether it is about identity (which piece this is: whether it is one already recorded, which of several, a new piece or a known one, whether it is the owner's or part of their outfit at all, what kind of thing it is) or only about a detail (the piece is clear and the question is a fact about it: material, model, brand, size, color name).

` + listing.String()
		answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, "You sort questions. Answer every one.", prompt, doubtTriageSchema, nil, 8000)
		if err != nil {
			return result, fmt.Errorf("sort questions %d to %d: %w", start+1, start+len(batch), err)
		}
		result.CostUSD += answer.CostUSD
		var parsed struct {
			Items []struct {
				QuestionID string `json:"question_id"`
				About      string `json:"about"`
			} `json:"items"`
		}
		if err := json.Unmarshal(answer.Parsed, &parsed); err != nil {
			return result, fmt.Errorf("%s's sorting is not one: %w", answer.Model, err)
		}
		for _, item := range parsed.Items {
			question, inBatch := questions[item.QuestionID]
			if !inBatch {
				continue
			}
			delete(questions, item.QuestionID)
			result.Looked++
			if item.About != "detail" {
				result.Identity++
				continue
			}
			if err := store.recordDetailDoubt(ctx, question, actor); err != nil {
				result.Failures = append(result.Failures, map[string]string{"question_id": question.ID, "error": err.Error()})
				continue
			}
			result.Detail++
		}
		for id := range questions {
			result.Failures = append(result.Failures, map[string]string{"question_id": id, "error": answer.Model + " did not sort it"})
		}
	}
	return result, nil
}

// recordDetailDoubt records a sighting as its reader proposed (the
// question's first choice) and makes its question a detail question.
func (s *Store) recordDetailDoubt(ctx context.Context, question OwnerQuestion, actor string) error {
	if len(question.Choices) == 0 {
		return fmt.Errorf("%s has no choices", question.ID)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	sighting, err := sightingByID(transaction, question.SightingID)
	if err != nil {
		return err
	}
	if sighting.Status != "proposed" {
		return nil // settled meanwhile
	}
	now := s.unixNow()
	if err := s.applyResolution(ctx, transaction, sighting.ID, question.Choices[0].Resolution, "agreement", sighting.PhotoReviewID, now); err != nil {
		return err
	}
	if err := saveRevision(transaction, "owner_questions", "id", question.ID, actor, now); err != nil {
		return err
	}
	keepChoices, _ := json.Marshal([]Choice{keepChoice})
	if _, err := transaction.Exec(`UPDATE owner_questions SET purpose = 'detail', choices = ? WHERE id = ?`, string(keepChoices), question.ID); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'done', finished_at = ? WHERE id = ? AND status = 'waiting_for_owner'
		AND NOT EXISTS (SELECT 1 FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
			WHERE sightings.photo_review_id = photo_reviews.id AND owner_questions.answered_at = 0 AND owner_questions.purpose = 'decision')`, now, sighting.PhotoReviewID); err != nil {
		return err
	}
	if err := s.addToReviewOutfit(transaction, sighting.ID, now); err != nil {
		return err
	}
	return transaction.Commit()
}
