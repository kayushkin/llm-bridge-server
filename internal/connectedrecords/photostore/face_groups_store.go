package photostore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

func faceGroupID(seq int64) string { return postgresstorage.FormatRecordID(FaceGroupIDPrefix, seq) }

func faceGroupGuessID(seq int64) string {
	return postgresstorage.FormatRecordID(FaceGroupGuessIDPrefix, seq)
}

// faceGroupSampleSize is how many faces GET /face-groups shows of each group.
const faceGroupSampleSize = 12

// checkFaceGroupMembers refuses a face that is missing or out of scope,
// retired, on a deleted photo, found by another model, already in a group,
// named twice, or of another owner than the rest. ownerPrincipalID is the
// group's owner, or "" for a new group, which takes its faces' owner; the
// owner is answered.
func checkFaceGroupMembers(ctx context.Context, tx *sql.Tx, scope ownership.Scope, ownerPrincipalID, model string, members []NewFaceGroupMember) (string, error) {
	seen := map[string]bool{}
	for index, member := range members {
		if seen[member.FaceID] {
			return "", fmt.Errorf("%w: member %d: %s is named twice", ErrInvalid, index, member.FaceID)
		}
		seen[member.FaceID] = true
		if member.Similarity < -1 || member.Similarity > 1 {
			return "", fmt.Errorf("%w: member %d: similarity is between -1 and 1", ErrInvalid, index)
		}
		faceSeq, err := postgresstorage.ParseRecordID(FaceIDPrefix, member.FaceID)
		if err != nil {
			return "", fmt.Errorf("%w: member %d: no face %s", ErrInvalid, index, member.FaceID)
		}
		args := &arguments{}
		var faceOwner, faceModel string
		var retiredAt, deletedAt int64
		err = tx.QueryRowContext(ctx, `SELECT faces.owner_principal_id, faces.model, faces.retired_at, photos.deleted_at
			FROM faces JOIN photos ON photos.seq = faces.photo_seq
			WHERE faces.seq = `+args.add(faceSeq)+` AND `+args.scope(scope, "faces.owner_principal_id")+` FOR UPDATE OF faces`, args.values...).
			Scan(&faceOwner, &faceModel, &retiredAt, &deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: member %d: no face %s", ErrInvalid, index, member.FaceID)
		} else if err != nil {
			return "", err
		}
		if ownerPrincipalID == "" {
			ownerPrincipalID = faceOwner
		}
		switch {
		case faceOwner != ownerPrincipalID:
			return "", fmt.Errorf("%w: member %d: %s belongs to %s, and the group's faces to %s; a group holds one owner's faces",
				ErrInvalid, index, member.FaceID, faceOwner, ownerPrincipalID)
		case faceModel != model:
			return "", fmt.Errorf("%w: member %d: %s was found by %s, not %s", ErrInvalid, index, member.FaceID, faceModel, model)
		case retiredAt != 0:
			return "", fmt.Errorf("%w: member %d: %s was retired", ErrInvalid, index, member.FaceID)
		case deletedAt != 0:
			return "", fmt.Errorf("%w: member %d: %s is on a deleted photo", ErrInvalid, index, member.FaceID)
		}
		var inGroup int64
		err = tx.QueryRowContext(ctx, `SELECT group_seq FROM face_group_members WHERE face_seq = $1 AND released_at = 0`, faceSeq).Scan(&inGroup)
		if err == nil {
			return "", fmt.Errorf("%w: member %d: %s is already in %s", ErrConflict, index, member.FaceID, faceGroupID(inGroup))
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return ownerPrincipalID, nil
}

func insertFaceGroupMembers(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, groupSeq int64, members []NewFaceGroupMember, at int64) error {
	for _, member := range members {
		// A face taken out of this group before and found to belong again
		// replaces its released row.
		if _, err := tx.ExecContext(ctx, `INSERT INTO face_group_members (owner_principal_id, group_seq, face_seq, similarity, added_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (group_seq, face_seq) DO UPDATE SET similarity = excluded.similarity, added_at = excluded.added_at, released_at = 0, released_by = ''`,
			ownerPrincipalID, groupSeq, mustRecordNumber(FaceIDPrefix, member.FaceID), member.Similarity, at); err != nil {
			return conflictOf(err)
		}
	}
	return nil
}

// faceGroupOwner reads a group in scope: its owner, model, status and the
// person it was named as. forUpdate locks it for the transaction.
func faceGroupOwner(ctx context.Context, q queryer, scope ownership.Scope, groupSeq int64, id string, forUpdate bool) (owner, model, status, personID string, err error) {
	if err := checkScope(scope); err != nil {
		return "", "", "", "", err
	}
	args := &arguments{}
	query := `SELECT owner_principal_id, model, status, person_id FROM face_groups WHERE seq = ` + args.add(groupSeq) + ` AND ` +
		args.scope(scope, "owner_principal_id")
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err = q.QueryRowContext(ctx, query, args.values...).Scan(&owner, &model, &status, &personID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	}
	return owner, model, status, personID, err
}

// CreateFaceGroup answers POST /face-groups: a new open group of faces in
// scope, all of one owner, which the group takes.
func (s *Store) CreateFaceGroup(ctx context.Context, scope ownership.Scope, group NewFaceGroup) (FaceGroup, error) {
	if err := checkScope(scope); err != nil {
		return FaceGroup{}, err
	}
	if strings.TrimSpace(group.Model) == "" {
		return FaceGroup{}, fmt.Errorf("%w: model is required: the face model whose faces these are", ErrInvalid)
	}
	if strings.TrimSpace(group.MadeBy) == "" {
		return FaceGroup{}, fmt.Errorf("%w: made_by is required: what grouped these faces", ErrInvalid)
	}
	if len(group.Members) == 0 {
		return FaceGroup{}, fmt.Errorf("%w: a group needs at least one face", ErrInvalid)
	}
	var id string
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		owner, err := checkFaceGroupMembers(ctx, tx, scope, "", group.Model, group.Members)
		if err != nil {
			return err
		}
		at := now()
		var seq int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO face_groups (owner_principal_id, model, status, made_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5) RETURNING seq`, owner, group.Model, FaceGroupOpen, group.MadeBy, at).Scan(&seq); err != nil {
			return err
		}
		id = faceGroupID(seq)
		return insertFaceGroupMembers(ctx, tx, owner, seq, group.Members, at)
	})
	if err != nil {
		return FaceGroup{}, err
	}
	return s.FaceGroupByID(ctx, scope, id, false)
}

// AddFaceGroupMembers answers POST /face-groups/{id}/members: faces of the
// group's owner a grouper found to belong to a group in scope still open.
func (s *Store) AddFaceGroupMembers(ctx context.Context, scope ownership.Scope, id string, addition FaceGroupMembersAddition) (FaceGroup, error) {
	if len(addition.Members) == 0 {
		return FaceGroup{}, fmt.Errorf("%w: members is empty", ErrInvalid)
	}
	groupSeq, err := recordNumberOf(FaceGroupIDPrefix, "face group", id)
	if err != nil {
		return FaceGroup{}, err
	}
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		owner, model, status, _, err := faceGroupOwner(ctx, tx, scope, groupSeq, id, true)
		if err != nil {
			return err
		}
		if status != FaceGroupOpen {
			return fmt.Errorf("%w: %s is %s; only an open group takes faces", ErrConflict, id, status)
		}
		if _, err := checkFaceGroupMembers(ctx, tx, scope, owner, model, addition.Members); err != nil {
			return err
		}
		at := now()
		if err := insertFaceGroupMembers(ctx, tx, owner, groupSeq, addition.Members, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE face_groups SET updated_at = $1 WHERE seq = $2`, at, groupSeq)
		return err
	})
	if err != nil {
		return FaceGroup{}, err
	}
	return s.FaceGroupByID(ctx, scope, id, false)
}

