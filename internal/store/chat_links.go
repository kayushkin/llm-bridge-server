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
func ChatLinkMessageRowID(id string) (int64, bool) {
	return rowIDWithPrefix(id, chatLinkMessageIDPrefix)
}

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
	var rowID int64
	err := s.db.QueryRow(`INSERT INTO chat_links (session_id, account_id, platform, conversation_id, person_display_name,
		created_by_principal_id, created_by_session_agent, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		link.SessionID, link.AccountID, link.Platform, link.ConversationID, link.PersonDisplayName,
		link.CreatedByPrincipalID, link.CreatedBySessionAgent, link.CreatedAt.UTC()).Scan(&rowID)
	if err != nil {
		if isUniqueViolation(err) {
			return ChatLink{}, ErrChatLinkConversationTaken
		}
		return ChatLink{}, err
	}
	return scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE id = $1`, rowID))
}

// GetChatLink answers one of a session's links.
func (s *Store) GetChatLink(sessionID, linkID string) (ChatLink, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return ChatLink{}, ErrChatLinkNotFound
	}
	link, err := scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE session_id = $1 AND id = $2`, sessionID, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return ChatLink{}, ErrChatLinkNotFound
	}
	return link, err
}

func (s *Store) getChatLinkByRowID(rowID int64) (ChatLink, error) {
	return scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE id = $1`, rowID))
}

