package outfits

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
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
	for _, kindName := range possessions.WearableKinds {
		ofKind, err := s.ListPossessions(possessions.PossessionFilter{Kind: kindName, Limit: 1000})
		if err != nil {
			return "", err
		}
		for _, possession := range ofKind {
			status := possession["status"].(string)
			if status != "owned" && status != "unconfirmed" && status != "lent_out" {
				continue
			}
			fmt.Fprintf(&list, "- %s: %s | %s %s | brand %q model %q | %s\n", possession["id"], possession["name"], kindName,
				review.SummarizeKindRecord(possession[kindName].(map[string]any)), possession["brand"], possession["model"], review.TruncateWords(possession["description"].(string), 30))
		}
	}
	rows, err := s.DB.Query(`SELECT id, label, proposed FROM sightings WHERE status = 'proposed' AND photo_review_id != ? ORDER BY seq`, excludeReviewID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label, proposed string
		if err := rows.Scan(&id, &label, &proposed); err != nil {
			return "", err
		}
		var proposal review.Proposal
		if err := json.Unmarshal([]byte(proposed), &proposal); err != nil {
			return "", fmt.Errorf("%s holds a proposal that is not JSON: %w", id, err)
		}
		if !slices.Contains(possessions.WearableKinds, proposal.Kind) {
			continue
		}
		fmt.Fprintf(&list, "- %s (seen in another photo, waiting on the owner): %s | %s | brand %q | %s\n", id, label, proposal.Kind, proposal.Brand, review.TruncateWords(proposal.Description, 30))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if list.Len() == 0 {
		return "(nothing yet)", nil
	}
	return list.String(), nil
}

func outfitPrompt(face possessions.Box, candidates string) string {
	return fmt.Sprintf(`This photo shows the owner; their face is in the box left %.2f, top %.2f, right %.2f, bottom %.2f (fractions of the photo from the top left). Other people may be in it: describe only what the owner has on.

List every piece the owner is wearing or carrying that can be seen: clothes, shoes, headwear, jewelry, a watch, glasses or sunglasses, a bag. Use only these kinds: %s. Give each a box. Leave out what cannot be seen well enough to tell apart from other pieces later (a collar at the frame edge); when only the face shows, the list is empty.

The owner's wearables already recorded, and pieces from other photos still waiting on the owner:
%s
Where a piece is clearly one listed above, put its id in existing_id: the same color, cut, pattern and marks, not merely the same kind of thing. Where it might be one of them but you cannot tell, or anything else that matters is unsure, write the one question you would ask the owner in doubt, and say in doubt_kind whether it is about which piece this is (identity) or only a fact about a piece you are sure of (detail). Otherwise leave both empty. from_readings stays empty.`,
		face.Left, face.Top, face.Right, face.Bottom, strings.Join(possessions.WearableKinds, ", "), candidates)
}

func (reviewer *Reviewer) reviewOutfit(ctx context.Context, photoReview review.PhotoReview) error {
	store := reviewer.Store
	photo, imageSource, err := reviewer.Photos.ReviewImage(ctx, photoReview.PhotoID)
	if err != nil {
		return err
	}
	face, err := reviewer.Photos.WearerFace(ctx, photoReview.PhotoID, photoReview.WearerID)
	if err != nil {
		return err
	}
	takenAt, err := reviewer.Photos.TakenAt(ctx, photoReview.PhotoID)
	if err != nil {
		return err
	}
	if takenAt == 0 {
		return fmt.Errorf("%w: photo-store does not know when %s was taken, so the outfit has no day", possessions.ErrInvalid, photoReview.PhotoID)
	}
	if reviewer.OwnerLocation == nil {
		return errors.New("the reviewer has no owner time zone")
	}
	wornOn := time.Unix(takenAt, 0).In(reviewer.OwnerLocation).Format("2006-01-02")
	if err := store.SetReviewStatus(photoReview.ID, "reading", imageSource, ""); err != nil {
		return err
	}
	candidates, err := store.wearableCandidateList(photoReview.ID)
	if err != nil {
		return err
	}
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, review.ReaderSystemPrompt(), outfitPrompt(face, candidates), review.ReadingSchema(true),
		[]msg.OneShotImage{{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(photo)}}, 16000)
	if err != nil {
		return fmt.Errorf("reader %s: %w", reviewer.FirstReaderRole, err)
	}
	if err := store.SaveReading(photoReview.ID, 1, reviewer.FirstReaderRole, answer); err != nil {
		return err
	}
	var reading review.FinalReading
	if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
		return fmt.Errorf("%s's answer is not a reading: %w", answer.Model, err)
	}
	matches, err := reviewer.MatchPieces(ctx, photoReview.ID, photo, reading)
	if err != nil {
		return err
	}
	return store.recordOutfitReading(ctx, photoReview.ID, answer.Model, wornOn, reading, matches)
}

