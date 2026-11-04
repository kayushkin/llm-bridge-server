package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ChatLink and ChatLinkMessage are the canonical types from
// llm-bridge/msg/chat_link.go.
type (
	ChatLink        = msg.ChatLink
	ChatLinkMessage = msg.ChatLinkMessage
)

const (
	chatLinkIDPrefix        = "chat_link_"
	chatLinkMessageIDPrefix = "chat_link_message_"
)

// ErrChatLinkNotFound is a link id this session does not hold.
var ErrChatLinkNotFound = errors.New("chat link not found")

// ErrChatLinkConversationTaken is a link to a conversation another active
// link already holds: one conversation reaches one session.
var ErrChatLinkConversationTaken = errors.New("another active chat link holds this conversation")

// migrateChatLinks creates the links between sessions and outside
// conversations, and the log of what went each way. Rows are never deleted:
// an ended link keeps its log.
func (s *Store) migrateChatLinks() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_links (
			id                       INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id               TEXT NOT NULL,
			account_id               TEXT NOT NULL,
			platform                 TEXT NOT NULL,
			conversation_id          TEXT NOT NULL,
			person_display_name      TEXT NOT NULL,
			created_by_principal_id  TEXT NOT NULL DEFAULT '',
			created_by_session_agent INTEGER NOT NULL DEFAULT 0,
			created_at               DATETIME NOT NULL,
			ended_at                 DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_chat_links_session ON chat_links(session_id, id);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_links_active_conversation
			ON chat_links(account_id, platform, conversation_id) WHERE ended_at IS NULL;
		CREATE TABLE IF NOT EXISTS chat_link_messages (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			link_id              INTEGER NOT NULL REFERENCES chat_links(id),
			direction            TEXT NOT NULL,
			state                TEXT NOT NULL,
			body                 TEXT NOT NULL,
			attachments          TEXT NOT NULL DEFAULT '[]',
			platform_message_ids TEXT NOT NULL DEFAULT '[]',
			bus_event_id         TEXT NOT NULL DEFAULT '',
			delivery_id          TEXT NOT NULL DEFAULT '',
			bridge_message_id    TEXT NOT NULL DEFAULT '',
			attempts             INTEGER NOT NULL DEFAULT 0,
			error                TEXT NOT NULL DEFAULT '',
			created_at           DATETIME NOT NULL,
			updated_at           DATETIME NOT NULL,
			next_attempt_at      DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_chat_link_messages_link ON chat_link_messages(link_id, id);
		CREATE INDEX IF NOT EXISTS idx_chat_link_messages_state ON chat_link_messages(state, direction);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_link_messages_bus_event
			ON chat_link_messages(link_id, bus_event_id) WHERE bus_event_id != '';
	`)
	return err
}

func chatLinkIDOf(rowID int64) string { return fmt.Sprintf("%s%06d", chatLinkIDPrefix, rowID) }

func chatLinkMessageIDOf(rowID int64) string {
	return fmt.Sprintf("%s%06d", chatLinkMessageIDPrefix, rowID)
}

func rowIDWithPrefix(id, prefix string) (int64, bool) {
	digits, hasPrefix := strings.CutPrefix(id, prefix)
	if !hasPrefix {
		return 0, false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	return rowID, err == nil && rowID > 0
}

// ChatLinkMessageRowID answers the row id of a chat_link_message_… id.
func ChatLinkMessageRowID(id string) (int64, bool) { return rowIDWithPrefix(id, chatLinkMessageIDPrefix) }

const chatLinkSelect = `SELECT id, session_id, account_id, platform, conversation_id, person_display_name,
	created_by_principal_id, created_by_session_agent, created_at, ended_at FROM chat_links`

func scanChatLink(scanner interface{ Scan(...any) error }) (ChatLink, error) {
	var link ChatLink
	var rowID int64
	var endedAt sql.NullTime
	if err := scanner.Scan(&rowID, &link.SessionID, &link.AccountID, &link.Platform, &link.ConversationID, &link.PersonDisplayName,
		&link.CreatedByPrincipalID, &link.CreatedBySessionAgent, &link.CreatedAt, &endedAt); err != nil {
		return ChatLink{}, err
	}
	link.ID = chatLinkIDOf(rowID)
	link.CreatedAt = link.CreatedAt.UTC()
	if endedAt.Valid {
		link.EndedAt = endedAt.Time.UTC()
	}
	return link, nil
}

// CreateChatLink records an active link. A conversation another active link
// holds is ErrChatLinkConversationTaken.
func (s *Store) CreateChatLink(link ChatLink) (ChatLink, error) {
	result, err := s.db.Exec(`INSERT INTO chat_links (session_id, account_id, platform, conversation_id, person_display_name,
		created_by_principal_id, created_by_session_agent, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		link.SessionID, link.AccountID, link.Platform, link.ConversationID, link.PersonDisplayName,
		link.CreatedByPrincipalID, link.CreatedBySessionAgent, link.CreatedAt.UTC())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ChatLink{}, ErrChatLinkConversationTaken
		}
		return ChatLink{}, err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return ChatLink{}, err
	}
	return scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE id = ?`, rowID))
}

// GetChatLink answers one of a session's links.
func (s *Store) GetChatLink(sessionID, linkID string) (ChatLink, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return ChatLink{}, ErrChatLinkNotFound
	}
	link, err := scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE session_id = ? AND id = ?`, sessionID, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return ChatLink{}, ErrChatLinkNotFound
	}
	return link, err
}