// ListChatLinks answers a session's links, active and ended, oldest first.
func (s *Store) ListChatLinks(sessionID string) ([]ChatLink, error) {
	return s.queryChatLinks(chatLinkSelect+` WHERE session_id = $1 ORDER BY id`, sessionID)
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
	link, err := scanChatLink(s.db.QueryRow(chatLinkSelect+` WHERE account_id = $1 AND platform = $2 AND conversation_id = $3 AND ended_at IS NULL`,
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
	if _, err := s.db.Exec(`UPDATE chat_links SET ended_at = $1 WHERE id = $2 AND ended_at IS NULL`, endedAt.UTC(), rowID); err != nil {
		return ChatLink{}, err
	}
	if link, err = s.GetChatLink(sessionID, linkID); err != nil {
		return ChatLink{}, err
	}
	return link, nil
}

const chatLinkMessageSelect = `SELECT id, link_id, direction, state, body, attachments, platform_message_ids, bus_event_id,
	delivery_id, bridge_message_id, attempts, error, created_at, updated_at, next_attempt_at,
	sender_platform_user_id, sender_platform_display_name, sender_person_id, sender_principal_id,
	sender_person_display_name, sender_resolved_at, sent_body FROM chat_link_messages`

func scanChatLinkMessage(scanner interface{ Scan(...any) error }) (ChatLinkMessage, error) {
	var message ChatLinkMessage
	var rowID, linkRowID int64
	var direction, state, attachments, platformMessageIDs string
	var nextAttemptAt, senderResolvedAt sql.NullTime
	if err := scanner.Scan(&rowID, &linkRowID, &direction, &state, &message.Body, &attachments, &platformMessageIDs, &message.BusEventID,
		&message.DeliveryID, &message.BridgeMessageID, &message.Attempts, &message.Error, &message.CreatedAt, &message.UpdatedAt, &nextAttemptAt,
		&message.SenderPlatformUserID, &message.SenderPlatformDisplayName, &message.SenderPersonID, &message.SenderPrincipalID,
		&message.SenderPersonDisplayName, &senderResolvedAt, &message.SentBody); err != nil {
		return ChatLinkMessage{}, err
	}
	if senderResolvedAt.Valid {
		message.SenderResolvedAt = senderResolvedAt.Time.UTC()
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
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = $1 ORDER BY id`, rowID)
}

// ChatLinkMessagesInState answers a link's messages one way in one state,
// oldest first.
func (s *Store) ChatLinkMessagesInState(linkID string, direction msg.ChatLinkDirection, state msg.ChatLinkMessageState) ([]ChatLinkMessage, error) {
	rowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return nil, ErrChatLinkNotFound
	}
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = $1 AND direction = $2 AND state = $3 ORDER BY id`,
		rowID, string(direction), string(state))
}

// ChatLinkMessagesOfDelivery answers the messages one delivery id ties
// together, oldest first.
func (s *Store) ChatLinkMessagesOfDelivery(deliveryID string) ([]ChatLinkMessage, error) {
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE delivery_id = $1 ORDER BY id`, deliveryID)
}

// ChatLinkOfMessage answers the link a message belongs to.
func (s *Store) ChatLinkOfMessage(message ChatLinkMessage) (ChatLink, error) {
	rowID, ok := rowIDWithPrefix(message.LinkID, chatLinkIDPrefix)
	if !ok {
		return ChatLink{}, ErrChatLinkNotFound
	}
	return s.getChatLinkByRowID(rowID)
}

// InboundChatLinkMessage is what AddInboundChatLinkMessage keeps.
type InboundChatLinkMessage struct {
	LinkID                    string
	BusEventID                string
	Body                      string
	Attachments               []msg.ChatLinkAttachment
	PlatformMessageID         string
	SenderPlatformUserID      string
	SenderPlatformDisplayName string
	At                        time.Time
}

// AddInboundChatLinkMessage keeps a message the person sent, pending. A bus
// event already kept for the link (JetStream redelivers) adds nothing and
// answers false.
func (s *Store) AddInboundChatLinkMessage(inbound InboundChatLinkMessage) (bool, error) {
	linkID, busEventID, body, attachments, platformMessageID, at := inbound.LinkID, inbound.BusEventID, inbound.Body, inbound.Attachments, inbound.PlatformMessageID, inbound.At
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
	result, err := s.db.Exec(`INSERT INTO chat_link_messages (link_id, direction, state, body, attachments, platform_message_ids, bus_event_id,
		sender_platform_user_id, sender_platform_display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
		rowID, string(msg.ChatLinkInbound), string(msg.ChatLinkMessagePending), body, string(encodedAttachments), string(encodedIDs), busEventID,
		inbound.SenderPlatformUserID, inbound.SenderPlatformDisplayName, at.UTC(), at.UTC())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// ChatLinkMessageSender is who people-store says sent an inbound message;
// PersonID and PrincipalID are empty when nobody owns the account.
type ChatLinkMessageSender struct {
	PersonID          string
	PrincipalID       string
	PersonDisplayName string
}

// RecordChatLinkMessageSender stores who sent an inbound message, read at at.
func (s *Store) RecordChatLinkMessageSender(messageID string, sender ChatLinkMessageSender, at time.Time) error {
	rowID, ok := ChatLinkMessageRowID(messageID)
	if !ok {
		return fmt.Errorf("not a chat link message id: %q", messageID)
	}
	_, err := s.db.Exec(`UPDATE chat_link_messages SET sender_person_id = $1, sender_principal_id = $2, sender_person_display_name = $3,
		sender_resolved_at = $4, updated_at = $5 WHERE id = $6`,
		sender.PersonID, sender.PrincipalID, sender.PersonDisplayName, at.UTC(), at.UTC(), rowID)
	return err
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
		result, err := transaction.Exec(`UPDATE chat_link_messages SET state = $1, delivery_id = $2, bridge_message_id = $3, attempts = attempts + 1, error = '', updated_at = $4
			WHERE id = $5 AND state = $6`, string(msg.ChatLinkMessageInSession), deliveryID, bridgeMessageID, at.UTC(), rowID, string(msg.ChatLinkMessagePending))
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
		if _, err := s.db.Exec(`UPDATE chat_link_messages SET attempts = attempts + 1, error = $1, updated_at = $2 WHERE id = $3`,
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
		_, err := s.db.Exec(`UPDATE chat_link_messages SET state = $1, error = $2, updated_at = $3, next_attempt_at = NULL WHERE id = $4`,
			string(state), reason, at.UTC(), rowID)
		return err
	}
	encoded, err := json.Marshal(platformMessageIDs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chat_link_messages SET state = $1, error = $2, platform_message_ids = $3, updated_at = $4, next_attempt_at = NULL WHERE id = $5`,
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
	existing, err := s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE link_id = $1 AND direction = $2 AND delivery_id = $3`,
		rowID, string(msg.ChatLinkOutbound), deliveryID)
	if err != nil {
		return ChatLinkMessage{}, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	var messageRowID int64
	if err := s.db.QueryRow(`INSERT INTO chat_link_messages (link_id, direction, state, body, delivery_id, error, created_at, updated_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		rowID, string(msg.ChatLinkOutbound), string(state), body, deliveryID, reason, at.UTC(), at.UTC(), at.UTC()).Scan(&messageRowID); err != nil {
		return ChatLinkMessage{}, err
	}
	return scanChatLinkMessage(s.db.QueryRow(chatLinkMessageSelect+` WHERE id = $1`, messageRowID))
}

// RecordChatLinkSendFailed counts a failed send of an outbound message and
// sets when to try again.
func (s *Store) RecordChatLinkSendFailed(messageID, reason string, nextAttemptAt, at time.Time) error {
	rowID, ok := ChatLinkMessageRowID(messageID)
	if !ok {
		return fmt.Errorf("not a chat link message id: %q", messageID)
	}
	_, err := s.db.Exec(`UPDATE chat_link_messages SET attempts = attempts + 1, error = $1, next_attempt_at = $2, updated_at = $3 WHERE id = $4`,
		reason, nextAttemptAt.UTC(), at.UTC(), rowID)
	return err
}

// DueOutboundChatLinkMessages answers pending replies whose next attempt is
// due, oldest first, whether or not their link has since ended: a reply the
// session wrote before the end is still owed.
func (s *Store) DueOutboundChatLinkMessages(now time.Time) ([]ChatLinkMessage, error) {
	return s.queryChatLinkMessages(chatLinkMessageSelect+` WHERE direction = $1 AND state = $2 AND (next_attempt_at IS NULL OR next_attempt_at <= $3) ORDER BY id`,
		string(msg.ChatLinkOutbound), string(msg.ChatLinkMessagePending), now.UTC())
}

// SetChatLinkMessageSentBody records what an outbound reply was sent as, when
// that is not its body: its references drawn for the person it went to.
func (s *Store) SetChatLinkMessageSentBody(messageID, sentBody string) error {
	rowID, ok := ChatLinkMessageRowID(messageID)
	if !ok {
		return fmt.Errorf("not a chat link message id: %q", messageID)
	}
	_, err := s.db.Exec(`UPDATE chat_link_messages SET sent_body = $1 WHERE id = $2`, sentBody, rowID)
	return err
}

// ChatLinkRecipientPrincipalID is the principal of the person a link talks
// with: the newest inbound message's sender whose principal is known and is
// not the one the session runs as (the operator writing in their own link).
// Empty when no message from them has been tied to a principal.
func (s *Store) ChatLinkRecipientPrincipalID(linkID, sessionPrincipalID string) (string, error) {
	linkRowID, ok := rowIDWithPrefix(linkID, chatLinkIDPrefix)
	if !ok {
		return "", fmt.Errorf("not a chat link id: %q", linkID)
	}
	var principalID string
	err := s.db.QueryRow(`SELECT sender_principal_id FROM chat_link_messages
		WHERE link_id = $1 AND direction = $2 AND sender_principal_id != '' AND sender_principal_id != $3
		ORDER BY id DESC LIMIT 1`, linkRowID, string(msg.ChatLinkInbound), sessionPrincipalID).Scan(&principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return principalID, err
}
