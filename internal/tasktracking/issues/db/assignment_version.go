package db

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
)

var ErrAssignmentsChanged = errors.New("card assignments changed; reload before assigning")

func AssignmentVersion(rows []model.CardAssignment) string {
	if rows == nil {
		rows = []model.CardAssignment{}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("\"%x\"", sha256.Sum256(data))
}

// AssignPrincipalToCardIfVersion assigns principalID to the card only when
// the card's assignments are still the ones version was taken from. The check
// and the insert run under writeImmediately, so no other such write lands
// between them.
func (s *Store) AssignPrincipalToCardIfVersion(cardID, principalID, actor, version string) (*model.CardAssignment, bool, error) {
	if version == "" {
		return s.AssignPrincipalToCard(cardID, principalID, actor)
	}
	var assignment *model.CardAssignment
	created := false
	err := s.db.writeImmediately(func(run executor) error {
		rows, err := run.Query(`SELECT `+cardAssignmentColumns+` FROM card_assignments WHERE card_id=? ORDER BY created_at ASC, principal_id ASC`, cardID)
		if err != nil {
			return err
		}
		snapshot := []model.CardAssignment{}
		for rows.Next() {
			item, scanErr := scanCardAssignment(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			snapshot = append(snapshot, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if AssignmentVersion(snapshot) != version {
			return ErrAssignmentsChanged
		}
		for _, item := range snapshot {
			if item.PrincipalID == principalID {
				assignment = &item
				return nil
			}
		}
		candidate := &model.CardAssignment{CardID: cardID, PrincipalID: principalID, AssignedBy: actor, CreatedAt: now()}
		if _, err := run.Exec(`INSERT INTO card_assignments (`+cardAssignmentColumns+`) VALUES (?, ?, ?, ?)`, cardID, principalID, actor, candidate.CreatedAt); err != nil {
			return err
		}
		assignment, created = candidate, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return assignment, created, nil
}
