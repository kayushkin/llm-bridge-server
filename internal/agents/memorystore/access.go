package memorystore

import (
	"time"
)

// updateAccess increments access count and updates last accessed time.
func (s *Store) updateAccess(id string) {
	query := `
	UPDATE memories
	SET access_count = access_count + 1,
	    last_accessed = ?,
	    importance = MIN(1.0, importance * 1.01)
	WHERE id = ?
	`
	s.db.Exec(query, time.Now().Unix(), id)
}