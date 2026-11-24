package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/kayushkin/kanban-store/internal/model"
)

// ClassificationLabelsForCards joins just the visible page in one snapshot/query.
// Revisions and labels must not be read separately across a concurrent review.
func (s *Store) ClassificationLabelsForCards(boardID string, ids []string) (map[string]*model.CardClassificationLabels, error) {
	out := make(map[string]*model.CardClassificationLabels, len(ids))
	for _, id := range ids {
		out[id] = &model.CardClassificationLabels{BoardID: boardID, CardID: id, Labels: []model.ClassificationLabel{}}
	}
	if len(ids) == 0 {
		return out, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT r.card_id, r.revision, l.axis_id, l.value_ids, l.origin_decision_id, l.origin_review_id, l.updated_at
 FROM classification_label_revisions r LEFT JOIN classification_labels l ON l.board_id=r.board_id AND l.card_id=r.card_id
 WHERE r.board_id=? AND r.card_id IN (SELECT value FROM json_each(?)) ORDER BY r.card_id,l.axis_id`, boardID, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var revision int64
		var axis, values, decision, review sql.NullString
		var updated sql.NullTime
		if err := rows.Scan(&id, &revision, &axis, &values, &decision, &review, &updated); err != nil {
			return nil, err
		}
		record := out[id]
		record.Revision = revision
		if !axis.Valid {
			continue
		}
		label := model.ClassificationLabel{AxisID: axis.String, OriginDecisionID: decision.String, OriginReviewID: review.String, UpdatedAt: updated.Time}
		if err := json.Unmarshal([]byte(values.String), &label.ValueIDs); err != nil {
			return nil, fmt.Errorf("card %s labels: %w", id, err)
		}
		record.Labels = append(record.Labels, label)
	}
	return out, rows.Err()
}
