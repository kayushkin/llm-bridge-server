package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// auditActions answer the wearable audit's questions. They act on the
// records the resolution names, not on the question's sighting, which only
// gives the question a picture.
var auditActions = []string{"keep_link", "merge_possessions", "keep_apart", "combine_outfits", "keep_outfits_apart", "slot_link", "slot_new", "slot_not_mine",
	"order_line_link", "order_line_not_this"}

// isAudit says whether a question is one the wearable audit asked: any
// choice is an audit action or undoes a link first.
func (question OwnerQuestion) isAudit() bool {
	for _, choice := range question.Choices {
		if contains(auditActions, choice.Resolution.Action) || choice.Resolution.UnlinkPossessionID != "" {
			return true
		}
	}
	return false
}

// applyAuditResolution carries out an answer to one of the audit's
// questions.
func (s *Store) applyAuditResolution(transaction *databaseTransaction, question OwnerQuestion, resolution Resolution, actor string, now int64) error {
	switch resolution.Action {
	case "keep_link", "keep_apart", "keep_outfits_apart":
		return nil
	case "merge_possessions":
		if resolution.PossessionID == "" || resolution.IntoPossessionID == "" {
			return fmt.Errorf("%w: merge_possessions needs possession_id and into_possession_id", ErrInvalid)
		}
		_, err := s.mergePossessionInTransaction(transaction, resolution.PossessionID, resolution.IntoPossessionID, actor, "the owner answered "+question.ID, now)
		return err
	case "slot_link", "slot_new", "slot_not_mine":
		return s.applySlotAnswer(transaction, resolution, actor, now)
	case "order_line_link":
		if resolution.PossessionID == "" || resolution.MarketplaceOrderLineID == "" {
			return fmt.Errorf("%w: order_line_link needs possession_id and marketplace_order_line_id", ErrInvalid)
		}
		return s.linkOrderLineInTransaction(context.Background(), transaction, resolution.PossessionID, resolution.MarketplaceOrderLineID,
			resolution.BudgetTransactionID, actor, "the owner answered "+question.ID)
	case "order_line_not_this":
		return nil
	case "combine_outfits":
		if resolution.OutfitID == "" || resolution.IntoOutfitID == "" {
			return fmt.Errorf("%w: combine_outfits needs outfit_id and into_outfit_id", ErrInvalid)
		}
		for _, piece := range resolution.PieceMerges {
			if _, err := s.mergePossessionInTransaction(transaction, piece.DuplicatePossessionID, piece.KeptPossessionID, actor, "the owner answered "+question.ID, now); err != nil {
				return err
			}
		}
		_, err := s.combineOutfitsInTransaction(transaction, resolution.OutfitID, resolution.IntoOutfitID, actor, "the owner answered "+question.ID, now)
		return err
	}
	return fmt.Errorf("%w: %q is not an audit action", ErrInvalid, resolution.Action)
}

// unlinkSighting undoes a sighting's record as a possession: the photo link
// it made goes, the sighting waits again (proposed), and the possession
// leaves the reading's outfit unless another photo of that outfit shows it.
// Each row is saved to revisions first.
func (s *Store) unlinkSighting(transaction *databaseTransaction, sighting Sighting, possessionID, actor string, now int64) error {
	if sighting.Status != "accepted" || sighting.PossessionID == nil || *sighting.PossessionID != possessionID {
		return fmt.Errorf("%w: %s is no longer recorded as %s", ErrConflict, sighting.ID, possessionID)
	}
	link, err := stringColumn(transaction, `SELECT json_build_object('table', 'possession_photos', 'possession_id', possession_id, 'photo_id', photo_id, 'box_left', box_left, 'box_top', box_top,
		'box_right', box_right, 'box_bottom', box_bottom, 'is_cover', is_cover, 'sighting_id', sighting_id, 'linked_by', linked_by, 'linked_at', linked_at)::text
		FROM possession_photos WHERE possession_id = ? AND photo_id = ?`, possessionID, sighting.PhotoID)
	if err != nil {
		return err
	}
	for _, row := range link {
		if _, err := transaction.Exec(`INSERT INTO revisions (record_id, record, changed_by, changed_at) VALUES (?, ?, ?, ?)`, possessionID, row, actor, now); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`DELETE FROM possession_photos WHERE possession_id = ? AND photo_id = ?`, possessionID, sighting.PhotoID); err != nil {
		return err
	}
	if _, err := chooseCover(transaction, possessionID); err != nil {
		return err
	}
	if err := saveRevision(transaction, "sightings", "id", sighting.ID, actor, now); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE sightings SET status = 'proposed', possession_id = NULL, same_as_sighting_id = NULL, decided_by = '', decided_at = 0 WHERE id = ?`, sighting.ID); err != nil {
		return err
	}
	review, err := photoReviewRow(transaction, sighting.PhotoReviewID)
	if err != nil {
		return err
	}
	if review.OutfitID == nil {
		return nil
	}
	var elsewhere int
	if err := transaction.QueryRow(`SELECT COUNT(*) FROM sightings JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id
		WHERE photo_reviews.outfit_id = ? AND sightings.possession_id = ? AND sightings.status = 'accepted' AND sightings.id != ?`, *review.OutfitID, possessionID, sighting.ID).Scan(&elsewhere); err != nil {
		return err
	}
	if elsewhere > 0 {
		return nil
	}
	if err := saveRevision(transaction, "outfits", "id", *review.OutfitID, actor, now); err != nil {
		return err
	}
	_, err = transaction.Exec(`DELETE FROM outfit_possessions WHERE outfit_id = ? AND possession_id = ?`, *review.OutfitID, possessionID)
	return err
}

// markReviewWaiting puts a finished review back to waiting_for_owner when a
// decision question is added to it; AnswerQuestion marks it done again once
// none is open.
func markReviewWaiting(transaction *databaseTransaction, sightingID string) error {
	_, err := transaction.Exec(`UPDATE photo_reviews SET status = 'waiting_for_owner' WHERE status = 'done' AND id = (SELECT photo_review_id FROM sightings WHERE id = ?)`, sightingID)
	return err
}

// askAuditQuestion adds a decision question from the audit and marks its
// review waiting.
func askAuditQuestion(transaction *databaseTransaction, askedBy, sightingID, question string, choices []Choice, now int64) (string, error) {
	if !contains(QuestionAskers, askedBy) {
		return "", fmt.Errorf("%q is not one of %s", askedBy, strings.Join(QuestionAskers, ", "))
	}
	id, err := insertQuestionAnsweringID(transaction, sightingID, question, choices, "decision", now)
	if err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE owner_questions SET asked_by = ? WHERE id = ?`, askedBy, id); err != nil {
		return "", err
	}
	return id, markReviewWaiting(transaction, sightingID)
}