func (s *Store) getChatLinkByRowID(rowID int64) (ChatLink, error) {
	return scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE id = ?`, rowID))
}

// ListChatLinks answers a session's links, active and ended, oldest first.
func (s *Store) ListChatLinks(sessionID string) ([]ChatLink, error) {
	return s.queryChatLinks(chatLinkSelect+` WHERE session_id = ? ORDER BY id`, sessionID)
}

// ActiveChatLinks answers every link not ended, oldest first.
func (s *Store) ActiveChatLinks() ([]ChatLink, error) {
	return s.queryChatLinks(chatLinkSelect + ` WHERE ended_at IS NULL ORDER BY id`)
}

func (s *Store) queryChatLinks(query string, arguments ...any) ([]ChatLink, error) {
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []ChatLink{}
	for rows.Next() {
		link, err := scanChatLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// ActiveChatLinkFor answers the active link holding a conversation, or false.
func (s *Store) ActiveChatLinkFor(accountID, platform, conversationID string) (ChatLink, bool, error) {
	link, err := scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE account_id = ? AND platform = ? AND conversation_id = ? AND ended_at IS NULL`,
		accountID, platform, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return ChatLink{}, false, nil
	}
	if err != nil {
		return ChatLink{}, false, err
	}
	return link, true, nil
}

// EndChatLink ends a link, keeping its log. Ending an ended link changes
// nothing.
func (s *Store) EndChatLink(sessionID, linkID string, endedAt time.Time) (ChatLink, error) {
	link, err := s.GetChatLink(sessionID, linkID)
	if err != nil {
		return ChatLink{}, err
	}
	rowID, _ := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if _, err := s.db.Exec(`UPDATE chat_links SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, endedAt.UTC(), rowID); err != nil {
		return ChatLink{}, err
	}
	if link, err = s.GetChatLink(sessionID, linkID); err != nil {
		return ChatLink{}, err
	}
	return link, nil
}

const chatLinkMessageSelect = `SELECT id, link_id, direction, state, body, attachments, platform_message_ids, bus_event_id,
	delivery_id, bridge_message_id, attempts, error, created_at, updated_at, next_attempt_at FROM chat_link_messages`

func scanChatLinkMessage(scanner interface{ Scan(...any) error }) (ChatLinkMessage, error) {
	var message ChatLinkMessage
	var rowID, linkRowID int64
	var direction, state, attachments, platformMessageIDs string
	var nextAttemptAt sql.NullTime
	if err := scanner.Scan(&rowID, &linkRowID, &direction, &state, &message.Body, &attachments, &platformMessageIDs, &message.BusEventID,
		&message.DeliveryID, &message.BridgeMessageID, &message.Attempts, &message.Error, &message.CreatedAt, &message.UpdatedAt, &nextAttemptAt); err != nil {
		return ChatLinkMessage{}, err
	}
	message.ID = chatLinkMessageIDOf(rowID)
	message.LinkID = chatLinkIDOf(linkRowID)
	message.Direction = msg.ChatLinkDirection(direction)
	message.State = msg.ChatLinkMessageState(state)
	message.CreatedAt = message.CreatedAt.UTC()
	message.UpdatedAt = message.UpdatedAt.UTC()
	if nextAttemptAt.Valid {
		message.NextAttemptAt = nextAttemptAt.Time.UTC()
	}
	if err := json.Unmarshal([]byte(attachments), &message.Attachments); err != nil {
		return ChatLinkMessage{}, fmt.Errorf("chat link message %s attachments: %w", message.ID, err)
	}
	if err := json.Unmarshal([]byte(platformMessageIDs), &message.PlatformMessageIDs); err != nil {
		return ChatLinkMessage{}, fmt.Errorf("chat link message %s platform ids: %w", message.ID, err)
	}
	return message, nil
}

func (s *Store) queryChatLinkMessages(query string, arguments ...any) ([]ChatLinkMessage, error) {
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []ChatLinkMessage{}
	for rows.Next() {
		message, err := scanChatLinkMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// ChatLinkMessages answers a link's log, oldest first.
func (s *Store) ChatLinkMessages(linkID string) ([]ChatLinkMessage, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return nil, ErrChatLinkNotFound
	}
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = ? ORDER BY id`, rowID)
}

