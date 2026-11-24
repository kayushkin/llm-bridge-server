package db

import (
	"database/sql"
	"errors"

	"github.com/kayushkin/kanban-store/internal/model"
)

func migrateCardAttachments(db *sql.DB) error {
	// file_id is the key: file-store mints one id per upload, and one upload
	// hangs on one card. card_id is not a foreign key — cards are noteboard's.
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS card_attachments (
			file_id     TEXT PRIMARY KEY,
			card_id     TEXT NOT NULL,
			visibility  TEXT NOT NULL DEFAULT 'internal',
			attached_by TEXT NOT NULL DEFAULT '',
			created_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_card_attachments_card ON card_attachments(card_id, created_at);
	`)
	return err
}

func (s *Store) CreateCardAttachment(attachment *model.CardAttachment) error {
	if attachment.CreatedAt.IsZero() {
		attachment.CreatedAt = now()
	}
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