// recordOutfitReading writes the sightings of one outfit reading, records
// the pieces the reader was sure of, and makes or finds the outfit.
func (s *Store) recordOutfitReading(ctx context.Context, reviewID, model, wornOn string, reading review.FinalReading, matches []review.PieceMatch) error {
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := s.UnixNow()
	if _, err := transaction.Exec(`UPDATE photo_reviews SET status = 'reconciling', worn_on = ? WHERE id = ?`, wornOn, reviewID); err != nil {
		return err
	}
	openQuestions := 0
	for index := range reading.Items {
		item := &reading.Items[index]
		version := s.VersionFor(item)
		if !slices.Contains(possessions.WearableKinds, version.Kind) {
			continue // the reader was told to list wearables only
		}
		version.Status = "unconfirmed"
		box := item.Box.Clamped()
		if !box.Valid() {
			box = possessions.Box{Left: 0, Top: 0, Right: 1, Bottom: 1}
		}
		sightingID, sequence, err := possessions.NextID(transaction, "sightings", "sighting")
		if err != nil {
			return err
		}
		proposed, _ := json.Marshal(version)
		versions, _ := json.Marshal(map[string]review.Proposal{model: version})
		if _, err := transaction.Exec(`INSERT INTO sightings (id, seq, photo_review_id, label, box_left, box_top, box_right, box_bottom, proposed, versions, agreement, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'single_reader', 'proposed')`,
			sightingID, sequence, reviewID, strings.TrimSpace(item.Label), box.Left, box.Top, box.Right, box.Bottom, string(proposed), string(versions)); err != nil {
			return err
		}
		doubt := strings.TrimSpace(item.Doubt)
		if index < len(matches) && matches[index].Asked {
			asked, err := s.RecordMatchedPiece(ctx, transaction, sightingID, reviewID, item.Label, version, doubt, matches[index], now)
			if err != nil {
				return fmt.Errorf("record %s: %w", sightingID, err)
			}
			if asked {
				openQuestions++
			}
			continue
		}
		if doubt == "" || item.DoubtKind == "detail" {
			if err := s.ApplyResolution(ctx, transaction, sightingID, review.VersionChoice("", version).Resolution, "agreement", reviewID, now); err != nil {
				return fmt.Errorf("record %s: %w", sightingID, err)
			}
			if doubt != "" {
				if err := review.InsertQuestion(transaction, sightingID, fmt.Sprintf("Recorded as %s. To add: %s", version.Summary(), doubt), []review.Choice{review.KeepChoice}, "detail", now); err != nil {
					return err
				}
			}
			continue
		}
		choices := []review.Choice{review.VersionChoice("As "+model+" says", version)}
		if version.ExistingPossessionID != "" || version.ExistingSightingID != "" {
			fresh := version
			fresh.ExistingPossessionID, fresh.ExistingSightingID = "", ""
			choices = append(choices, review.VersionChoice("A piece not recorded yet", fresh))
		}
		choices = append(choices, review.RejectChoice)
		if err := review.InsertQuestion(transaction, sightingID, fmt.Sprintf("%s: %s %s asks: %s", item.Label, version.Summary(), model, doubt), choices, "decision", now); err != nil {
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
func (s *Store) settleReviewOutfit(transaction *possessions.DatabaseTransaction, reviewID string, now int64) error {
	photoReview, err := review.PhotoReviewRow(transaction, reviewID)
	if err != nil {
		return err
	}
	if photoReview.OutfitID != nil {
		return nil
	}
	pieces, err := possessions.StringColumn(transaction, `SELECT possession_id FROM sightings WHERE photo_review_id = ? AND status = 'accepted' AND possession_id IS NOT NULL GROUP BY possession_id ORDER BY MIN(seq)`, reviewID)
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
		if outfitID, err = sameDayOutfit(transaction, photoReview.WornOn, pieces); err != nil {
			return err
		}
		if outfitID != "" {
			if err := possessions.SaveRevision(transaction, "outfits", "id", outfitID, reviewID, now); err != nil {
				return err
			}
			for _, piece := range pieces {
				if _, err := transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position)
					VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM outfit_possessions WHERE outfit_id = ?)) ON CONFLICT DO NOTHING`, outfitID, piece, outfitID); err != nil {
					return err
				}
			}
		}
	}
	if outfitID == "" {
		var sequence int64
		outfitID, sequence, err = possessions.NextID(transaction, "outfits", "outfit")
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at) VALUES (?, ?, '', '', '', 'kept', ?, '', ?, ?, ?)`,
			outfitID, sequence, "Worn in "+photoReview.PhotoID, reviewID, now, now); err != nil {
			return err
		}
		if err := replaceOutfitPossessions(transaction, outfitID, pieces); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?) ON CONFLICT DO NOTHING`, outfitID, photoReview.WornOn); err != nil {
		return err
	}
	_, err = transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id = ?`, outfitID, reviewID)
	return err
}