// ChatLinkMessagesInState answers a link's messages one way in one state,
// oldest first.
func (s *Store) ChatLinkMessagesInState(linkID string, direction msg.ChatLinkDirection, state msg.ChatLinkMessageState) ([]ChatLinkMessage, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return nil, ErrChatLinkNotFound
	}
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = ? AND direction = ? AND state = ? ORDER BY id`,
		rowID, string(direction), string(state))
}

// ChatLinkMessagesOfDelivery answers the messages one delivery id ties
// together, oldest first.
func (s *Store) ChatLinkMessagesOfDelivery(deliveryID string) ([]ChatLinkMessage, error) {
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE delivery_id = ? ORDER BY id`, deliveryID)
}

// ChatLinkOfMessage answers the link a message belongs to.
func (s *Store) ChatLinkOfMessage(message ChatLinkMessage) (ChatLink, error) {
	rowID, ok := rowIDWithPrefix(message.LinkID, chatLinkIDPrefix)
	if !ok {
		return ChatLink{}, ErrChatLinkNotFound
	}
	return s.getChatLinkByRowID(rowID)
}

// AddInboundChatLinkMessage keeps a message the person sent, pending. A bus
// event already kept for the link (JetStream redelivers) adds nothing and
// answers false.
func (s *Store) AddInboundChatLinkMessage(linkID, busEventID, body string, attachments []msg.ChatLinkAttachment, platformMessageID string, at time.Time) (bool, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return false, ErrChatLinkNotFound
	}
	if attachments == nil {
		attachments = []msg.ChatLinkAttachment{}
	}
	encodedAttachments, err := json.Marshal(attachments)
	if err != nil {
		return false, err
	}
	platformMessageIDs := []string{}
	if platformMessageID != "" {
		platformMessageIDs = append(platformMessageIDs, platformMessageID)
	}
	encodedIDs, err := json.Marshal(platformMessageIDs)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO chat_link_messages (link_id, direction, state, body, attachments, platform_message_ids, bus_event_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		rowID, string(msg.ChatLinkInbound), string(msg.ChatLinkMessagePending), body, string(encodedAttachments), string(encodedIDs), busEventID, at.UTC(), at.UTC())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// MarkChatLinkMessagesInSession records that pending inbound messages went
// into the session together, as one turn.
func (s *Store) MarkChatLinkMessagesInSession(messageIDs []string, deliveryID, bridgeMessageID string, at time.Time) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, messageID := range messageIDs {
		rowID, ok := ChatLinkMessageRowID(messageID)
		if !ok {
			return fmt.Errorf("not a chat link message id: %q", messageID)
		}
		result, err := transaction.Exec(`UPDATE chat_link_messages SET state = ?, delivery_id = ?, bridge_message_id = ?, attempts = attempts + 1, error = '', updated_at = ?
			WHERE id = ? AND state = ?`, string(msg.ChatLinkMessageInSession), deliveryID, bridgeMessageID, at.UTC(), rowID, string(msg.ChatLinkMessagePending))
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return fmt.Errorf("chat link message %s is no longer pending", messageID)
		}
	}
	return transaction.Commit()
}

