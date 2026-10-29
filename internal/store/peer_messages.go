package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Messages between connected servers (internal/server/peer_messages.go): one
// row per message, outgoing or incoming, as this server holds it. An
// incoming message is unique per (connection, the sender's id), so a delivery
// the sender retries after a lost acknowledgement is recorded once.

const peerMessageIDPrefix = "peer_message_"

// PeerMessage is one row of peer_messages.
type PeerMessage = msg.PeerMessage

// ErrPeerMessageNotFound is a message id this server has no row for.
var ErrPeerMessageNotFound = errors.New("peer message not found")

// ErrPeerMessageNotPending is a decision on a message that is not waiting
// for one.
var ErrPeerMessageNotPending = errors.New("peer message is not pending")

func (s *Store) migratePeerMessages() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS peer_messages (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_id      TEXT NOT NULL,
			direction          TEXT NOT NULL,
			kind               TEXT NOT NULL,
			body               TEXT NOT NULL,
			in_reply_to        TEXT NOT NULL DEFAULT '',
			remote_message_id  TEXT NOT NULL DEFAULT '',
			state              TEXT NOT NULL,
			created_by         TEXT NOT NULL DEFAULT '',
			sender_server_name TEXT NOT NULL DEFAULT '',
			created_at         DATETIME NOT NULL,
			updated_at         DATETIME NOT NULL,
			delivered_at       DATETIME,
			attempts           INTEGER NOT NULL DEFAULT 0,
			last_error         TEXT NOT NULL DEFAULT '',
			next_attempt_at    DATETIME,
			decided_by         TEXT NOT NULL DEFAULT '',
			decided_at         DATETIME,
			decision_reason    TEXT NOT NULL DEFAULT '',
			session_id         TEXT NOT NULL DEFAULT '',
			operation          TEXT NOT NULL DEFAULT '',
			status_message_id  TEXT NOT NULL DEFAULT '',
			signal_id          TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_peer_messages_connection ON peer_messages(connection_id, id);
		CREATE INDEX IF NOT EXISTS idx_peer_messages_state ON peer_messages(direction, state);
		CREATE INDEX IF NOT EXISTS idx_peer_messages_session ON peer_messages(session_id) WHERE session_id != '';
		CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_messages_incoming ON peer_messages(connection_id, remote_message_id)
			WHERE direction = 'incoming' AND remote_message_id != '';
	`)
	return err
}

const peerMessageSelect = `SELECT id, connection_id, direction, kind, body, in_reply_to, remote_message_id, state, created_by, sender_server_name,
	created_at, updated_at, delivered_at, attempts, last_error, next_attempt_at, decided_by, decided_at, decision_reason,
	session_id, operation, status_message_id, signal_id FROM peer_messages`

func scanPeerMessage(row rowScanner) (PeerMessage, error) {
	var message PeerMessage
	var rowID int64
	var body, operation, direction, kind, state string
	var deliveredAt, nextAttemptAt, decidedAt sql.NullTime
	if err := row.Scan(&rowID, &message.ConnectionID, &direction, &kind, &body, &message.InReplyTo, &message.RemoteMessageID, &state,
		&message.CreatedBy, &message.SenderServerName, &message.CreatedAt, &message.UpdatedAt, &deliveredAt, &message.Attempts, &message.LastError,
		&nextAttemptAt, &message.DecidedBy, &decidedAt, &message.DecisionReason, &message.SessionID, &operation, &message.StatusMessageID,
		&message.SignalID); err != nil {
		return PeerMessage{}, err
	}
	message.ID = prefixedIDOf(peerMessageIDPrefix, rowID)
	message.Direction = msg.PeerMessageDirection(direction)
	message.Kind = msg.PeerMessageKind(kind)
	message.State = msg.PeerMessageState(state)
	if err := json.Unmarshal([]byte(body), &message.Body); err != nil {
		return PeerMessage{}, fmt.Errorf("peer message %s holds a body this server cannot read: %w", message.ID, err)
	}
	message.DeliveredAt = timeOrNil(deliveredAt)
	message.NextAttemptAt = timeOrNil(nextAttemptAt)
	message.DecidedAt = timeOrNil(decidedAt)
	if operation != "" {
		var recorded msg.PeerMessageOperation
		if err := json.Unmarshal([]byte(operation), &recorded); err != nil {
			return PeerMessage{}, fmt.Errorf("peer message %s holds an operation this server cannot read: %w", message.ID, err)
		}
		message.Operation = &recorded
	}
	return message, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

// InsertPeerMessage records a message and gives it its id.
func (s *Store) InsertPeerMessage(message *PeerMessage) error {
	if message.ConnectionID == "" || message.Direction == "" || message.Kind == "" || message.State == "" {
		return fmt.Errorf("insert peer message: connection, direction, kind and state are all required")
	}
	body, err := json.Marshal(message.Body)
	if err != nil {
		return fmt.Errorf("insert peer message on %s: encode body: %w", message.ConnectionID, err)
	}
	now := time.Now().UTC()
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	message.UpdatedAt = now
	result, err := s.db.Exec(`INSERT INTO peer_messages (connection_id, direction, kind, body, in_reply_to, remote_message_id, state, created_by,
		sender_server_name, created_at, updated_at, next_attempt_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ConnectionID, string(message.Direction), string(message.Kind), string(body), message.InReplyTo, message.RemoteMessageID,
		string(message.State), message.CreatedBy, message.SenderServerName, message.CreatedAt.UTC(), message.UpdatedAt, nullableTime(message.NextAttemptAt))
	if err != nil {
		return fmt.Errorf("insert peer message on %s: %w", message.ConnectionID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	message.ID = prefixedIDOf(peerMessageIDPrefix, rowID)
	return nil
}

// GetPeerMessage returns one message by its id on this server.
func (s *Store) GetPeerMessage(id string) (PeerMessage, error) {
	rowID, ok := rowIDOfPrefixed(peerMessageIDPrefix, id)
	if !ok {
		return PeerMessage{}, ErrPeerMessageNotFound
	}
	found, err := scanPeerMessage(s.db.QueryRow(peerMessageSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrPeerMessageNotFound)
}

// IncomingPeerMessageBySenderID returns the incoming message a connection's
// other server sent under its own id remoteID.
func (s *Store) IncomingPeerMessageBySenderID(connectionID, remoteID string) (PeerMessage, error) {
	found, err := scanPeerMessage(s.db.QueryRow(peerMessageSelect+` WHERE connection_id = ? AND direction = 'incoming' AND remote_message_id = ?`, connectionID, remoteID))
	return notFoundAs(found, err, ErrPeerMessageNotFound)
}

// PeerMessageFilter narrows ListPeerMessages; an empty field matches all.
type PeerMessageFilter struct {
	ConnectionID string
	Direction    msg.PeerMessageDirection
	State        msg.PeerMessageState
	Kind         msg.PeerMessageKind
	SessionID    string
	Limit        int
}

// ListPeerMessages returns messages, newest first.
func (s *Store) ListPeerMessages(filter PeerMessageFilter) ([]PeerMessage, error) {
	var conditions []string
	var arguments []any
	for _, condition := range []struct {
		column, value string
	}{
		{"connection_id", filter.ConnectionID}, {"direction", string(filter.Direction)}, {"state", string(filter.State)},
		{"kind", string(filter.Kind)}, {"session_id", filter.SessionID},
	} {
		if condition.value != "" {
			conditions = append(conditions, condition.column+" = ?")
			arguments = append(arguments, condition.value)
		}
	}
	query := peerMessageSelect
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id DESC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []PeerMessage{}
	for rows.Next() {
		message, err := scanPeerMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// QueuedPeerMessages returns every outgoing message still queued on one
// connection, oldest first, whatever its next attempt: what a socket that
// has just opened sends at once.
func (s *Store) QueuedPeerMessages(connectionID string) ([]PeerMessage, error) {
	return s.queuedPeerMessages(connectionID, nil)
}

// QueuedPeerMessagesDue returns the outgoing messages still queued whose
// next attempt is due by now, oldest first, on one connection or every one.
func (s *Store) QueuedPeerMessagesDue(connectionID string, now time.Time) ([]PeerMessage, error) {
	return s.queuedPeerMessages(connectionID, &now)
}

func (s *Store) queuedPeerMessages(connectionID string, dueBy *time.Time) ([]PeerMessage, error) {
	query := peerMessageSelect + ` WHERE direction = 'outgoing' AND state = 'queued'`
	arguments := []any{}
	if dueBy != nil {
		query += ` AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`
		arguments = append(arguments, dueBy.UTC())
	}
	if connectionID != "" {
		query += ` AND connection_id = ?`
		arguments = append(arguments, connectionID)
	}
	rows, err := s.db.Query(query+` ORDER BY id`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []PeerMessage
	for rows.Next() {
		message, err := scanPeerMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) updatePeerMessage(id, assignments string, arguments ...any) error {
	rowID, ok := rowIDOfPrefixed(peerMessageIDPrefix, id)
	if !ok {
		return ErrPeerMessageNotFound
	}
	arguments = append(append([]any{time.Now().UTC()}, arguments...), rowID)
	result, err := s.db.Exec(`UPDATE peer_messages SET updated_at = ?, `+assignments+` WHERE id = ?`, arguments...)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return ErrPeerMessageNotFound
	}
	return nil
}

// RecordPeerMessageDelivered marks an outgoing message delivered, with the
// receiver's id for it.
func (s *Store) RecordPeerMessageDelivered(id, remoteID string, at time.Time) error {
	return s.updatePeerMessage(id, `state = 'delivered', remote_message_id = ?, delivered_at = ?, attempts = attempts + 1, last_error = '', next_attempt_at = NULL`,
		remoteID, at.UTC())
}

// RecordPeerMessageAttemptFailed keeps an outgoing message queued after a
// failed attempt, to be tried again at next.
func (s *Store) RecordPeerMessageAttemptFailed(id, reason string, next time.Time) error {
	return s.updatePeerMessage(id, `attempts = attempts + 1, last_error = ?, next_attempt_at = ?`, reason, next.UTC())
}

// MarkPeerMessageFailed ends an outgoing message the other server refused.
func (s *Store) MarkPeerMessageFailed(id, reason string) error {
	return s.updatePeerMessage(id, `state = 'failed', attempts = attempts + 1, last_error = ?, next_attempt_at = NULL`, reason)
}

// DecidePeerMessage moves a pending incoming message to approved or
// declined, once: a second decision is ErrPeerMessageNotPending.
func (s *Store) DecidePeerMessage(id string, to msg.PeerMessageState, decidedBy, reason string, at time.Time) (PeerMessage, error) {
	if to != msg.PeerMessageStateApproved && to != msg.PeerMessageStateDeclined {
		return PeerMessage{}, fmt.Errorf("a decision is approved or declined, not %s", to)
	}
	rowID, ok := rowIDOfPrefixed(peerMessageIDPrefix, id)
	if !ok {
		return PeerMessage{}, ErrPeerMessageNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_messages SET state = ?, decided_by = ?, decided_at = ?, decision_reason = ?, updated_at = ?
		WHERE id = ? AND direction = 'incoming' AND state = 'pending'`, string(to), decidedBy, at.UTC(), reason, time.Now().UTC(), rowID)
	if err != nil {
		return PeerMessage{}, err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		if _, err := s.GetPeerMessage(id); err != nil {
			return PeerMessage{}, err
		}
		return PeerMessage{}, ErrPeerMessageNotPending
	}
	return s.GetPeerMessage(id)
}

// ReturnPeerMessageToPending undoes an approval whose action could not be
// taken, so the message waits again.
func (s *Store) ReturnPeerMessageToPending(id string) error {
	return s.updatePeerMessage(id, `state = 'pending', decided_by = '', decided_at = NULL, decision_reason = ''`)
}

// SetPeerMessageSession records the session an approved request started.
func (s *Store) SetPeerMessageSession(id, sessionID string) error {
	return s.updatePeerMessage(id, `session_id = ?`, sessionID)
}

// SetPeerMessageOperation records the update an approved notice ran.
func (s *Store) SetPeerMessageOperation(id string, operation msg.PeerMessageOperation) error {
	data, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	return s.updatePeerMessage(id, `operation = ?`, string(data))
}

// SetPeerMessageSignal records the signal that shows an incoming message.
func (s *Store) SetPeerMessageSignal(id, signalID string) error {
	return s.updatePeerMessage(id, `signal_id = ?`, signalID)
}

// FinishPeerMessage marks an incoming message done, naming the status this
// server sent in answer, if any.
func (s *Store) FinishPeerMessage(id, statusMessageID string) error {
	return s.updatePeerMessage(id, `state = 'done', status_message_id = ?`, statusMessageID)
}

// SetPeerMessageStatusMessage names the status sent in answer without
// changing the message's state (a declined one stays declined).
func (s *Store) SetPeerMessageStatusMessage(id, statusMessageID string) error {
	return s.updatePeerMessage(id, `status_message_id = ?`, statusMessageID)
}
