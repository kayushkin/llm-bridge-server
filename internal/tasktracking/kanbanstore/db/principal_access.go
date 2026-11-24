package db

import (
	"database/sql"
	"errors"
)

// CardIDOfLink returns the card a link hangs off, so a request that names only
// the link can be checked against the boards that card sits on.
func (s *Store) CardIDOfLink(linkID string) (string, error) {
	return s.singleCardID(`SELECT card_id FROM card_links WHERE id = ?`, linkID)
}

// CardIDOfNote returns the card a note was written on, for the same reason.
func (s *Store) CardIDOfNote(noteID string) (string, error) {
	return s.singleCardID(`SELECT card_id FROM card_notes WHERE id = ?`, noteID)
}

func (s *Store) singleCardID(query, id string) (string, error) {
	var cardID string
	err := s.db.QueryRow(query, id).Scan(&cardID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return cardID, err
}
