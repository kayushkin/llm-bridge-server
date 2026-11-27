package possessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// createPieceFromSlot records a new unconfirmed possession from what a slot
// says it is. It has no photos of its own until a slot is linked to it.
func (s *Store) createPieceFromSlot(ctx context.Context, slot Slot, actor string) (string, error) {
	closed, err := groupClosed(s.db, slot.Group)
	if err != nil {
		return "", err
	}
	if closed {
		return "", fmt.Errorf("%w: the owner closed the %s group, so nothing in it is recorded as new", ErrConflict, slot.Group)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer transaction.Rollback()
	proposal := slot.Proposed
	proposal.ExistingPossessionID, proposal.ExistingSightingID = "", ""
	proposal.Status = "unconfirmed"
	id, err := s.createPossessionInTransaction(ctx, transaction, proposal.possessionFields(), actor)
	if err != nil {
		return "", err
	}
	return id, transaction.Commit()
}

// linkSlotBestGuess links a slot to its most likely piece: a sighting on
// each of its photos, recorded as that piece, the photos linked to it, and
// one optional question whose choices are its ranked pieces. Without link
// (no reader picked any piece) the slot stays unlinked, decided_by
// no_pick, its sightings proposed, with the same candidates and question.
// It answers the question's id.
func (s *Store) linkSlotBestGuess(ctx context.Context, slot Slot, candidates []SlotCandidate, closed, link bool) (string, error) {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer transaction.Rollback()
	current, err := slotByID(transaction, slot.ID)
	if err != nil {
		return "", err
	}
	if current.DecidedBy != "" {
		return "", fmt.Errorf("%w: %s was decided meanwhile by %s", ErrConflict, slot.ID, current.DecidedBy)
	}
	now := s.unixNow()
	best := candidates[0].PossessionID
	if err := requireLivePossession(transaction, best); err != nil {
		return "", err
	}
	linked, decidedBy := best, "best_guess"
	if !link {
		linked, decidedBy = "", "no_pick"
	}
	firstSighting := ""
	for _, photo := range current.Photos {
		sightingID, err := s.slotSighting(transaction, current, photo, linked, now)
		if err != nil {
			return "", err
		}
		if firstSighting == "" {
			firstSighting = sightingID
		}
	}
	encoded, _ := json.Marshal(candidates)
	if err := saveRevision(transaction, "outfit_slots", "id", slot.ID, "best_guess", now); err != nil {
		return "", err
	}
	var possessionID any
	if link {
		possessionID = best
	}
	if _, err := transaction.Exec(`UPDATE outfit_slots SET possession_id = ?, decided_by = ?, candidates = ?, updated_at = ? WHERE id = ?`, possessionID, decidedBy, string(encoded), now, slot.ID); err != nil {
		return "", err
	}
	choices := []Choice{}
	for _, candidate := range candidates {
		view, err := possessionView(transaction, candidate.PossessionID, false)
		if err != nil {
			return "", err
		}
		choices = append(choices, Choice{Label: fmt.Sprintf("It is %s (%s), %.0f%% likely", candidate.PossessionID, view["name"], candidate.Likelihood*100),
			Resolution: Resolution{Action: "slot_link", SlotID: slot.ID, PossessionID: candidate.PossessionID}})
	}
	if !closed {
		choices = append(choices, Choice{Label: "A different piece, not recorded yet", Resolution: Resolution{Action: "slot_new", SlotID: slot.ID}})
	}
	choices = append(choices, Choice{Label: "Not mine, or not worn here", Resolution: Resolution{Action: "slot_not_mine", SlotID: slot.ID}})
	pictures := []string{}
	for _, candidate := range candidates {
		pictures = append(pictures, "/possessions/"+candidate.PossessionID+"/image")
	}
	question := fmt.Sprintf("Linked as the best guess: the %s on %s is %s. Pictures of each choice: %s.", current.Slot, lookDay(transaction, current.LookID), best, strings.Join(pictures, ", "))
	if !link {
		question = fmt.Sprintf("Neither reader matched the %s on %s to a recorded piece, so it is not linked. Is it one of these? Pictures of each choice: %s.", current.Slot, lookDay(transaction, current.LookID), strings.Join(pictures, ", "))
	}
	questionID, err := insertQuestionAnsweringID(transaction, firstSighting, question, choices, "detail", now)
	if err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE owner_questions SET asked_by = 'outfit_slots' WHERE id = ?`, questionID); err != nil {
		return "", err
	}
	if err := s.placeLook(transaction, current.LookID, "best_guess", now); err != nil {
		return "", err
	}
	return questionID, transaction.Commit()
}

func lookDay(q querier, lookID string) string {
	var day string
	if err := q.QueryRow(`SELECT worn_on FROM outfit_looks WHERE id = ?`, lookID).Scan(&day); err != nil {
		return lookID
	}
	return day
}

// slotSighting makes the sighting of a slot on one photo, recorded as a
// possession, and links the photo to it; with no possession ("") the
// sighting waits, proposed.
func (s *Store) slotSighting(transaction *databaseTransaction, slot Slot, photo SlotPhoto, possessionID string, now int64) (string, error) {
	var reviewID string
	if err := transaction.QueryRow(`SELECT photo_review_id FROM outfit_look_photos WHERE look_id = ? AND photo_id = ?`, slot.LookID, photo.PhotoID).Scan(&reviewID); err != nil {
		return "", fmt.Errorf("%s's photo %s is not in its look: %w", slot.ID, photo.PhotoID, err)
	}
	sightingID, sequence, err := nextID(transaction, "sightings", "sighting")
	if err != nil {
		return "", err
	}
	proposed := slot.Proposed
	proposed.ExistingPossessionID = possessionID
	encoded, _ := json.Marshal(proposed)
	status, decidedBy, decidedAt, linked := "accepted", "best_guess", now, any(possessionID)
	if possessionID == "" {
		status, decidedBy, decidedAt, linked = "proposed", "", int64(0), nil
	}
	if _, err := transaction.Exec(`INSERT INTO sightings (id, seq, photo_review_id, label, box_left, box_top, box_right, box_bottom, proposed, versions, agreement, status, possession_id, decided_by, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 'best_guess', ?, ?, ?, ?)`,
		sightingID, sequence, reviewID, slot.Slot, photo.Box.Left, photo.Box.Top, photo.Box.Right, photo.Box.Bottom, string(encoded), status, linked, decidedBy, decidedAt); err != nil {
		return "", err
	}
	if possessionID == "" {
		if _, err := transaction.Exec(`UPDATE outfit_slot_photos SET sighting_id = ? WHERE slot_id = ? AND photo_id = ?`, sightingID, slot.ID, photo.PhotoID); err != nil {
			return "", err
		}
		return sightingID, nil
	}
	box := photo.Box
	if err := s.linkPhotoInTransaction(context.Background(), transaction, possessionID, PhotoLink{PhotoID: photo.PhotoID, BoxLeft: &box.Left, BoxTop: &box.Top, BoxRight: &box.Right, BoxBottom: &box.Bottom,
		SightingID: &sightingID, LinkedBy: "best_guess"}, now); err != nil {
		return "", err
	}
	if _, err := transaction.Exec(`UPDATE outfit_slot_photos SET sighting_id = ? WHERE slot_id = ? AND photo_id = ?`, sightingID, slot.ID, photo.PhotoID); err != nil {
		return "", err
	}
	return sightingID, nil
}

// applySlotAnswer relinks a slot as the owner answered: to a candidate, to
// a new piece, or to nothing.
func (s *Store) applySlotAnswer(transaction *databaseTransaction, resolution Resolution, actor string, now int64) error {
	slot, err := slotByID(transaction, resolution.SlotID)
	if err != nil {
		return err
	}
	switch resolution.Action {
	case "slot_link":
		possessionID, err := mergedInto(transaction, resolution.PossessionID)
		if err != nil {
			return err
		}
		return s.relinkSlot(transaction, slot, possessionID, actor, now)
	case "slot_new":
		closed, err := groupClosed(transaction, slot.Group)
		if err != nil {
			return err
		}
		if closed {
			return fmt.Errorf("%w: the owner closed the %s group, so nothing in it is recorded as new; pick one of its members or not mine", ErrConflict, slot.Group)
		}
		proposal := slot.Proposed
		proposal.ExistingPossessionID, proposal.ExistingSightingID = "", ""
		proposal.Status = "unconfirmed"
		created, err := s.createPossessionInTransaction(context.Background(), transaction, proposal.possessionFields(), actor)
		if err != nil {
			return err
		}
		return s.relinkSlot(transaction, slot, created, actor, now)
	}
	return s.relinkSlot(transaction, slot, "", actor, now)
}

// relinkSlot points a slot, its sightings and its photos at another
// possession ("" for none), then puts its look in the outfit that holds its
// pieces. Each row is saved to revisions first.
func (s *Store) relinkSlot(transaction *databaseTransaction, slot Slot, possessionID, actor string, now int64) error {
	if err := saveRevision(transaction, "outfit_slots", "id", slot.ID, actor, now); err != nil {
		return err
	}
	for _, photo := range slot.Photos {
		if photo.SightingID == nil {
			if possessionID == "" {
				continue
			}
			if _, err := s.slotSighting(transaction, slot, photo, possessionID, now); err != nil {
				return err
			}
			continue
		}
		sightingID := *photo.SightingID
		if err := saveRevision(transaction, "sightings", "id", sightingID, actor, now); err != nil {
			return err
		}
		if slot.PossessionID != nil {
			if _, err := transaction.Exec(`DELETE FROM possession_photos WHERE possession_id = ? AND photo_id = ? AND sighting_id = ?`, *slot.PossessionID, photo.PhotoID, sightingID); err != nil {
				return err
			}
			if _, err := chooseCover(transaction, *slot.PossessionID); err != nil {
				return err
			}
		}
		if possessionID == "" {
			if _, err := transaction.Exec(`UPDATE sightings SET status = 'rejected', possession_id = NULL, decided_by = ?, decided_at = ? WHERE id = ?`, actor, now, sightingID); err != nil {
				return err
			}
			continue
		}
		if _, err := transaction.Exec(`UPDATE sightings SET status = 'accepted', possession_id = ?, decided_by = ?, decided_at = ? WHERE id = ?`, possessionID, actor, now, sightingID); err != nil {
			return err
		}
		box := photo.Box
		if err := s.linkPhotoInTransaction(context.Background(), transaction, possessionID, PhotoLink{PhotoID: photo.PhotoID, BoxLeft: &box.Left, BoxTop: &box.Top, BoxRight: &box.Right, BoxBottom: &box.Bottom,
			SightingID: &sightingID, LinkedBy: actor}, now); err != nil {
			return err
		}
	}
	var linked any
	if possessionID != "" {
		linked = possessionID
	}
	if _, err := transaction.Exec(`UPDATE outfit_slots SET possession_id = ?, decided_by = ?, updated_at = ? WHERE id = ?`, linked, actor, now, slot.ID); err != nil {
		return err
	}
	return s.placeLook(transaction, slot.LookID, actor, now)
}

// lookPieces answers the possessions a look's slots are linked to.
func lookPieces(q querier, lookID string) ([]string, error) {
	return stringColumn(q, `SELECT DISTINCT possession_id FROM outfit_slots WHERE look_id = ? AND possession_id IS NOT NULL ORDER BY possession_id`, lookID)
}

// placeLook keeps a look's outfit right after one of its slots changed,
// once outfits are rebuilt from looks. An outfit worn only as this look
// holds the look's pieces. One shared with other looks keeps the look when
// its main pieces still match theirs (identityMatch), and then holds the
// union of every look's pieces. Otherwise the look moves, with its day and
// readings, to the outfit of other looks it matches best, or a new one, and
// both outfits hold the union of their looks' pieces.
func (s *Store) placeLook(transaction *databaseTransaction, lookID, actor string, now int64) error {
	look, err := lookByID(transaction, lookID)
	if err != nil {
		return err
	}
	if look.OutfitID == nil {
		return nil
	}
	pieces, err := lookPieces(transaction, lookID)
	if err != nil {
		return err
	}
	current := *look.OutfitID
	others, err := outfitLooks(transaction, current, lookID)
	if err != nil {
		return err
	}
	if err := saveRevision(transaction, "outfits", "id", current, actor, now); err != nil {
		return err
	}
	if others.looks == 0 {
		return replaceOutfitPossessions(transaction, current, pieces)
	}
	identity, err := looksIdentity(transaction, lookID)
	if err != nil {
		return err
	}
	if _, same := identityMatch(identity, others.identity, contains(others.days, look.WornOn)); same {
		return refreshOutfitPieces(transaction, current)
	}
	outfitIDs, err := stringColumn(transaction, `SELECT outfit_looks.outfit_id FROM outfit_looks JOIN outfits ON outfits.id = outfit_looks.outfit_id
		WHERE outfits.deleted_at = 0 AND outfits.status != 'rejected' AND outfit_looks.outfit_id != ? GROUP BY outfit_looks.outfit_id, outfits.seq ORDER BY outfits.seq`, current)
	if err != nil {
		return err
	}
	candidates := []*identityCandidate{}
	for _, outfitID := range outfitIDs {
		candidate, err := outfitLooks(transaction, outfitID, "")
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	target := ""
	if joined := bestIdentityMatch(identity, look.WornOn, candidates); joined != nil {
		target = joined.outfitID
		if err := saveRevision(transaction, "outfits", "id", target, actor, now); err != nil {
			return err
		}
	} else {
		var sequence int64
		if target, sequence, err = nextID(transaction, "outfits", "outfit"); err != nil {
			return err
		}
		if _, err := transaction.Exec(`INSERT INTO outfits (id, seq, name, occasion, weather, status, reasoning, preview_image_path, created_by, created_at, updated_at) VALUES (?, ?, '', '', '', 'kept', ?, '', ?, ?, ?)`,
			target, sequence, "Split from "+current+" when a piece of "+lookID+" changed", actor, now, now); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec(`UPDATE outfit_looks SET outfit_id = ?, updated_at = ? WHERE id = ?`, target, now, lookID); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE photo_reviews SET outfit_id = ? WHERE id IN (SELECT photo_review_id FROM outfit_look_photos WHERE look_id = ?)`, target, lookID); err != nil {
		return err
	}
	if _, err := transaction.Exec(`INSERT INTO outfit_wearings (outfit_id, worn_on) VALUES (?, ?) ON CONFLICT DO NOTHING`, target, look.WornOn); err != nil {
		return err
	}
	if _, err := transaction.Exec(`DELETE FROM outfit_wearings WHERE outfit_id = ? AND worn_on = ? AND NOT EXISTS (SELECT 1 FROM outfit_looks WHERE outfit_id = ? AND worn_on = ?)`, current, look.WornOn, current, look.WornOn); err != nil {
		return err
	}
	if err := refreshOutfitPieces(transaction, current); err != nil {
		return err
	}
	return refreshOutfitPieces(transaction, target)
}

