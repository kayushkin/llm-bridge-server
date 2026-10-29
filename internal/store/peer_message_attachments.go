package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Files messages between connected servers carry
// (internal/server/peer_message_attachments.go). One row per attachment, as
// this server holds it:
//
//   - outgoing: uploaded by a sender (uploaded_by) before its message exists,
//     with message_id empty, then claimed by the message it is sent with. Its
//     bytes stay here for the other server to fetch. State stored.
//   - incoming: recorded when a message that describes it arrives, with the
//     sender's id in remote_attachment_id. State pending until the bytes are
//     fetched and match, then fetched; failed once the fetch gave up.
//
// The bytes live in a file named by the row's id (the server decides where);
// this table holds only what is known about them.

const peerAttachmentIDPrefix = "peer_attachment_"

// PeerMessageAttachment is one row of peer_message_attachments.
type PeerMessageAttachment = msg.PeerMessageAttachment

// ErrPeerAttachmentNotFound is an attachment id this server has no row for.
var ErrPeerAttachmentNotFound = errors.New("peer message attachment not found")

func (s *Store) migratePeerMessageAttachments() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS peer_message_attachments (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id           TEXT NOT NULL DEFAULT '',
			direction            TEXT NOT NULL,
			name                 TEXT NOT NULL,
			media_type           TEXT NOT NULL,
			size                 INTEGER NOT NULL,
			sha256               TEXT NOT NULL,
			state                TEXT NOT NULL,
			remote_attachment_id TEXT NOT NULL DEFAULT '',
			uploaded_by          TEXT NOT NULL DEFAULT '',
			attempts             INTEGER NOT NULL DEFAULT 0,
			last_error           TEXT NOT NULL DEFAULT '',
			next_attempt_at      DATETIME,
			fetched_at           DATETIME,
			created_at           DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_peer_message_attachments_message ON peer_message_attachments(message_id, id);
		CREATE INDEX IF NOT EXISTS idx_peer_message_attachments_state ON peer_message_attachments(direction, state);
	`)
	return err
}

const peerAttachmentSelect = `SELECT id, message_id, name, media_type, size, sha256, state, remote_attachment_id, attempts, last_error,
	fetched_at, created_at FROM peer_message_attachments`

func scanPeerAttachment(row rowScanner) (PeerMessageAttachment, error) {
	var attachment PeerMessageAttachment
	var rowID int64
	var state string
	var fetchedAt sql.NullTime
	if err := row.Scan(&rowID, &attachment.MessageID, &attachment.Name, &attachment.MediaType, &attachment.Size, &attachment.SHA256, &state,
		&attachment.RemoteAttachmentID, &attachment.Attempts, &attachment.LastError, &fetchedAt, &attachment.CreatedAt); err != nil {
		return PeerMessageAttachment{}, err
	}
	attachment.ID = prefixedIDOf(peerAttachmentIDPrefix, rowID)
	attachment.State = msg.PeerMessageAttachmentState(state)
	attachment.FetchedAt = timeOrNil(fetchedAt)
	return attachment, nil
}

// InsertStagedPeerAttachment records a file a sender uploaded before the
// message that will carry it exists.
func (s *Store) InsertStagedPeerAttachment(attachment *PeerMessageAttachment, uploadedBy string) error {
	if attachment.Name == "" || attachment.SHA256 == "" || uploadedBy == "" {
		return fmt.Errorf("insert staged attachment: name, sha256 and uploader are required")
	}
	attachment.CreatedAt = time.Now().UTC()
	attachment.State = msg.PeerMessageAttachmentStored
	result, err := s.db.Exec(`INSERT INTO peer_message_attachments (direction, name, media_type, size, sha256, state, uploaded_by, created_at)
		VALUES ('outgoing', ?, ?, ?, ?, ?, ?, ?)`, attachment.Name, attachment.MediaType, attachment.Size, attachment.SHA256,
		string(attachment.State), uploadedBy, attachment.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert staged attachment %s: %w", attachment.Name, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	attachment.ID = prefixedIDOf(peerAttachmentIDPrefix, rowID)
	return nil
}

// ClaimStagedPeerAttachments ties staged uploads to the message sending
// them, all or none: each must exist, be outgoing, be unclaimed and have
// been uploaded by uploadedBy.
func (s *Store) ClaimStagedPeerAttachments(ids []string, messageID, uploadedBy string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		rowID, ok := rowIDOfPrefixed(peerAttachmentIDPrefix, id)
		if !ok {
			return fmt.Errorf("%q is not an attachment id", id)
		}
		result, err := tx.Exec(`UPDATE peer_message_attachments SET message_id = ? WHERE id = ? AND direction = 'outgoing' AND message_id = '' AND uploaded_by = ?`,
			messageID, rowID, uploadedBy)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return fmt.Errorf("%s is not an upload of %s waiting for a message", id, uploadedBy)
		}
	}
	return tx.Commit()
}

// StagedPeerAttachment returns an unclaimed upload of uploadedBy, to check a
// message's attachments before the message is recorded.
func (s *Store) StagedPeerAttachment(id, uploadedBy string) (PeerMessageAttachment, error) {
	rowID, ok := rowIDOfPrefixed(peerAttachmentIDPrefix, id)
	if !ok {
		return PeerMessageAttachment{}, ErrPeerAttachmentNotFound
	}
	found, err := scanPeerAttachment(s.db.QueryRow(peerAttachmentSelect+` WHERE id = ? AND direction = 'outgoing' AND message_id = '' AND uploaded_by = ?`,
		rowID, uploadedBy))
	return notFoundAs(found, err, ErrPeerAttachmentNotFound)
}

// InsertIncomingPeerAttachment records a file an arriving message describes,
// pending until its bytes are fetched.
func (s *Store) InsertIncomingPeerAttachment(attachment *PeerMessageAttachment) error {
	if attachment.MessageID == "" || attachment.RemoteAttachmentID == "" {
		return fmt.Errorf("insert incoming attachment: message and the sender's id are required")
	}
	attachment.CreatedAt = time.Now().UTC()
	attachment.State = msg.PeerMessageAttachmentPending
	result, err := s.db.Exec(`INSERT INTO peer_message_attachments (message_id, direction, name, media_type, size, sha256, state, remote_attachment_id, created_at)
		VALUES (?, 'incoming', ?, ?, ?, ?, ?, ?, ?)`, attachment.MessageID, attachment.Name, attachment.MediaType, attachment.Size,
		attachment.SHA256, string(attachment.State), attachment.RemoteAttachmentID, attachment.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert incoming attachment %s: %w", attachment.Name, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	attachment.ID = prefixedIDOf(peerAttachmentIDPrefix, rowID)
	return nil
}

// GetPeerAttachment returns one attachment by its id on this server.
func (s *Store) GetPeerAttachment(id string) (PeerMessageAttachment, error) {
	rowID, ok := rowIDOfPrefixed(peerAttachmentIDPrefix, id)
	if !ok {
		return PeerMessageAttachment{}, ErrPeerAttachmentNotFound
	}
	found, err := scanPeerAttachment(s.db.QueryRow(peerAttachmentSelect+` WHERE id = ?`, rowID))
	return notFoundAs(found, err, ErrPeerAttachmentNotFound)
}

// PeerAttachmentsOfMessage returns a message's attachments in the order they
// were added.
func (s *Store) PeerAttachmentsOfMessage(messageID string) ([]PeerMessageAttachment, error) {
	return s.queryPeerAttachments(peerAttachmentSelect+` WHERE message_id = ? ORDER BY id`, messageID)
}

// PeerAttachmentsToFetch returns incoming attachments still pending whose
// next try is due by now.
func (s *Store) PeerAttachmentsToFetch(now time.Time) ([]PeerMessageAttachment, error) {
	return s.queryPeerAttachments(peerAttachmentSelect+` WHERE direction = 'incoming' AND state = 'pending'
		AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY id`, now.UTC())
}

// StalePeerAttachmentUploads returns uploads no message claimed since before.
func (s *Store) StalePeerAttachmentUploads(before time.Time) ([]PeerMessageAttachment, error) {
	return s.queryPeerAttachments(peerAttachmentSelect+` WHERE direction = 'outgoing' AND message_id = '' AND created_at < ? ORDER BY id`, before.UTC())
}

func (s *Store) queryPeerAttachments(query string, arguments ...any) ([]PeerMessageAttachment, error) {
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []PeerMessageAttachment{}
	for rows.Next() {
		attachment, err := scanPeerAttachment(rows)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

// RecordPeerAttachmentFetched marks an incoming attachment's bytes as here
// and verified.
func (s *Store) RecordPeerAttachmentFetched(id string, at time.Time) error {
	return s.updatePeerAttachment(id, `state = 'fetched', fetched_at = ?, last_error = '', next_attempt_at = NULL`, at.UTC())
}

// RecordPeerAttachmentFetchFailed counts a failed fetch: tried again at next,
// or failed for good when giveUp.
func (s *Store) RecordPeerAttachmentFetchFailed(id, reason string, next time.Time, giveUp bool) error {
	if giveUp {
		return s.updatePeerAttachment(id, `state = 'failed', attempts = attempts + 1, last_error = ?, next_attempt_at = NULL`, reason)
	}
	return s.updatePeerAttachment(id, `attempts = attempts + 1, last_error = ?, next_attempt_at = ?`, reason, next.UTC())
}

// RetryPeerAttachment puts a failed incoming attachment back to pending, to
// be fetched again now.
func (s *Store) RetryPeerAttachment(id string) error {
	return s.updatePeerAttachment(id, `state = 'pending', next_attempt_at = NULL`)
}

// DeletePeerAttachment removes an attachment's row; the caller removes its
// file.
func (s *Store) DeletePeerAttachment(id string) error {
	rowID, ok := rowIDOfPrefixed(peerAttachmentIDPrefix, id)
	if !ok {
		return ErrPeerAttachmentNotFound
	}
	_, err := s.db.Exec(`DELETE FROM peer_message_attachments WHERE id = ?`, rowID)
	return err
}

func (s *Store) updatePeerAttachment(id, assignments string, arguments ...any) error {
	rowID, ok := rowIDOfPrefixed(peerAttachmentIDPrefix, id)
	if !ok {
		return ErrPeerAttachmentNotFound
	}
	result, err := s.db.Exec(`UPDATE peer_message_attachments SET `+assignments+` WHERE id = ?`, append(arguments, rowID)...)
	if err != nil {
		return fmt.Errorf("update attachment %s: %w", id, err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return ErrPeerAttachmentNotFound
	}
	return nil
}
