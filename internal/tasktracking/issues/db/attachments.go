package db

import (
	"database/sql"
	"errors"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
)

func (s *Store) CreateCardAttachment(attachment *model.CardAttachment) error {
	if attachment.CreatedAt.IsZero() {
		attachment.CreatedAt = now()
	}
	attachment.CreatedAt = storedTime(attachment.CreatedAt)
	_, err := s.db.Exec(`INSERT INTO card_attachments (file_id, card_id, visibility, attached_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		attachment.FileID, attachment.CardID, string(attachment.Visibility), attachment.AttachedBy, attachment.CreatedAt)
	return err
}

// ListCardAttachments lists a card's attachments, oldest first. An empty
// visibility keeps them all.
func (s *Store) ListCardAttachments(cardID string, visibility model.NoteVisibility) ([]model.CardAttachment, error) {
	query := `SELECT file_id, card_id, visibility, attached_by, created_at FROM card_attachments WHERE card_id = ?`
	args := []any{cardID}
	if visibility != "" {
		query += ` AND visibility = ?`
		args = append(args, string(visibility))
	}
	rows, err := s.db.Query(query+` ORDER BY created_at ASC, file_id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []model.CardAttachment{}
	for rows.Next() {
		var attachment model.CardAttachment
		if err := rows.Scan(&attachment.FileID, &attachment.CardID, &attachment.Visibility, &attachment.AttachedBy, &attachment.CreatedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

// GetCardAttachment reads the row for one file on one card. A file that hangs
// on another card is ErrNotFound here: the card in the path is what the caller
// was authorized against, so it is the only card whose files it may reach.
func (s *Store) GetCardAttachment(cardID, fileID string) (*model.CardAttachment, error) {
	var attachment model.CardAttachment
	err := s.db.QueryRow(`SELECT file_id, card_id, visibility, attached_by, created_at FROM card_attachments WHERE card_id = ? AND file_id = ?`, cardID, fileID).
		Scan(&attachment.FileID, &attachment.CardID, &attachment.Visibility, &attachment.AttachedBy, &attachment.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &attachment, nil
}

func (s *Store) DeleteCardAttachment(cardID, fileID string) error {
	result, err := s.db.Exec(`DELETE FROM card_attachments WHERE card_id = ? AND file_id = ?`, cardID, fileID)
	if err != nil {
		return err
	}
	if removed, _ := result.RowsAffected(); removed == 0 {
		return ErrNotFound
	}
	return nil
}
