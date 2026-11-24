package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/kanban-store/internal/model"
)

func migrateMessaging(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS message_triggers (
			id                     TEXT PRIMARY KEY,
			board_id               TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			name                   TEXT NOT NULL,
			event_kind             TEXT NOT NULL,
			to_column_id           TEXT NOT NULL DEFAULT '',
			priority_value         INTEGER,               -- NULL = any priority
			recipient_user_id      TEXT NOT NULL,
			recipient_display_name TEXT NOT NULL DEFAULT '',
			message_template       TEXT NOT NULL,
			enabled                INTEGER NOT NULL DEFAULT 1,
			created_at             TEXT NOT NULL,
			updated_at             TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_message_triggers_board ON message_triggers(board_id);

		CREATE TABLE IF NOT EXISTS message_deliveries (
			id                TEXT PRIMARY KEY,
			trigger_id        TEXT NOT NULL REFERENCES message_triggers(id) ON DELETE CASCADE,
			board_id          TEXT NOT NULL,
			event_id          TEXT NOT NULL,
			card_id           TEXT NOT NULL,
			recipient_user_id TEXT NOT NULL,
			rendered_message  TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL,
			error             TEXT NOT NULL DEFAULT '',
			room_id           TEXT NOT NULL DEFAULT '',
			matrix_event_id   TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL,
			UNIQUE (trigger_id, event_id)
		);
		CREATE INDEX IF NOT EXISTS idx_message_deliveries_board ON message_deliveries(board_id, created_at DESC);
	`)
	if err != nil {
		return fmt.Errorf("migrate messaging: %w", err)
	}
	return nil
}

const messageTriggerColumns = `id, board_id, name, event_kind, to_column_id, priority_value, recipient_user_id, recipient_display_name, message_template, enabled, created_at, updated_at`

func scanMessageTrigger(row interface{ Scan(...any) error }) (*model.MessageTrigger, error) {
	var t model.MessageTrigger
	var priority sql.NullInt64
	var createdAt, updatedAt string
	if err := row.Scan(&t.ID, &t.BoardID, &t.Name, &t.EventKind, &t.ToColumnID, &priority, &t.RecipientUserID,
		&t.RecipientDisplayName, &t.MessageTemplate, &t.Enabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if priority.Valid {
		v := int(priority.Int64)
		t.PriorityValue = &v
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &t, nil
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Store) CreateMessageTrigger(t *model.MessageTrigger) (*model.MessageTrigger, error) {
	now := time.Now().UTC()
	t.ID = uuid.NewString()
	t.CreatedAt, t.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO message_triggers (`+messageTriggerColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.BoardID, t.Name, t.EventKind, t.ToColumnID, nullableInt(t.PriorityValue), t.RecipientUserID,
		t.RecipientDisplayName, t.MessageTemplate, t.Enabled, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("create message trigger: %w", err)
	}
	return s.GetMessageTrigger(t.ID)
}

func (s *Store) GetMessageTrigger(id string) (*model.MessageTrigger, error) {
	t, err := scanMessageTrigger(s.db.QueryRow(`SELECT `+messageTriggerColumns+` FROM message_triggers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get message trigger %s: %w", id, err)
	}
	return t, nil
}

// ListMessageTriggers lists a board's triggers, oldest first. enabledOnly is
// what the dispatcher asks for; the settings page wants all of them.
func (s *Store) ListMessageTriggers(boardID string, enabledOnly bool) ([]*model.MessageTrigger, error) {
	query := `SELECT ` + messageTriggerColumns + ` FROM message_triggers WHERE board_id = ?`
	if enabledOnly {
		query += ` AND enabled = 1`
	}
	rows, err := s.db.Query(query+` ORDER BY created_at, id`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list message triggers: %w", err)
	}
	defer rows.Close()
	out := []*model.MessageTrigger{}
	for rows.Next() {
		t, err := scanMessageTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message trigger: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateMessageTrigger(t *model.MessageTrigger) (*model.MessageTrigger, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE message_triggers SET name=?, event_kind=?, to_column_id=?, priority_value=?, recipient_user_id=?,
		recipient_display_name=?, message_template=?, enabled=?, updated_at=? WHERE id = ?`,
		t.Name, t.EventKind, t.ToColumnID, nullableInt(t.PriorityValue), t.RecipientUserID, t.RecipientDisplayName,
		t.MessageTemplate, t.Enabled, now.Format(time.RFC3339Nano), t.ID)
	if err != nil {
		return nil, fmt.Errorf("update message trigger %s: %w", t.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetMessageTrigger(t.ID)
}

func (s *Store) DeleteMessageTrigger(id string) error {
	res, err := s.db.Exec(`DELETE FROM message_triggers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete message trigger %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrAlreadyDelivered means this trigger already has a delivery row for this
// event: the dispatcher saw the event twice and must not send twice.
var ErrAlreadyDelivered = errors.New("already delivered")

// ClaimMessageDelivery inserts the delivery row; the UNIQUE (trigger_id,
// event_id) index makes the insert itself the claim.
func (s *Store) ClaimMessageDelivery(d *model.MessageDelivery) error {
	d.ID = uuid.NewString()
	d.CreatedAt = time.Now().UTC()
	res, err := s.db.Exec(`INSERT OR IGNORE INTO message_deliveries
		(id, trigger_id, board_id, event_id, card_id, recipient_user_id, rendered_message, status, error, room_id, matrix_event_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.TriggerID, d.BoardID, d.EventID, d.CardID, d.RecipientUserID, d.RenderedMessage, d.Status, d.Error, d.RoomID, d.MatrixEventID,
		d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("claim message delivery: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAlreadyDelivered
	}
	return nil
}

// FinishMessageDelivery records the outcome of a claimed delivery.
func (s *Store) FinishMessageDelivery(id string, status model.MessageDeliveryStatus, deliveryError, roomID, matrixEventID string) error {
	if _, err := s.db.Exec(`UPDATE message_deliveries SET status=?, error=?, room_id=?, matrix_event_id=? WHERE id = ?`,
		status, deliveryError, roomID, matrixEventID, id); err != nil {
		return fmt.Errorf("finish message delivery %s: %w", id, err)
	}
	return nil
}

func (s *Store) ListMessageDeliveries(boardID string, limit int) ([]*model.MessageDelivery, error) {
	rows, err := s.db.Query(`SELECT id, trigger_id, board_id, event_id, card_id, recipient_user_id, rendered_message, status, error, room_id, matrix_event_id, created_at
		FROM message_deliveries WHERE board_id = ? ORDER BY created_at DESC, id LIMIT ?`, boardID, limit)
	if err != nil {
		return nil, fmt.Errorf("list message deliveries: %w", err)
	}
	defer rows.Close()
	out := []*model.MessageDelivery{}
	for rows.Next() {
		var d model.MessageDelivery
		var createdAt string
		if err := rows.Scan(&d.ID, &d.TriggerID, &d.BoardID, &d.EventID, &d.CardID, &d.RecipientUserID, &d.RenderedMessage, &d.Status, &d.Error, &d.RoomID, &d.MatrixEventID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan message delivery: %w", err)
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, &d)
	}
	return out, rows.Err()
}
