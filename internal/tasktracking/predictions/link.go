package predictions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// knownEntityTypesNote is quoted in the error below. This store deliberately
// does NOT validate entity_type against reference-store's list of reference
// types: kanban-store does not validate a card link's type either,
// unregistered types work end to end there today,
// and a hardcoded copy of somebody else's vocabulary is exactly the kind of
// second source of truth that goes stale silently.
const knownEntityTypesNote = "entity_type is free text; the canonical list is reference-store's GET /types"

func validateLink(l *Link) error {
	if strings.TrimSpace(l.EntityType) == "" || strings.TrimSpace(l.EntityRef) == "" {
		return fmt.Errorf("%w: entity_type and entity_ref are both required (%s)",
			ErrInvalidLink, knownEntityTypesNote)
	}
	return nil
}

// insertLink writes a link inside tx. A link already there is left as it is,
// so linking the same entity twice is not an error.
func insertLink(ctx context.Context, tx *sql.Tx, predictionSeq int64, l *Link, ts int64) error {
	if err := validateLink(l); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO links (prediction_seq, entity_type, entity_ref, label, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (prediction_seq, entity_type, entity_ref) DO NOTHING`,
		predictionSeq, strings.TrimSpace(l.EntityType), strings.TrimSpace(l.EntityRef), strings.TrimSpace(l.Label), ts); err != nil {
		return fmt.Errorf("inserting a link: %w", err)
	}
	return nil
}

// AddLink attaches the prediction to a record in the store that owns it.
// Re-linking the same entity is idempotent, matching kanban-store's own
// (card_id, entity_type, entity_ref) uniqueness: the link that is already
// there is returned, label and all.
func (s *Store) AddLink(ctx context.Context, predictionID string, l *Link) (*Link, error) {
	seq, err := predictionNumberOf(predictionID)
	if err != nil {
		return nil, err
	}
	if err := validateLink(l); err != nil {
		return nil, err
	}
	var out Link
	err = postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM predictions WHERE seq = $1)`, seq).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: prediction %s", ErrNotFound, predictionID)
		}
		if err := insertLink(ctx, tx, seq, l, now()); err != nil {
			return err
		}
		var linkedSeq int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id, prediction_seq, entity_type, entity_ref, label, created_at
			FROM links WHERE prediction_seq = $1 AND entity_type = $2 AND entity_ref = $3`,
			seq, strings.TrimSpace(l.EntityType), strings.TrimSpace(l.EntityRef)).
			Scan(&out.ID, &linkedSeq, &out.EntityType, &out.EntityRef, &out.Label, &out.CreatedAt); err != nil {
			return fmt.Errorf("reading the link back: %w", err)
		}
		out.PredictionID = formatPredictionID(linkedSeq)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListLinks returns everything the prediction points at.
// An id that names no prediction has no links, malformed or not.
func (s *Store) ListLinks(ctx context.Context, predictionID string) ([]*Link, error) {
	seq, err := predictionNumberOf(predictionID)
	if errors.Is(err, ErrNotFound) {
		return []*Link{}, nil
	}
	return s.listLinksOf(ctx, seq)
}

func (s *Store) listLinksOf(ctx context.Context, seq int64) ([]*Link, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, prediction_seq, entity_type, entity_ref, label, created_at
		FROM links WHERE prediction_seq = $1 ORDER BY id ASC`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Link{}
	for rows.Next() {
		var l Link
		var linkedSeq int64
		if err := rows.Scan(&l.ID, &linkedSeq, &l.EntityType, &l.EntityRef, &l.Label, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.PredictionID = formatPredictionID(linkedSeq)
		out = append(out, &l)
	}
	return out, rows.Err()
}

// DeleteLink detaches one entity.
func (s *Store) DeleteLink(ctx context.Context, linkID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, linkID)
	if err != nil {
		return err
	}
	return requireOneRowChanged(result, fmt.Sprintf("link %d", linkID))
}