// questionAboutExists says whether any question, open or answered, has a
// choice whose resolution holds every one of these JSON fragments: the
// audit asks about a pair or a link once, and an answer stands.
func questionAboutExists(q querier, fragments ...string) (bool, error) {
	conditions := []string{}
	arguments := []any{}
	for _, fragment := range fragments {
		conditions = append(conditions, "strpos(choices, ?) > 0")
		arguments = append(arguments, fragment)
	}
	var found int
	err := q.QueryRow(`SELECT COUNT(*) FROM owner_questions WHERE `+strings.Join(conditions, " AND "), arguments...).Scan(&found)
	return found > 0, err
}

func jsonField(name, value string) string {
	encoded, _ := json.Marshal(value)
	return fmt.Sprintf(`"%s":%s`, name, encoded)
}

// sightingShowing answers an accepted sighting that recorded a possession,
// the cover's first, for a question to hang on; "" when there is none.
func sightingShowing(q querier, possessionID string) (string, error) {
	var sightingID string
	err := q.QueryRow(`SELECT sighting_id FROM possession_photos WHERE possession_id = ? AND sighting_id IS NOT NULL ORDER BY is_cover DESC, linked_at LIMIT 1`, possessionID).Scan(&sightingID)
	if errors.Is(err, sql.ErrNoRows) {
		err = q.QueryRow(`SELECT id FROM sightings WHERE possession_id = ? AND status = 'accepted' ORDER BY seq LIMIT 1`, possessionID).Scan(&sightingID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sightingID, err
}

// sightingOfOutfit answers an accepted sighting from a reading of the
// outfit; "" when there is none.
func sightingOfOutfit(q querier, outfitID string) (string, error) {
	var sightingID string
	err := q.QueryRow(`SELECT sightings.id FROM sightings JOIN photo_reviews ON photo_reviews.id = sightings.photo_review_id
		WHERE photo_reviews.outfit_id = ? AND sightings.status = 'accepted' ORDER BY sightings.seq LIMIT 1`, outfitID).Scan(&sightingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sightingID, err
}

var choiceInterpretationSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["choice","explanation"],"properties":{
	"choice":{"type":"integer","description":"The index of the choice the owner's words mean, from 0; -1 when they mean none of them"},
	"explanation":{"type":"string","description":"One sentence: what you took the owner to mean"}}}`)

// interpretChoice asks the first reader which of an audit question's
// choices the owner's words mean. Words that mean none of them are refused,
// so nothing is written on a guess.
func (reviewer *Reviewer) interpretChoice(ctx context.Context, question OwnerQuestion, text string) (Resolution, error) {
	var listing strings.Builder
	for index, choice := range question.Choices {
		fmt.Fprintf(&listing, "%d. %s\n", index, choice.Label)
	}
	prompt := fmt.Sprintf("The owner was asked:\n%s\n\nThe choices were:\n%s\nThe owner answered in their own words:\n%q\n\nWhich choice do they mean?", question.Question, listing.String(), text)
	answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, "You read an owner's answer to a question about their inventory.", prompt, choiceInterpretationSchema, nil, 2000)
	if err != nil {
		return Resolution{}, fmt.Errorf("interpret the answer: %w", err)
	}
	var interpretation struct {
		Choice      int    `json:"choice"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal(answer.Parsed, &interpretation); err != nil {
		return Resolution{}, fmt.Errorf("%s's interpretation is not one: %w", answer.Model, err)
	}
	if interpretation.Choice < 0 || interpretation.Choice >= len(question.Choices) {
		return Resolution{}, fmt.Errorf("%w: %s read the answer as none of the choices (%s); pick one", ErrInvalid, answer.Model, interpretation.Explanation)
	}
	resolution := question.Choices[interpretation.Choice].Resolution
	resolution.Explanation = fmt.Sprintf("%s read the answer as choice %d: %s", answer.Model, interpretation.Choice, interpretation.Explanation)
	return resolution, nil
}
