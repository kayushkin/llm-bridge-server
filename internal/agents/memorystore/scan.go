package memorystore

import (
	"database/sql"
	"encoding/json"
	"time"
)

// scanMemory scans a memory row from a SQL result
func (s *Store) scanMemory(scanner interface {
	Scan(dest ...interface{}) error
}) (Memory, error) {
	var m Memory
	var summary, originalID sql.NullString
	var embJSON []byte
	var lastAccessed, createdAt int64
	var expiresAt sql.NullInt64

	err := scanner.Scan(
		&m.ID, &m.Content, &summary, &originalID,
		&m.Importance, &m.AccessCount, &lastAccessed, &createdAt, &m.Source, &embJSON,
		&m.AlwaysLoad, &expiresAt, &m.Tokens,
	)
	if err != nil {
		return Memory{}, err
	}

	m.Summary = summary.String
	m.OriginalID = originalID.String
	m.LastAccessed = time.Unix(lastAccessed, 0)
	m.CreatedAt = time.Unix(createdAt, 0)
	if expiresAt.Valid {
		exp := time.Unix(expiresAt.Int64, 0)
		m.ExpiresAt = &exp
	}

	if len(embJSON) > 0 {
		json.Unmarshal(embJSON, &m.Embedding)
	}

	return m, nil
}

// loadTags is gone deliberately. It read one memory's tags per call and
// discarded a row-scan failure with `continue`, and its last caller was the
// swallow in ListByOrchestrator. Every tag lookup now goes through
// loadTagsForMemories (builder.go), which is batched, chunked under SQLite's
// bound-parameter ceiling, and returns its errors. A second tag-loading path is
// how one site came to be repaired and the other left broken.