// sameDayOutfit finds the live outfit worn on the same day that shares the
// most pieces with these, at least one; ties go to the older. A same-day
// outfit with nothing in common is a change of clothes, so it is not taken.
func sameDayOutfit(q possessions.Querier, wornOn string, pieces []string) (string, error) {
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

// outfitWithExactly finds a live outfit holding exactly these possessions.
func outfitWithExactly(q possessions.Querier, pieces []string) (string, error) {
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
		HAVING SUM(CASE WHEN outfit_possessions.possession_id IN (`+placeholders+`) THEN 1 ELSE 0 END) = ? AND COUNT(*) = ?
		ORDER BY outfits.seq LIMIT 1`, arguments...).Scan(&outfitID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return outfitID, err
}

// addToReviewOutfit runs after any sighting is settled. A piece of an
// outfit reading accepted once the reading is done (an owner's answer, or a
// sighting it waited on being settled) joins that reading's outfit.
func (s *Store) addToReviewOutfit(transaction *possessions.DatabaseTransaction, sightingID string, now int64) error {
	sighting, err := review.SightingByID(transaction, sightingID)
	if err != nil {
		return err
	}
	if sighting.Status != "accepted" || sighting.PossessionID == nil {
		return nil
	}
	photoReview, err := review.PhotoReviewRow(transaction, sighting.PhotoReviewID)
	if err != nil {
		return err
	}
	if photoReview.Reading != "outfit" || photoReview.Status == "reconciling" || photoReview.Status == "reading" {
		return nil // settleReviewOutfit gathers the pieces of a reading still being recorded
	}
	if photoReview.OutfitID == nil {
		return s.settleReviewOutfit(transaction, photoReview.ID, now)
	}
	_, err = transaction.Exec(`INSERT INTO outfit_possessions (outfit_id, possession_id, position)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM outfit_possessions WHERE outfit_id = ?)) ON CONFLICT DO NOTHING`, *photoReview.OutfitID, *sighting.PossessionID, *photoReview.OutfitID)
	return err
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
		return DoubtTriage{}, fmt.Errorf("%w: actor is required", possessions.ErrInvalid)
	}
	ids, err := possessions.StringColumn(store.DB, `SELECT owner_questions.id FROM owner_questions JOIN sightings ON sightings.id = owner_questions.sighting_id
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
		questions := map[string]review.OwnerQuestion{}
		for _, id := range batch {
			question, err := review.QuestionByID(store.DB, id)
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
func (s *Store) recordDetailDoubt(ctx context.Context, question review.OwnerQuestion, actor string) error {
	if len(question.Choices) == 0 {
		return fmt.Errorf("%s has no choices", question.ID)
	}
	s.WriteMutex.Lock()
	defer s.WriteMutex.Unlock()
	transaction, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	sighting, err := review.SightingByID(transaction, question.SightingID)
	if err != nil {
		return err
	}
	if sighting.Status != "proposed" {
		return nil // settled meanwhile
	}
	now := s.UnixNow()
	if err := s.ApplyResolution(ctx, transaction, sighting.ID, question.Choices[0].Resolution, "agreement", sighting.PhotoReviewID, now); err != nil {
		return err
	}
	if err := possessions.SaveRevision(transaction, "owner_questions", "id", question.ID, actor, now); err != nil {
		return err
	}
	keepChoices, _ := json.Marshal([]review.Choice{review.KeepChoice})
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