// AddFaceGroupGuess answers POST /face-groups/{id}/guesses: a guess on a
// group in scope, owned by the group's owner. A person it names must be one
// that owner keeps in people-store.
func (s *Store) AddFaceGroupGuess(ctx context.Context, scope ownership.Scope, id string, proposal FaceGroupGuessProposal) (FaceGroupGuess, error) {
	if proposal.PersonID != "" && !personIDPattern.MatchString(proposal.PersonID) {
		return FaceGroupGuess{}, fmt.Errorf("%w: person_id must be people-store's id (person_000001) or \"\" for nobody in it, got %q", ErrInvalid, proposal.PersonID)
	}
	if strings.TrimSpace(proposal.ProposedBy) == "" {
		return FaceGroupGuess{}, fmt.Errorf("%w: proposed_by is required: who or what guessed", ErrInvalid)
	}
	if proposal.Confidence < 0 || proposal.Confidence > 1 {
		return FaceGroupGuess{}, fmt.Errorf("%w: confidence is between 0 and 1", ErrInvalid)
	}
	groupSeq, err := recordNumberOf(FaceGroupIDPrefix, "face group", id)
	if err != nil {
		return FaceGroupGuess{}, err
	}
	owner, _, _, _, err := faceGroupOwner(ctx, s.db, scope, groupSeq, id, false)
	if err != nil {
		return FaceGroupGuess{}, err
	}
	if proposal.PersonID != "" {
		if err := s.personKeptByOwner(ctx, owner, proposal.PersonID); err != nil {
			return FaceGroupGuess{}, err
		}
	}
	guess := FaceGroupGuess{OwnerPrincipalID: owner, GroupID: id, PersonID: proposal.PersonID, Confidence: proposal.Confidence,
		Evidence: proposal.Evidence, ProposedBy: proposal.ProposedBy, CreatedAt: now()}
	var seq int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO face_group_guesses (owner_principal_id, group_seq, person_id, confidence, evidence, proposed_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING seq`, owner, groupSeq, guess.PersonID, guess.Confidence, guess.Evidence, guess.ProposedBy,
		guess.CreatedAt).Scan(&seq); err != nil {
		return FaceGroupGuess{}, err
	}
	guess.ID = faceGroupGuessID(seq)
	return guess, nil
}

// FaceGroupFilter selects groups for GET /face-groups.
type FaceGroupFilter struct {
	Status   string
	PersonID string
	Limit    int
	Offset   int
}

// FaceGroups answers GET /face-groups: groups in scope with their counts,
// samples and guesses, the most photos first.
func (s *Store) FaceGroups(ctx context.Context, scope ownership.Scope, filter FaceGroupFilter) ([]FaceGroup, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	args := &arguments{}
	where := []string{args.scope(scope, "face_groups.owner_principal_id")}
	if filter.Status != "" {
		if !slices.Contains(FaceGroupStatuses, filter.Status) {
			return nil, fmt.Errorf("%w: status is one of %s", ErrInvalid, strings.Join(FaceGroupStatuses, ", "))
		}
		where = append(where, "face_groups.status = "+args.add(filter.Status))
	}
	if filter.PersonID != "" {
		where = append(where, "face_groups.person_id = "+args.add(filter.PersonID))
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumLimit {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT face_groups.seq FROM face_groups
		LEFT JOIN (SELECT face_group_members.group_seq, COUNT(DISTINCT faces.photo_seq) AS photo_count
			FROM face_group_members JOIN faces ON faces.seq = face_group_members.face_seq JOIN photos ON photos.seq = faces.photo_seq
			WHERE face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
			GROUP BY face_group_members.group_seq) counted ON counted.group_seq = face_groups.seq
		WHERE `+strings.Join(where, " AND ")+` ORDER BY COALESCE(counted.photo_count, 0) DESC, face_groups.seq
		LIMIT `+args.add(limit)+` OFFSET `+args.add(max(filter.Offset, 0)), args.values...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, faceGroupID(seq))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groups := make([]FaceGroup, 0, len(ids))
	for _, id := range ids {
		group, err := s.FaceGroupByID(ctx, scope, id, false)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// FaceGroupByID answers GET /face-groups/{id} for a group in scope;
// withMembers fills Members.
func (s *Store) FaceGroupByID(ctx context.Context, scope ownership.Scope, id string, withMembers bool) (FaceGroup, error) {
	if err := checkScope(scope); err != nil {
		return FaceGroup{}, err
	}
	groupSeq, err := recordNumberOf(FaceGroupIDPrefix, "face group", id)
	if err != nil {
		return FaceGroup{}, err
	}
	args := &arguments{}
	group := FaceGroup{ID: id}
	err = s.db.QueryRowContext(ctx, `SELECT owner_principal_id, model, status, person_id, made_by, decided_by, decided_at, created_at, updated_at
		FROM face_groups WHERE seq = `+args.add(groupSeq)+` AND `+args.scope(scope, "owner_principal_id"), args.values...).
		Scan(&group.OwnerPrincipalID, &group.Model, &group.Status, &group.PersonID, &group.MadeBy, &group.DecidedBy, &group.DecidedAt,
			&group.CreatedAt, &group.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FaceGroup{}, fmt.Errorf("%w: no face group %s", ErrNotFound, id)
	} else if err != nil {
		return FaceGroup{}, err
	}
	members, err := s.faceGroupMembers(ctx, groupSeq)
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
	group.Guesses, err = s.faceGroupGuesses(ctx, groupSeq, id)
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
func (s *Store) faceGroupMembers(ctx context.Context, groupSeq int64) ([]FaceGroupMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT faces.seq, faces.photo_seq, face_group_members.similarity, photos.taken_at, photos.folder
		FROM face_group_members JOIN faces ON faces.seq = face_group_members.face_seq JOIN photos ON photos.seq = faces.photo_seq
		WHERE face_group_members.group_seq = $1 AND face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
		ORDER BY face_group_members.similarity DESC, faces.seq`, groupSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []FaceGroupMember{}
	for rows.Next() {
		var member FaceGroupMember
		var faceSeq, photoSeq int64
		if err := rows.Scan(&faceSeq, &photoSeq, &member.Similarity, &member.TakenAt, &member.Folder); err != nil {
			return nil, err
		}
		member.FaceID, member.PhotoID = faceID(faceSeq), photoID(photoSeq)
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Store) faceGroupGuesses(ctx context.Context, groupSeq int64, groupID string) ([]FaceGroupGuess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, owner_principal_id, person_id, confidence, evidence, proposed_by, created_at FROM face_group_guesses
		WHERE group_seq = $1 ORDER BY seq DESC`, groupSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	guesses := []FaceGroupGuess{}
	for rows.Next() {
		guess := FaceGroupGuess{GroupID: groupID}
		var seq int64
		if err := rows.Scan(&seq, &guess.OwnerPrincipalID, &guess.PersonID, &guess.Confidence, &guess.Evidence, &guess.ProposedBy, &guess.CreatedAt); err != nil {
			return nil, err
		}
		guess.ID = faceGroupGuessID(seq)
		guesses = append(guesses, guess)
	}
	return guesses, rows.Err()
}

// DecideFaceGroup answers POST /face-groups/{id}/decision for a group in
// scope. It takes the excluded faces out of the group, then sets its status.
// Naming it, as a person the group's owner keeps in people-store, confirms
// each remaining face as the person, through the same rules as POST
// /photo-people: a person's rejection holds, and a confirmed link stays. A
// suggestion of anyone else on those faces is rejected, since a face is one
// person. An open group can be decided; a group left not_known or mixed can
// still be named; a named group can only be named as someone else, which
// rejects the first person's links on its faces and confirms the new one's.
// A single wrong face on it is fixed with PATCH /photo-people.
func (s *Store) DecideFaceGroup(ctx context.Context, scope ownership.Scope, id string, decision FaceGroupDecision) (FaceGroupDecisionResult, error) {
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
	groupSeq, err := recordNumberOf(FaceGroupIDPrefix, "face group", id)
	if err != nil {
		return FaceGroupDecisionResult{}, err
	}
	// A group's owner never changes, so it is read once, before people-store
	// is asked.
	owner, _, _, _, err := faceGroupOwner(ctx, s.db, scope, groupSeq, id, false)
	if err != nil {
		return FaceGroupDecisionResult{}, err
	}
	if decision.Status == FaceGroupNamed {
		if err := s.personKeptByOwner(ctx, owner, decision.PersonID); err != nil {
			return FaceGroupDecisionResult{}, err
		}
	}
	result := FaceGroupDecisionResult{}
	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		_, _, status, previousPerson, err := faceGroupOwner(ctx, tx, ownership.OwnedBy(owner), groupSeq, id, true)
		if err != nil {
			return err
		}
		renaming := status == FaceGroupNamed
		if renaming && (decision.Status != FaceGroupNamed || decision.PersonID == previousPerson || len(decision.ExcludedFaceIDs) > 0) {
			return fmt.Errorf("%w: %s is named as %s; it can only be named as someone else, with no faces left out", ErrConflict, id, previousPerson)
		}
		if status != FaceGroupOpen && decision.Status != FaceGroupNamed {
			return fmt.Errorf("%w: %s is %s; it can still be named, not decided again", ErrConflict, id, status)
		}
		at := now()
		for _, excluded := range decision.ExcludedFaceIDs {
			faceSeq, err := postgresstorage.ParseRecordID(FaceIDPrefix, excluded)
			if err != nil {
				return fmt.Errorf("%w: %s is not in %s", ErrInvalid, excluded, id)
			}
			released, err := tx.ExecContext(ctx, `UPDATE face_group_members SET released_at = $1, released_by = $2
				WHERE group_seq = $3 AND face_seq = $4 AND released_at = 0`, at, decision.DecidedBy, groupSeq, faceSeq)
			if err != nil {
				return err
			}
			if count, err := released.RowsAffected(); err != nil {
				return err
			} else if count == 0 {
				return fmt.Errorf("%w: %s is not in %s", ErrInvalid, excluded, id)
			}
			result.Excluded++
		}
		if decision.Status == FaceGroupNamed {
			if err := s.nameFaceGroup(ctx, tx, owner, groupSeq, id, previousPerson, renaming, decision, at, &result); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE face_groups SET status = $1, person_id = $2, decided_by = $3, decided_at = $4, updated_at = $4 WHERE seq = $5`,
			decision.Status, decision.PersonID, decision.DecidedBy, at, groupSeq)
		return err
	})
	if err != nil {
		return FaceGroupDecisionResult{}, conflictOf(err)
	}
	result.Group, err = s.FaceGroupByID(ctx, scope, id, false)
	return result, err
}

// nameFaceGroup does what naming a group does to the links of its faces,
// inside DecideFaceGroup's transaction.
func (s *Store) nameFaceGroup(ctx context.Context, tx *sql.Tx, owner string, groupSeq int64, id, previousPerson string, renaming bool,
	decision FaceGroupDecision, at int64, result *FaceGroupDecisionResult) error {
	rows, err := tx.QueryContext(ctx, `SELECT faces.seq, faces.photo_seq FROM face_group_members JOIN faces ON faces.seq = face_group_members.face_seq
		JOIN photos ON photos.seq = faces.photo_seq
		WHERE face_group_members.group_seq = $1 AND face_group_members.released_at = 0 AND faces.retired_at = 0 AND photos.deleted_at = 0
		ORDER BY faces.seq`, groupSeq)
	if err != nil {
		return err
	}
	type member struct{ faceSeq, photoSeq int64 }
	members := []member{}
	for rows.Next() {
		var each member
		if err := rows.Scan(&each.faceSeq, &each.photoSeq); err != nil {
			rows.Close()
			return err
		}
		members = append(members, each)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	evidence := fmt.Sprintf("Named with [a face group](ref:%s).", id)
	if renaming {
		// The group was named as someone else: those links on its faces
		// were this group's word, and its word changed.
		faceSeqs := make([]int64, 0, len(members))
		for _, each := range members {
			faceSeqs = append(faceSeqs, each.faceSeq)
		}
		withdrawn, err := rejectPersonOnFaces(ctx, tx, faceSeqs, previousPerson, decision.DecidedBy,
			fmt.Sprintf("[The face group](ref:%s) these faces are in was named as [someone else](ref:%s) instead.", id, decision.PersonID), at)
		if err != nil {
			return err
		}
		result.PreviousPersonRejected = withdrawn
		evidence = fmt.Sprintf("Named with [a face group](ref:%s), which was first named as [someone else](ref:%s).", id, previousPerson)
	}
	for _, each := range members {
		link, created, err := proposePhotoPersonInTransaction(ctx, tx, owner, each.photoSeq, PhotoPersonProposal{PhotoID: photoID(each.photoSeq),
			PersonID: decision.PersonID, FaceID: faceID(each.faceSeq), Status: PhotoPersonConfirmed, Confidence: 1, Evidence: evidence,
			ProposedBy: decision.DecidedBy, DecidedBy: decision.DecidedBy}, at)
		if err != nil {
			return err
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
		others, err := rejectOtherSuggestionsOnFace(ctx, tx, each.faceSeq, decision.PersonID, decision.DecidedBy, id, at)
		if err != nil {
			return err
		}
		result.OthersRejected += others
	}
	return nil
}

// rejectPersonOnFaces rejects a person's links that name any of the faces,
// whatever their status, keeping each old row as a revision.
func rejectPersonOnFaces(ctx context.Context, tx *sql.Tx, faceSeqs []int64, personID, decidedBy, note string, at int64) (int, error) {
	links := []PhotoPerson{}
	for _, faceSeq := range faceSeqs {
		found, err := queryPhotoPeople(ctx, tx, `SELECT `+photoPersonColumns+` FROM photo_people WHERE face_seq = $1 AND person_id = $2 AND status <> $3 FOR UPDATE`,
			faceSeq, personID, PhotoPersonRejected)
		if err != nil {
			return 0, err
		}
		links = append(links, found...)
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(ctx, tx, link, at); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_people SET status = $1, decided_by = $2, evidence = $3, updated_at = $4 WHERE seq = $5`,
			PhotoPersonRejected, decidedBy, strings.TrimSpace(link.Evidence+"\n\n"+note), at, mustRecordNumber(PhotoPersonIDPrefix, link.ID)); err != nil {
			return 0, err
		}
	}
	return len(links), nil
}

// rejectOtherSuggestionsOnFace rejects suggestions that a face is someone
// other than personID, keeping each old row as a revision.
func rejectOtherSuggestionsOnFace(ctx context.Context, tx *sql.Tx, faceSeq int64, personID, decidedBy, groupID string, at int64) (int, error) {
	links, err := queryPhotoPeople(ctx, tx, `SELECT `+photoPersonColumns+` FROM photo_people WHERE face_seq = $1 AND person_id <> $2 AND status = $3 FOR UPDATE`,
		faceSeq, personID, PhotoPersonSuggested)
	if err != nil {
		return 0, err
	}
	for _, link := range links {
		if err := keepPhotoPersonRevision(ctx, tx, link, at); err != nil {
			return 0, err
		}
		evidence := strings.TrimSpace(link.Evidence + fmt.Sprintf("\n\nThis face was named as [someone else](ref:%s) with [a face group](ref:%s).", personID, groupID))
		if _, err := tx.ExecContext(ctx, `UPDATE photo_people SET status = $1, decided_by = $2, evidence = $3, updated_at = $4 WHERE seq = $5`,
			PhotoPersonRejected, decidedBy, evidence, at, mustRecordNumber(PhotoPersonIDPrefix, link.ID)); err != nil {
			return 0, err
		}
	}
	return len(links), nil
}
