package db

import (
	"database/sql"

	"github.com/kayushkin/kanban-store/internal/model"
)

// A column's work_state, and the reads that turn it into a card's shared state.
// model.SharedWorkState owns the rule; these only fetch its input, in one query
// however many cards are asked about.

// migrateWorkStates adds columns.work_state: nullable, because a column nobody
// has mapped has no answer and must not be given one.
func migrateWorkStates(db *sql.DB) error {
	return addColumnIfMissing(db, "columns", "work_state", "TEXT")
}

// workStateValue renders a column's work_state for storage; absent is NULL.
func workStateValue(state *model.WorkState) any {
	if state == nil || *state == "" {
		return nil
	}
	return string(*state)
}

// workStateFromRequest reads the field a create or update request carries.
// The request validated the word already; an empty string clears.
func workStateFromRequest(raw *string) *model.WorkState {
	if raw == nil || *raw == "" {
		return nil
	}
	state := model.WorkState(*raw)
	return &state
}

// placementWorkStateColumns is the select list scanPlacementWorkState reads,
// over placements p joined to columns c.
const placementWorkStateColumns = `p.board_id, p.column_id, c.name, c.work_state, p.updated_at`

// scanPlacementWorkState reads one placement row. present is false for the
// all-NULL row a LEFT JOIN answers for a card on no board.
func scanPlacementWorkState(boardID, columnID, columnName, workState sql.NullString, movedAt sql.NullTime) (model.PlacementWorkState, bool) {
	if !boardID.Valid {
		return model.PlacementWorkState{}, false
	}
	placement := model.PlacementWorkState{
		BoardID: boardID.String, ColumnID: columnID.String, ColumnName: columnName.String,
		MovedAt: movedAt.Time.UTC(),
	}
	if workState.Valid && workState.String != "" {
		state := model.WorkState(workState.String)
		placement.WorkState = &state
	}
	return placement, true
}

// ListPlacementWorkStatesForCards reads every placement of each named card,
// on every board, with its column's work_state — one query, so a board view
// of thousands of cards is not thousands of reads. A card on no board is
// absent from the map.
func (s *Store) ListPlacementWorkStatesForCards(cardIDs []string) (map[string][]model.PlacementWorkState, error) {
	out := map[string][]model.PlacementWorkState{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT p.card_id, `+placementWorkStateColumns+`
		FROM placements p JOIN columns c ON c.id = p.column_id
		WHERE p.card_id IN (`+placeholders(len(cardIDs))+`)`, anySlice(cardIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cardID string
		var boardID, columnID, columnName, workState sql.NullString
		var movedAt sql.NullTime
		if err := rows.Scan(&cardID, &boardID, &columnID, &columnName, &workState, &movedAt); err != nil {
			return nil, err
		}
		if placement, present := scanPlacementWorkState(boardID, columnID, columnName, workState, movedAt); present {
			out[cardID] = append(out[cardID], placement)
		}
	}
	return out, rows.Err()
}

// EntityLinkedCard is one card linked to an entity, with every placement it
// has. Placements is empty for a card that sits on no board.
type EntityLinkedCard struct {
	CardID     string
	Placements []model.PlacementWorkState
}

// ListCardsByEntity returns the cards linked to one entity, oldest link first,
// each with every placement it has and those columns' work_state — in one
// query, so a rollup over an entity (a project, a session) costs the same
// however many cards it has.
//
// The order is part of the answer, not a detail: a caller that has to pick a
// single card out of several — "which todo is this session for?" — gets the
// link that was made first, which is the one made before the entity did
// anything. Without an ORDER BY, SQLite picks, and that caller silently picks a
// different card as rows move around. Ties break on card_id so the order is
// total.
func (s *Store) ListCardsByEntity(entityType, entityRef string) ([]EntityLinkedCard, error) {
	rows, err := s.db.Query(`SELECT linked.card_id, `+placementWorkStateColumns+`
		FROM (SELECT card_id, MIN(created_at) AS first_linked_at
		      FROM card_links WHERE entity_type=? AND entity_ref=? GROUP BY card_id) linked
		LEFT JOIN placements p ON p.card_id = linked.card_id
		LEFT JOIN columns c ON c.id = p.column_id
		ORDER BY linked.first_linked_at, linked.card_id, p.board_id`,
		entityType, entityRef,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityLinkedCard
	for rows.Next() {
		var cardID string
		var boardID, columnID, columnName, workState sql.NullString
		var movedAt sql.NullTime
		if err := rows.Scan(&cardID, &boardID, &columnID, &columnName, &workState, &movedAt); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].CardID != cardID {
			out = append(out, EntityLinkedCard{CardID: cardID, Placements: []model.PlacementWorkState{}})
		}
		if placement, present := scanPlacementWorkState(boardID, columnID, columnName, workState, movedAt); present {
			out[len(out)-1].Placements = append(out[len(out)-1].Placements, placement)
		}
	}
	return out, rows.Err()
}
