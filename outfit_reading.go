package possessionstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
Where a piece is clearly one listed above, put its id in existing_id: the same color, cut, pattern and marks, not merely the same kind of thing. Where it might be one of them but you cannot tell, or anything else that matters is unsure, write the one question you would ask the owner in doubt; otherwise leave doubt empty. from_readings stays empty.`,
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
	return store.recordOutfitReading(ctx, review.ID, answer.Model, wornOn, reading)
}

// recordOutfitReading writes the sightings of one outfit reading, records
// the pieces the reader was sure of, and makes or finds the outfit.
func (s *Store) recordOutfitReading(ctx context.Context, reviewID, model, wornOn string, reading finalReading) error {
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
		if doubt == "" {
			if err := s.applyResolution(ctx, transaction, sightingID, versionChoice("", version).Resolution, "agreement", reviewID, now); err != nil {
				return fmt.Errorf("record %s: %w", sightingID, err)
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
