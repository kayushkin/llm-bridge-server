package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/kayushkin/kanban-store/internal/model"
)

// The ticket row and the lifecycle read.
//
// One row per card, created when a card becomes a ticket and deleted when it
// stops being one. The card outlives both: deleting a ticket leaves the card
// where it is, because "this is not a ticket" is not "this is not work".

// ensureTicketTables is called from the schema path; see db.go.
func ensureTicketTables(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tickets (
			card_id                TEXT PRIMARY KEY,
			requester_principal_id TEXT NOT NULL,
			channel                TEXT NOT NULL,
			created_at             DATETIME NOT NULL,
			updated_at             DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_tickets_requester ON tickets(requester_principal_id);
	`); err != nil {
		return err
	}
	// The record a ticket came from; see model.Ticket. Empty on rows written
	// before these columns existed.
	for _, column := range []string{"source_entity_type", "source_entity_ref"} {
		if err := addColumnIfMissing(db, "tickets", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// A column's lifecycle state: nullable, because a board that is not a
	// ticket board has no answer and must not be given one.
	return addColumnIfMissing(db, "columns", "lifecycle_state", "TEXT")
}

// ErrTicketSourceConflict is a write naming a source other than the one the
// ticket already records. A ticket comes from one place, once.
var ErrTicketSourceConflict = errors.New("ticket source conflict")

// ticketColumns is the select list every ticket read scans with scanTicket.
const ticketColumns = `card_id, requester_principal_id, channel, source_entity_type, source_entity_ref, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanTicket(row rowScanner) (*model.Ticket, error) {
	ticket := &model.Ticket{}
	var channel string
	if err := row.Scan(&ticket.CardID, &ticket.RequesterPrincipalID, &channel,
		&ticket.SourceEntityType, &ticket.SourceEntityRef, &ticket.CreatedAt, &ticket.UpdatedAt); err != nil {
		return nil, err
	}
	ticket.Channel = model.TicketChannel(channel)
	return ticket, nil
}

// UpsertTicket writes the ticket for a card. The caller has already checked
// the requester with principal-store, the channel against the vocabulary and
// the source type against the entity-type registry; this is the write.
//
// An empty source leaves a stored one alone, and a ticket with no source yet
// takes the one given. A source different from the stored one is
// ErrTicketSourceConflict and writes nothing. The check and the write are one
// statement, so two writers cannot both set a source.
func (s *Store) UpsertTicket(cardID string, requesterPrincipalID string, channel model.TicketChannel,
	sourceEntityType, sourceEntityRef string) (*model.Ticket, error) {
	timestamp := now()
	result, err := s.db.Exec(`
		INSERT INTO tickets (card_id, requester_principal_id, channel, source_entity_type, source_entity_ref, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(card_id) DO UPDATE SET
			requester_principal_id = excluded.requester_principal_id,
			channel                = excluded.channel,
			source_entity_type     = CASE WHEN tickets.source_entity_type = '' THEN excluded.source_entity_type ELSE tickets.source_entity_type END,
			source_entity_ref      = CASE WHEN tickets.source_entity_type = '' THEN excluded.source_entity_ref ELSE tickets.source_entity_ref END,
			updated_at             = excluded.updated_at
		WHERE excluded.source_entity_type = ''
		   OR tickets.source_entity_type = ''
		   OR (tickets.source_entity_type = excluded.source_entity_type AND tickets.source_entity_ref = excluded.source_entity_ref)`,
		cardID, requesterPrincipalID, string(channel), sourceEntityType, sourceEntityRef, timestamp, timestamp)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if affected == 0 {
		return nil, ErrTicketSourceConflict
	}
	return s.GetTicket(cardID)
}

// GetTicket returns one card's ticket row, or ErrNotFound when the card is not
// a ticket.
func (s *Store) GetTicket(cardID string) (*model.Ticket, error) {
	ticket, err := scanTicket(s.db.QueryRow(`SELECT `+ticketColumns+` FROM tickets WHERE card_id = ?`, cardID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return ticket, nil
}

// DeleteTicket makes a card an ordinary card again. Missing is ErrNotFound
// rather than a silent success: a caller deleting a ticket that is not there
// has the wrong card id, and should hear so.
func (s *Store) DeleteTicket(cardID string) error {
	result, err := s.db.Exec(`DELETE FROM tickets WHERE card_id = ?`, cardID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTicketsForCards reads the ticket of each card in one query, for a board
// view that must not do one read per card.
func (s *Store) ListTicketsForCards(cardIDs []string) (map[string]*model.Ticket, error) {
	out := map[string]*model.Ticket{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	query := `SELECT ` + ticketColumns + ` FROM tickets WHERE card_id IN (` +
		placeholders(len(cardIDs)) + `)`
	args := make([]any, 0, len(cardIDs))
	for _, id := range cardIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out[ticket.CardID] = ticket
	}
	return out, rows.Err()
}

// TicketListFilter narrows ListTickets. Empty fields do not filter.
type TicketListFilter struct {
	Channel model.TicketChannel
	// Before keeps tickets created strictly before this time; the page cursor.
	Before *time.Time
}

// ListTickets reads tickets newest first, across every board, each with its
// card's card_created time when that event exists. The caller filters by
// board access, so it asks for more than one page when it has to.
func (s *Store) ListTickets(filter TicketListFilter, limit int) ([]model.TicketLogEntry, error) {
	conditions, args := "1=1", []any{}
	if filter.Channel != "" {
		conditions += ` AND t.channel = ?`
		args = append(args, string(filter.Channel))
	}
	if filter.Before != nil {
		conditions += ` AND t.created_at < ?`
		args = append(args, filter.Before.UTC())
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
		SELECT t.card_id, t.requester_principal_id, t.channel, t.source_entity_type, t.source_entity_ref,
		       t.created_at, t.updated_at,
		       (SELECT MIN(e.occurred_at) FROM card_events e WHERE e.card_id = t.card_id AND e.kind = ?)
		FROM tickets t
		WHERE `+conditions+`
		ORDER BY t.created_at DESC, t.card_id DESC
		LIMIT ?`, append([]any{string(model.EventCardCreated)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []model.TicketLogEntry{}
	for rows.Next() {
		ticket := &model.Ticket{}
		var channel string
		var cardCreatedAt sql.NullString
		if err := rows.Scan(&ticket.CardID, &ticket.RequesterPrincipalID, &channel,
			&ticket.SourceEntityType, &ticket.SourceEntityRef, &ticket.CreatedAt, &ticket.UpdatedAt,
			&cardCreatedAt); err != nil {
			return nil, err
		}
		ticket.Channel = model.TicketChannel(channel)
		entry := model.TicketLogEntry{TicketView: model.TicketView{Ticket: ticket}}
		if cardCreatedAt.Valid {
			parsed, err := parseStoredTime(cardCreatedAt.String)
			if err != nil {
				return nil, fmt.Errorf("card %s card_created time %q: %w", ticket.CardID, cardCreatedAt.String, err)
			}
			entry.CardCreatedAt = &parsed
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// TicketPlacementStates reads where the card sits on every board it is on, and
// what each of those columns means for a ticket. See model.TicketView for why
// this answers a list rather than one state.
func (s *Store) TicketPlacementStates(cardID string) ([]model.TicketPlacementState, error) {
	rows, err := s.db.Query(`
		SELECT p.board_id, p.column_id, c.name, c.lifecycle_state
		FROM placements p JOIN columns c ON c.id = p.column_id
		WHERE p.card_id = ?
		ORDER BY p.board_id`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []model.TicketPlacementState{}
	for rows.Next() {
		var state model.TicketPlacementState
		var lifecycle sql.NullString
		if err := rows.Scan(&state.BoardID, &state.ColumnID, &state.ColumnName, &lifecycle); err != nil {
			return nil, err
		}
		if lifecycle.Valid && lifecycle.String != "" {
			value := model.TicketLifecycleState(lifecycle.String)
			state.LifecycleState = &value
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// lifecycleStateValue renders a column's ticket lifecycle for storage; absent
// is stored as NULL, which is what "this column is not classified" means.
func lifecycleStateValue(state *model.TicketLifecycleState) any {
	if state == nil || *state == "" {
		return nil
	}
	return string(*state)
}

// lifecycleStateFromRequest reads the field a create or update request
// carries. The request validated the word already; an empty string clears.
func lifecycleStateFromRequest(raw *string) *model.TicketLifecycleState {
	if raw == nil || *raw == "" {
		return nil
	}
	state := model.TicketLifecycleState(*raw)
	return &state
}

// parseStoredTime reads a DATETIME that reached Go as text. The driver turns a
// DATETIME column into a time.Time itself, but not the result of an aggregate
// such as MIN(), which has no declared type; it wrote the text in one of these
// layouts.
func parseStoredTime(text string) (time.Time, error) {
	for _, layout := range sqlite3.SQLiteTimestampFormats {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not a time the sqlite driver writes")
}
