package photostore

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

func formatFaceGroupID(seq int64) string { return fmt.Sprintf("face_group_%06d", seq) }

func formatFaceGroupGuessID(seq int64) string { return fmt.Sprintf("face_group_guess_%06d", seq) }

// faceGroupSampleSize is how many faces GET /face-groups shows of each group.
const faceGroupSampleSize = 12

// checkFaceGroupMembers refuses a face that is missing, retired, on a deleted
// photo, found by another model, already in a group, or named twice.
func checkFaceGroupMembers(tx *sql.Tx, model string, members []NewFaceGroupMember) error {
	seen := map[string]bool{}
	for index, member := range members {
		if seen[member.FaceID] {
			return fmt.Errorf("%w: member %d: %s is named twice", ErrInvalid, index, member.FaceID)
		}
		seen[member.FaceID] = true
		if member.Similarity < -1 || member.Similarity > 1 {
			return fmt.Errorf("%w: member %d: similarity is between -1 and 1", ErrInvalid, index)
		}
		var faceModel string
		var retiredAt, deletedAt int64
		err := tx.QueryRow(`SELECT faces.model, faces.retired_at, photos.deleted_at FROM faces JOIN photos ON photos.id = faces.photo_id
			WHERE faces.id = ?`, member.FaceID).Scan(&faceModel, &retiredAt, &deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: member %d: no face %s", ErrInvalid, index, member.FaceID)
		} else if err != nil {
			return err
		}
		switch {
		case faceModel != model:
			return fmt.Errorf("%w: member %d: %s was found by %s, not %s", ErrInvalid, index, member.FaceID, faceModel, model)
		case retiredAt != 0:
			return fmt.Errorf("%w: member %d: %s was retired", ErrInvalid, index, member.FaceID)
		case deletedAt != 0:
			return fmt.Errorf("%w: member %d: %s is on a deleted photo", ErrInvalid, index, member.FaceID)
		}
		var inGroup string
		err = tx.QueryRow(`SELECT group_id FROM face_group_members WHERE face_id = ? AND released_at = 0`, member.FaceID).Scan(&inGroup)
		if err == nil {
			return fmt.Errorf("%w: member %d: %s is already in %s", ErrConflict, index, member.FaceID, inGroup)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func insertFaceGroupMembers(tx *sql.Tx, groupID string, members []NewFaceGroupMember, at int64) error {
	for _, member := range members {
		// A face taken out of this group before and found to belong again
		// replaces its released row.
		if _, err := tx.Exec(`INSERT INTO face_group_members (group_id, face_id, similarity, added_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (group_id, face_id) DO UPDATE SET similarity = excluded.similarity, added_at = excluded.added_at, released_at = 0, released_by = ''`,
			groupID, member.FaceID, member.Similarity, at); err != nil {
			return err
		}
	}
	return nil
}

// CreateFaceGroup answers POST /face-groups.
func (s *Store) CreateFaceGroup(group NewFaceGroup) (FaceGroup, error) {
	if strings.TrimSpace(group.Model) == "" {
		return FaceGroup{}, fmt.Errorf("%w: model is required: the face model whose faces these are", ErrInvalid)
	}
	if strings.TrimSpace(group.MadeBy) == "" {
		return FaceGroup{}, fmt.Errorf("%w: made_by is required: what grouped these faces", ErrInvalid)
	}
	if len(group.Members) == 0 {
		return FaceGroup{}, fmt.Errorf("%w: a group needs at least one face", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FaceGroup{}, err
	}
	defer tx.Rollback()
	if err := checkFaceGroupMembers(tx, group.Model, group.Members); err != nil {
		return FaceGroup{}, err
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM face_groups`).Scan(&seq); err != nil {
		return FaceGroup{}, err
	}
	id, at := formatFaceGroupID(seq), now()
	if _, err := tx.Exec(`INSERT INTO face_groups (id, seq, model, status, made_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, seq, group.Model, FaceGroupOpen, group.MadeBy, at, at); err != nil {
		return FaceGroup{}, err
	}
	if err := insertFaceGroupMembers(tx, id, group.Members, at); err != nil {
		return FaceGroup{}, err
	}
	if err := tx.Commit(); err != nil {
		return FaceGroup{}, err
	}
	return s.FaceGroupByID(id, false)
}

// AddFaceGroupMembers answers POST /face-groups/{id}/members: faces a grouper
// found to belong to a group still open.
func (s *Store) AddFaceGroupMembers(id string, addition FaceGroupMembersAddition) (FaceGroup, error) {
	if len(addition.Members) == 0 {
		return FaceGroup{}, fmt.Errorf("%w: members is empty", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FaceGroup{}, err
	}
	defer tx.Rollback()
	var model, status string
	if err := tx.QueryRow(`SELECT model, status FROM face_groups WHERE id = ?`, id).Scan(&model, &status); errors.Is(err, sql.ErrNoRows) {
		return FaceGroup{}, fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	} else if err != nil {
		return FaceGroup{}, err
	}
	if status != FaceGroupOpen {
		return FaceGroup{}, fmt.Errorf("%w: %s is %s; only an open group takes faces", ErrConflict, id, status)
	}
	if err := checkFaceGroupMembers(tx, model, addition.Members); err != nil {
		return FaceGroup{}, err
	}
	at := now()
	if err := insertFaceGroupMembers(tx, id, addition.Members, at); err != nil {
		return FaceGroup{}, err
	}
	if _, err := tx.Exec(`UPDATE face_groups SET updated_at = ? WHERE id = ?`, at, id); err != nil {
		return FaceGroup{}, err
	}
	if err := tx.Commit(); err != nil {
		return FaceGroup{}, err
	}
	return s.FaceGroupByID(id, false)
}

// AddFaceGroupGuess answers POST /face-groups/{id}/guesses.
func (s *Store) AddFaceGroupGuess(id string, proposal FaceGroupGuessProposal) (FaceGroupGuess, error) {
	if proposal.PersonID != "" && !personIDPattern.MatchString(proposal.PersonID) {
		return FaceGroupGuess{}, fmt.Errorf("%w: person_id must be people-store's id (person_000001) or \"\" for nobody in it, got %q", ErrInvalid, proposal.PersonID)
	}
	if strings.TrimSpace(proposal.ProposedBy) == "" {
		return FaceGroupGuess{}, fmt.Errorf("%w: proposed_by is required: who or what guessed", ErrInvalid)
	}
	if proposal.Confidence < 0 || proposal.Confidence > 1 {
		return FaceGroupGuess{}, fmt.Errorf("%w: confidence is between 0 and 1", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FaceGroupGuess{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM face_groups WHERE id = ?`, id).Scan(&exists); err != nil {
		return FaceGroupGuess{}, err
	}
	if exists == 0 {
		return FaceGroupGuess{}, fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM face_group_guesses`).Scan(&seq); err != nil {
		return FaceGroupGuess{}, err
	}
	guess := FaceGroupGuess{ID: formatFaceGroupGuessID(seq), GroupID: id, PersonID: proposal.PersonID, Confidence: proposal.Confidence,
		Evidence: proposal.Evidence, ProposedBy: proposal.ProposedBy, CreatedAt: now()}
	if _, err := tx.Exec(`INSERT INTO face_group_guesses (id, seq, group_id, person_id, confidence, evidence, proposed_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, guess.ID, seq, id, guess.PersonID, guess.Confidence, guess.Evidence, guess.ProposedBy, guess.CreatedAt); err != nil {
		return FaceGroupGuess{}, err
	}
	return guess, tx.Commit()
}

// FaceGroupFilter selects groups for GET /face-groups.
type FaceGroupFilter struct {
	Status   string
	PersonID string
	Limit    int
	Offset   int
}

// FaceGroups answers GET /face-groups: groups with their counts, samples and
// guesses, the most photos first.
func (s *Store) FaceGroups(filter FaceGroupFilter) ([]FaceGroup, error) {
	where, args := []string{"1 = 1"}, []any{}
	if filter.Status != "" {
		if !slices.Contains(FaceGroupStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(FaceGroupStatuses, ", "))
		}
		where, args = append(where, "face_groups.status = ?"), append(args, filter.Status)
	}
	if filter.PersonID != "" {
		where, args = append(where, "face_groups.person_id = ?"), append(args, filter.PersonID)
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumLimit {
		limit = 100
	}
	args = append(args, limit, max(filter.Offset, 0))
	rows, err := s.db.Query(`SELECT face_groups.id FROM face_groups
		LEFT JOIN (SELECT face_group_members.group_id, COUNT(DISTINCT faces.photo_id) AS photo_count
			FROM face_group_members JOIN faces ON faces.id = face_group_members.face_id JOIN photos ON photos.id = faces.photo_id
			WHERE face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
			GROUP BY face_group_members.group_id) counted ON counted.group_id = face_groups.id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY COALESCE(counted.photo_count, 0) DESC, face_groups.seq LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groups := make([]FaceGroup, 0, len(ids))
	for _, id := range ids {
		group, err := s.FaceGroupByID(id, false)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// FaceGroupByID answers GET /face-groups/{id}; withMembers fills Members.
func (s *Store) FaceGroupByID(id string, withMembers bool) (FaceGroup, error) {
	var group FaceGroup
	err := s.db.QueryRow(`SELECT id, model, status, person_id, made_by, decided_by, decided_at, created_at, updated_at FROM face_groups WHERE id = ?`, id).
		Scan(&group.ID, &group.Model, &group.Status, &group.PersonID, &group.MadeBy, &group.DecidedBy, &group.DecidedAt, &group.CreatedAt, &group.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FaceGroup{}, fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	} else if err != nil {
		return FaceGroup{}, err
	}
	members, err := s.faceGroupMembers(id)
	if err != nil {
		return FaceGroup{}, err
	}
	photos := map[string]bool{}
	onePerPhoto := []string{}
	for _, member := range members {
		if member.TakenAt != 0 && (group.FirstTakenAt == 0 || member.TakenAt < group.FirstTakenAt) {
			group.FirstTakenAt = member.TakenAt
		}
		group.LastTakenAt = max(group.LastTakenAt, member.TakenAt)
		if !photos[member.PhotoID] {
			onePerPhoto = append(onePerPhoto, member.FaceID)
		}
		photos[member.PhotoID] = true
	}
	// Samples run evenly from the most typical face to the least, so a face
	// that is someone else, which sits at the far end, shows among them.
	group.SampleFaceIDs = evenlyFrom(onePerPhoto, faceGroupSampleSize)
	group.FaceCount, group.PhotoCount = len(members), len(photos)
	if withMembers {
		group.Members = members
	}
	group.Guesses, err = s.faceGroupGuesses(id)
	return group, err
}

// evenlyFrom picks up to count items spread evenly over a list, its first and
// last included.
func evenlyFrom(items []string, count int) []string {
	if len(items) <= count {
		return append([]string{}, items...)
	}
	picked := make([]string, 0, count)
	for index := 0; index < count; index++ {
		picked = append(picked, items[index*(len(items)-1)/(count-1)])
	}
	return picked
}

// faceGroupMembers reads a group's live faces, the most typical first.
func (s *Store) faceGroupMembers(id string) ([]FaceGroupMember, error) {
	rows, err := s.db.Query(`SELECT faces.id, faces.photo_id, face_group_members.similarity, photos.taken_at, photos.folder
		FROM face_group_members JOIN faces ON faces.id = face_group_members.face_id JOIN photos ON photos.id = faces.photo_id
		WHERE face_group_members.group_id = ? AND face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
		ORDER BY face_group_members.similarity DESC, faces.seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []FaceGroupMember{}
	for rows.Next() {
		var member FaceGroupMember
		if err := rows.Scan(&member.FaceID, &member.PhotoID, &member.Similarity, &member.TakenAt, &member.Folder); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Store) faceGroupGuesses(id string) ([]FaceGroupGuess, error) {
	rows, err := s.db.Query(`SELECT id, group_id, person_id, confidence, evidence, proposed_by, created_at FROM face_group_guesses
		WHERE group_id = ? ORDER BY seq DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	guesses := []FaceGroupGuess{}
	for rows.Next() {
		var guess FaceGroupGuess
		if err := rows.Scan(&guess.ID, &guess.GroupID, &guess.PersonID, &guess.Confidence, &guess.Evidence, &guess.ProposedBy, &guess.CreatedAt); err != nil {
			return nil, err
		}
		guesses = append(guesses, guess)
	}
	return guesses, rows.Err()
}

// DecideFaceGroup answers POST /face-groups/{id}/decision. It takes the
// excluded faces out of the group, then sets its status. Naming it confirms
// each remaining face as the person, through the same rules as POST
// /photo-people: a person's rejection holds, and a confirmed link stays. A
// suggestion of anyone else on those faces is rejected, since a face is one
// person. An open group can be decided; a group left not_known or mixed can
// still be named; a named group can only be named as someone else, which
// rejects the first person's links on its faces and confirms the new one's.
// A single wrong face on it is fixed with PATCH /photo-people.
func (s *Store) DecideFaceGroup(id string, decision FaceGroupDecision) (FaceGroupDecisionResult, error) {
	if !slices.Contains([]string{FaceGroupNamed, FaceGroupNotKnown, FaceGroupMixed}, decision.Status) {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: a decision's status is named, not_known or mixed, got %q", ErrInvalid, decision.Status)
	}
	if strings.TrimSpace(decision.DecidedBy) == "" {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: decided_by is required", ErrInvalid)
	}
	if decision.Status == FaceGroupNamed && !personIDPattern.MatchString(decision.PersonID) {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: naming a group needs person_id, people-store's id (person_000001), got %q", ErrInvalid, decision.PersonID)
	}
	if decision.Status != FaceGroupNamed && decision.PersonID != "" {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: person_id goes only with status named", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FaceGroupDecisionResult{}, err
	}
	defer tx.Rollback()
	var status, previousPerson string
	if err := tx.QueryRow(`SELECT status, person_id FROM face_groups WHERE id = ?`, id).Scan(&status, &previousPerson); errors.Is(err, sql.ErrNoRows) {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	} else if err != nil {
		return FaceGroupDecisionResult{}, err
	}
	renaming := status == FaceGroupNamed
	if renaming && (decision.Status != FaceGroupNamed || decision.PersonID == previousPerson || len(decision.ExcludedFaceIDs) > 0) {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: %s is named as %s; it can only be named as someone else, with no faces left out", ErrConflict, id, previousPerson)
	}
	if status != FaceGroupOpen && decision.Status != FaceGroupNamed {
		return FaceGroupDecisionResult{}, fmt.Errorf("%w: %s is %s; it can still be named, not decided again", ErrConflict, id, status)
	}
	at := now()
	result := FaceGroupDecisionResult{}
	for _, faceID := range decision.ExcludedFaceIDs {
		released, err := tx.Exec(`UPDATE face_group_members SET released_at = ?, released_by = ? WHERE group_id = ? AND face_id = ? AND released_at = 0`,
			at, decision.DecidedBy, id, faceID)
		if err != nil {
			return FaceGroupDecisionResult{}, err
		}
		if count, _ := released.RowsAffected(); count == 0 {
			return FaceGroupDecisionResult{}, fmt.Errorf("%w: %s is not in %s", ErrInvalid, faceID, id)
		}
		result.Excluded++
	}
	if decision.Status == FaceGroupNamed {
		rows, err := tx.Query(`SELECT faces.id, faces.photo_id FROM face_group_members JOIN faces ON faces.id = face_group_members.face_id
			JOIN photos ON photos.id = faces.photo_id
			WHERE face_group_members.group_id = ? AND face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
			ORDER BY faces.seq`, id)
		if err != nil {
			return FaceGroupDecisionResult{}, err
		}
		type member struct{ faceID, photoID string }
		members := []member{}
		for rows.Next() {
			var each member
			if err := rows.Scan(&each.faceID, &each.photoID); err != nil {
				rows.Close()
				return FaceGroupDecisionResult{}, err
			}
			members = append(members, each)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return FaceGroupDecisionResult{}, err
		}
		evidence := fmt.Sprintf("Named with [a face group](ref:%s).", id)
		if renaming {
			// The group was named as someone else: those links on its faces
			// were this group's word, and its word changed.
			faceIDs := make([]string, 0, len(members))
			for _, each := range members {
				faceIDs = append(faceIDs, each.faceID)
			}
			withdrawn, err := rejectPersonOnFaces(tx, faceIDs, previousPerson, decision.DecidedBy,
				fmt.Sprintf("[The face group](ref:%s) these faces are in was named as [someone else](ref:%s) instead.", id, decision.PersonID), at)
			if err != nil {
				return FaceGroupDecisionResult{}, err
			}
			result.PreviousPersonRejected = withdrawn
			evidence = fmt.Sprintf("Named with [a face group](ref:%s), which was first named as [someone else](ref:%s).", id, previousPerson)
		}
		for _, each := range members {
			link, created, err := proposePhotoPersonInTransaction(tx, PhotoPersonProposal{PhotoID: each.photoID, PersonID: decision.PersonID,
				FaceID: each.faceID, Status: PhotoPersonConfirmed, Confidence: 1, Evidence: evidence, ProposedBy: decision.DecidedBy,
				DecidedBy: decision.DecidedBy}, at)
			if err != nil {
				return FaceGroupDecisionResult{}, err
			}
			switch {
			case link.Status == PhotoPersonRejected:
				result.KeptRejected++
				continue
			case created || link.DecidedBy == decision.DecidedBy && link.UpdatedAt == at:
				result.Confirmed++
			default:
				result.AlreadyConfirmed++
			}
			others, err := rejectOtherSuggestionsOnFace(tx, each.faceID, decision.PersonID, decision.DecidedBy, id, at)
			if err != nil {
				return FaceGroupDecisionResult{}, err
			}
			result.OthersRejected += others
		}
	}
	if _, err := tx.Exec(`UPDATE face_groups SET status = ?, person_id = ?, decided_by = ?, decided_at = ?, updated_at = ? WHERE id = ?`,
		decision.Status, decision.PersonID, decision.DecidedBy, at, at, id); err != nil {
		return FaceGroupDecisionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FaceGroupDecisionResult{}, err
	}
	result.Group, err = s.FaceGroupByID(id, false)
	return result, err
}

// rejectPersonOnFaces rejects a person's links that name any of the faces,
// whatever their status, keeping each old row as a revision.
func rejectPersonOnFaces(tx *sql.Tx, faceIDs []string, personID, decidedBy, note string, at int64) (int, error) {
	links := []PhotoPerson{}
	for _, faceID := range faceIDs {
		rows, err := tx.Query(`SELECT `+photoPersonColumns+` FROM photo_people WHERE face_id = ? AND person_id = ? AND status != ?`, faceID, personID, PhotoPersonRejected)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			link, err := scanPhotoPerson(rows)
			if err != nil {
				rows.Close()
				return 0, err
			}
			links = append(links, link)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(tx, link, at); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE photo_people SET status = ?, decided_by = ?, evidence = ?, updated_at = ? WHERE id = ?`,
			PhotoPersonRejected, decidedBy, strings.TrimSpace(link.Evidence+"\n\n"+note), at, link.ID); err != nil {
			return 0, err
		}
	}
	return len(links), nil
}

// rejectOtherSuggestionsOnFace rejects suggestions that a face is someone
// other than personID, keeping each old row as a revision.
func rejectOtherSuggestionsOnFace(tx *sql.Tx, faceID, personID, decidedBy, groupID string, at int64) (int, error) {
	rows, err := tx.Query(`SELECT `+photoPersonColumns+` FROM photo_people WHERE face_id = ? AND person_id != ? AND status = ?`,
		faceID, personID, PhotoPersonSuggested)
	if err != nil {
		return 0, err
	}
	links := []PhotoPerson{}
	for rows.Next() {
		link, err := scanPhotoPerson(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		links = append(links, link)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(tx, link, at); err != nil {
			return 0, err
		}
		evidence := strings.TrimSpace(link.Evidence + fmt.Sprintf("\n\nThis face was named as [someone else](ref:%s) with [a face group](ref:%s).", personID, groupID))
		if _, err := tx.Exec(`UPDATE photo_people SET status = ?, decided_by = ?, evidence = ?, updated_at = ? WHERE id = ?`,
			PhotoPersonRejected, decidedBy, evidence, at, link.ID); err != nil {
			return 0, err
		}
	}
	return len(links), nil
}
