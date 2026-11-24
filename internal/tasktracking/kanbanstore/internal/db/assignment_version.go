package db

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kayushkin/kanban-store/internal/model"
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

// The reserved SQLite writer lock covers both the snapshot check and insert.
func (s *Store) AssignPrincipalToCardIfVersion(cardID, principalID, actor, version string) (*model.CardAssignment, bool, error) {
	if version == "" {
		return s.AssignPrincipalToCard(cardID, principalID, actor)
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, false, err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	rows, err := conn.QueryContext(ctx, `SELECT `+cardAssignmentColumns+` FROM card_assignments WHERE card_id=? ORDER BY created_at ASC, principal_id ASC`, cardID)
	if err != nil {
		return nil, false, err
	}
	snapshot := []model.CardAssignment{}
	for rows.Next() {
		item, scanErr := scanCardAssignment(rows)
		if scanErr != nil {
			rows.Close()
			return nil, false, scanErr
		}
		snapshot = append(snapshot, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	if AssignmentVersion(snapshot) != version {
		return nil, false, ErrAssignmentsChanged
	}
	for _, item := range snapshot {
		if item.PrincipalID == principalID {
			return &item, false, nil
		}
	}
	candidate := &model.CardAssignment{CardID: cardID, PrincipalID: principalID, AssignedBy: actor, CreatedAt: now()}
	if _, err = conn.ExecContext(ctx, `INSERT INTO card_assignments (`+cardAssignmentColumns+`) VALUES (?, ?, ?, ?)`, cardID, principalID, actor, candidate.CreatedAt); err != nil {
		return nil, false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	return candidate, true, nil
}