// refreshOutfitPieces makes an outfit hold the union of its looks' pieces.
func refreshOutfitPieces(transaction *databaseTransaction, outfitID string) error {
	pieces, err := outfitPiecesFromLooks(transaction, outfitID)
	if err != nil {
		return err
	}
	return replaceOutfitPossessions(transaction, outfitID, pieces)
}

// SlotImage cuts a slot from the full-size photo where its box is largest,
// as identification shows it.
func (reviewer *Reviewer) SlotImage(ctx context.Context, slotID string) ([]byte, error) {
	slot, err := slotByID(reviewer.Store.db, slotID)
	if err != nil {
		return nil, err
	}
	if len(slot.Photos) == 0 {
		return nil, fmt.Errorf("%w: %s has no photo", ErrNotFound, slotID)
	}
	photo := slot.largestPhoto()
	picture, _, err := reviewer.Photos.DetailImage(ctx, photo.PhotoID)
	if err != nil {
		return nil, err
	}
	return detailCrop(picture, photo.Box, smallPieceGroup(slot.Group))
}

// mergedInto answers the live possession a choice means: the possession
// itself, or the one it was merged into (and so on) since the choice was
// offered. A possession deleted without a merge is not found.
func mergedInto(q querier, possessionID string) (string, error) {
	for id, steps := possessionID, 0; steps < 100; steps++ {
		var deletedAt int64
		var into sql.NullString
		err := q.QueryRow(`SELECT deleted_at, merged_into_id FROM possessions WHERE id = ?`, id).Scan(&deletedAt, &into)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: no possession %s", ErrNotFound, id)
		}
		if err != nil {
			return "", err
		}
		if deletedAt == 0 {
			return id, nil
		}
		if !into.Valid {
			return "", fmt.Errorf("%w: no possession %s", ErrNotFound, id)
		}
		id = into.String
	}
	return "", fmt.Errorf("%s's merges run in a loop", possessionID)
}

// unlinkSlot takes a best-guess link no reader picked off a slot: its
// sightings and photo links go, and it is decided_by no_pick.
func (s *Store) unlinkSlot(ctx context.Context, slot Slot, actor string) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	if err := s.relinkSlot(transaction, slot, "", actor, now); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE outfit_slots SET decided_by = 'no_pick' WHERE id = ?`, slot.ID); err != nil {
		return err
	}
	return transaction.Commit()
}