// RecordChatLinkDeliveryAttemptFailed keeps pending inbound messages pending,
// counting the failed attempt and its reason.
func (s *Store) RecordChatLinkDeliveryAttemptFailed(messageIDs []string, reason string, at time.Time) error {
	for _, messageID := range messageIDs {
		rowID, ok := ChatLinkMessageRowID(messageID)
		if !ok {
			return fmt.Errorf("not a chat link message id: %q", messageID)
		}
		if _, err := s.db.Exec(`UPDATE chat_link_messages SET attempts = attempts + 1, error = ?, updated_at = ? WHERE id = ?`,
			reason, at.UTC(), rowID); err != nil {
			return err
		}
	}
	return nil
}

// SetChatLinkMessageState moves one message to state, with the reason when
// it failed. platformMessageIDs, when not nil, replaces the stored ones.
func (s *Store) SetChatLinkMessageState(messageID string, state msg.ChatLinkMessageState, reason string, platformMessageIDs []string, at time.Time) error {
	rowID, ok := ChatLinkMessageRowID(messageID)
	if !ok {
		return fmt.Errorf("not a chat link message id: %q", messageID)
	}
	if platformMessageIDs == nil {
		_, err := s.db.Exec(`UPDATE chat_link_messages SET state = ?, error = ?, updated_at = ?, next_attempt_at = NULL WHERE id = ?`,
			string(state), reason, at.UTC(), rowID)
		return err
	}
	encoded, err := json.Marshal(platformMessageIDs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chat_link_messages SET state = ?, error = ?, platform_message_ids = ?, updated_at = ?, next_attempt_at = NULL WHERE id = ?`,
		string(state), reason, string(encoded), at.UTC(), rowID)
	return err
}

// AddOutboundChatLinkMessage keeps a turn's final reply for the person, in
// state (pending to be sent, or failed with reason when there is nothing to
// send). A delivery has one reply: a second for the same delivery id is the
// first one, answered as it is.
func (s *Store) AddOutboundChatLinkMessage(linkID, deliveryID, body string, state msg.ChatLinkMessageState, reason string, at time.Time) (ChatLinkMessage, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return ChatLinkMessage{}, ErrChatLinkNotFound
	}
	existing, err := s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = ? AND direction = ? AND delivery_id = ?`,
		rowID, string(msg.ChatLinkOutbound), deliveryID)
	if err != nil {
		return ChatLinkMessage{}, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	result, err := s.db.Exec(`INSERT INTO chat_link_messages (link_id, direction, state, body, delivery_id, error, created_at, updated_at, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rowID, string(msg.ChatLinkOutbound), string(state), body, deliveryID, reason, at.UTC(), at.UTC(), at.UTC())
	if err != nil {
		return ChatLinkMessage{}, err
	}
	messageRowID, err := result.LastInsertId()
	if err != nil {
		return ChatLinkMessage{}, err
	}
	return scanChatLinkMessage(s.db.QueryRow(chatLinkMessageSelect+` WHERE id = ?`, messageRowID))
}

// RecordChatLinkSendFailed counts a failed send of an outbound message and
// sets when to try again.
func (s *Store) RecordChatLinkSendFailed(messageID, reason string, nextAttemptAt, at time.Time) error {
	rowID, ok := ChatLinkMessageRowID(messageID)
	if !ok {
		return fmt.Errorf("not a chat link message id: %q", messageID)
	}
	_, err := s.db.Exec(`UPDATE chat_link_messages SET attempts = attempts + 1, error = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?`,
		reason, nextAttemptAt.UTC(), at.UTC(), rowID)
	return err
}

// DueOutboundChatLinkMessages answers pending replies whose next attempt is
// due, oldest first, whether or not their link has since ended: a reply the
// session wrote before the end is still owed.
func (s *Store) DueOutboundChatLinkMessages(now time.Time) ([]ChatLinkMessage, error) {
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE direction = ? AND state = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY id`,
		string(msg.ChatLinkOutbound), string(msg.ChatLinkMessagePending), now.UTC())
}
