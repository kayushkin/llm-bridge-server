package store

// Messages this server held instead of sending, because it was waiting for
// running work to finish before a restart (internal/server/restart_drain.go).
// A held message is sent once the wait ends: by this process when the wait is
// called off or runs out, or by the restarted server at start. A row is sent
// at most once: a delivery claims it first, and only an unclaimed row can be
// claimed.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The outcomes a held message's delivery records.
const (
	HeldMessageDelivered      = "delivered"
	HeldMessageDeliveryFailed = "failed"
	// HeldMessageHeldAgain: a new drain began while the message was being
	// sent, so the send held it again under a new id.
	HeldMessageHeldAgain = "held_again"
)

// HeldMessage is one message held while the server waited to restart.
type HeldMessage struct {
	HeldMessageID string
	SessionID     string
	// Request is the send exactly as it arrived.
	Request msg.SendMessageRequest
	// Source names the path that would have sent it: "send" for
	// POST /sessions/{id}/send, "signal_answer" for an answered question.
	Source string
	HeldAt time.Time
}

func (s *Store) migrateHeldMessages() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS held_messages (
		held_message_id      TEXT PRIMARY KEY,
		session_id           TEXT NOT NULL,
		request_json         TEXT NOT NULL,
		source               TEXT NOT NULL,
		held_at              DATETIME NOT NULL,
		delivery_claimed_at  DATETIME,
		delivered_at         DATETIME,
		delivery_outcome     TEXT NOT NULL DEFAULT '',
		delivery_status_code INTEGER NOT NULL DEFAULT 0,
		delivery_answer      TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS held_messages_unclaimed ON held_messages (held_message_id) WHERE delivery_claimed_at IS NULL`)
	return err
}

// HoldMessage records a message the server will send once its restart wait
// ends.
func (s *Store) HoldMessage(held HeldMessage) error {
	request, err := json.Marshal(held.Request)
	if err != nil {
		return fmt.Errorf("encode held message %s: %w", held.HeldMessageID, err)
	}
	_, err = s.db.Exec(`INSERT INTO held_messages (held_message_id, session_id, request_json, source, held_at) VALUES (?, ?, ?, ?, ?)`,
		held.HeldMessageID, held.SessionID, string(request), held.Source, held.HeldAt.UTC())
	if err != nil {
		return fmt.Errorf("hold message %s: %w", held.HeldMessageID, err)
	}
	return nil
}

// UnclaimedHeldMessages lists the held messages no delivery has claimed yet,
// oldest first, which is the order they were sent in.
func (s *Store) UnclaimedHeldMessages() ([]HeldMessage, error) {
	rows, err := s.db.Query(`SELECT held_message_id, session_id, request_json, source, held_at FROM held_messages
		WHERE delivery_claimed_at IS NULL ORDER BY held_message_id`)
	if err != nil {
		return nil, fmt.Errorf("list unclaimed held messages: %w", err)
	}
	defer rows.Close()
	var held []HeldMessage
	for rows.Next() {
		var message HeldMessage
		var request string
		if err := rows.Scan(&message.HeldMessageID, &message.SessionID, &request, &message.Source, &message.HeldAt); err != nil {
			return nil, fmt.Errorf("read held message: %w", err)
		}
		if err := json.Unmarshal([]byte(request), &message.Request); err != nil {
			return nil, fmt.Errorf("decode held message %s: %w", message.HeldMessageID, err)
		}
		held = append(held, message)
	}
	return held, rows.Err()
}

// ClaimHeldMessage marks a held message as being delivered, and reports false
// when another delivery claimed it first.
func (s *Store) ClaimHeldMessage(heldMessageID string) (bool, error) {
	result, err := s.db.Exec(`UPDATE held_messages SET delivery_claimed_at = ? WHERE held_message_id = ? AND delivery_claimed_at IS NULL`,
		time.Now().UTC(), heldMessageID)
	if err != nil {
		return false, fmt.Errorf("claim held message %s: %w", heldMessageID, err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return claimed == 1, nil
}

// RecordHeldMessageDelivery records how a claimed held message's delivery
// went: the outcome, the status code the send answered and its body.
func (s *Store) RecordHeldMessageDelivery(heldMessageID, outcome string, statusCode int, answer string) error {
	result, err := s.db.Exec(`UPDATE held_messages SET delivered_at = ?, delivery_outcome = ?, delivery_status_code = ?, delivery_answer = ?
		WHERE held_message_id = ? AND delivery_claimed_at IS NOT NULL`,
		time.Now().UTC(), outcome, statusCode, answer, heldMessageID)
	if err != nil {
		return fmt.Errorf("record delivery of held message %s: %w", heldMessageID, err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("record delivery of held message %s: %w", heldMessageID, sql.ErrNoRows)
	}
	return nil
}
