package predictionstore

import (
	"database/sql"
	"fmt"
	"strings"
)

// execer is satisfied by both *sql.DB and *sql.Tx so a link can be written
// inside Create's transaction or on its own.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// knownEntityTypesNote is quoted in the error below. This store deliberately
// does NOT validate entity_type against reference-store's list of reference
// types: kanban-store does not validate a card link's type either,
// unregistered types work end to end there today,
// and a hardcoded copy of somebody else's vocabulary is exactly the kind of
// second source of truth that goes stale silently.
const knownEntityTypesNote = "entity_type is free text; the canonical list is reference-store's GET /types"

func insertLink(ex execer, predictionID string, l *Link, ts int64) error {
	entityType := strings.TrimSpace(l.EntityType)
	entityRef := strings.TrimSpace(l.EntityRef)
	if entityType == "" || entityRef == "" {
		return fmt.Errorf("%w: entity_type and entity_ref are both required (%s)",
			ErrInvalidLink, knownEntityTypesNote)
	}
	_, err := ex.Exec(`
		INSERT OR IGNORE INTO links (prediction_id, entity_type, entity_ref, label, created_at)
		VALUES (?,?,?,?,?)`, predictionID, entityType, entityRef, strings.TrimSpace(l.Label), ts)
	return err
}

// AddLink attaches the prediction to a record in the store that owns it.
// Re-linking the same entity is idempotent, matching kanban-store's own
// (card_id, entity_type, entity_ref) uniqueness.
func (s *Store) AddLink(predictionID string, l *Link) (*Link, error) {
	if _, err := s.Get(predictionID); err != nil {
		return nil, err
	}
	if err := insertLink(s.db, predictionID, l, now()); err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`
		SELECT id, prediction_id, entity_type, entity_ref, label, created_at
		FROM links WHERE prediction_id=? AND entity_type=? AND entity_ref=?`,
		predictionID, strings.TrimSpace(l.EntityType), strings.TrimSpace(l.EntityRef))
	var out Link
	if err := row.Scan(&out.ID, &out.PredictionID, &out.EntityType, &out.EntityRef,
		&out.Label, &out.CreatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListLinks returns everything the prediction points at.
func (s *Store) ListLinks(predictionID string) ([]*Link, error) {
	rows, err := s.db.Query(`
		SELECT id, prediction_id, entity_type, entity_ref, label, created_at
		FROM links WHERE prediction_id=? ORDER BY id ASC`, predictionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.PredictionID, &l.EntityType, &l.EntityRef,
			&l.Label, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// DeleteLink detaches one entity.
func (s *Store) DeleteLink(linkID int64) error {
	res, err := s.db.Exec(`DELETE FROM links WHERE id=?`, linkID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: link %d", ErrNotFound, linkID)
	}
	return nil
}
