package memorystore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// likePrefixPattern turns a literal id into a LIKE pattern that matches the ids
// starting with it, and nothing else.
//
// LIKE reads `%` and `_` as wildcards, and memory ids are not opaque UUIDs here:
// inber writes "conversation-summary:<session-id>:<suffix>", and a session id
// carrying an underscore would otherwise make that position match any character
// — the caller asks for one memory by id and Get answers with a different one.
// The backslash escape is declared by the ESCAPE clause at every call site.
func likePrefixPattern(id string) string {
	var b strings.Builder
	b.Grow(len(id) + 1)
	for _, r := range id {
		if r == '%' || r == '_' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

// Save stores a new memory.
func (s *Store) Save(m Memory) error {
	// Generate embedding if not provided
	if len(m.Embedding) == 0 {
		m.Embedding = s.embedder.Embed(m.Content)
	}

	// Set defaults
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.LastAccessed.IsZero() {
		m.LastAccessed = m.CreatedAt
	}
	if m.Importance == 0 {
		m.Importance = 0.5
	}

	// Serialize embedding
	embJSON, err := json.Marshal(m.Embedding)
	if err != nil {
		return fmt.Errorf("marshal embedding: %w", err)
	}

	// Start transaction
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Auto-compute tokens if not set
	if m.Tokens == 0 && m.Content != "" {
		m.Tokens = (len(m.Content) + 2) / 3 // ~3 chars per token
	}
	
	// Set default ref_type if empty
	if m.RefType == "" {
		m.RefType = "memory"
	}

	// Upsert memory (insert or update on conflict)
	query := `
	INSERT INTO memories (id, content, summary, original_id, importance, access_count, last_accessed, created_at, source, embedding, always_load, expires_at, tokens, ref_type, ref_target, is_lazy, orchestrator)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		content = excluded.content,
		summary = excluded.summary,
		importance = excluded.importance,
		last_accessed = excluded.last_accessed,
		source = excluded.source,
		embedding = excluded.embedding,
		always_load = excluded.always_load,
		expires_at = excluded.expires_at,
		tokens = excluded.tokens,
		ref_type = excluded.ref_type,
		ref_target = excluded.ref_target,
		is_lazy = excluded.is_lazy,
		orchestrator = excluded.orchestrator
	`
	_, err = tx.Exec(query,
		m.ID, m.Content, nullString(m.Summary), nullString(m.OriginalID),
		m.Importance, m.AccessCount,
		m.LastAccessed.Unix(), m.CreatedAt.Unix(), m.Source, embJSON,
		m.AlwaysLoad, nullInt64Ptr(m.ExpiresAt), m.Tokens,
		m.RefType, nullString(m.RefTarget), m.IsLazy, m.Orchestrator,
	)
	if err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}

	// Keep the full-text index in step with the row, inside the same
	// transaction. On the same connection and outside it, a rolled-back Save
	// would leave the index describing a memory the store does not have, and
	// Search would rank a candidate whose row the join then drops.
	if err := indexMemoryText(tx, m.ID, m.Content, m.Summary); err != nil {
		return err
	}

	// Replace tags (delete old, insert new)
	if _, err := tx.Exec("DELETE FROM memory_tags WHERE memory_id = ?", m.ID); err != nil {
		return fmt.Errorf("delete old tags: %w", err)
	}
	if len(m.Tags) > 0 {
		tagStmt, err := tx.Prepare("INSERT INTO memory_tags (memory_id, tag) VALUES (?, ?)")
		if err != nil {
			return fmt.Errorf("prepare tag insert: %w", err)
		}
		defer tagStmt.Close()

		for _, tag := range m.Tags {
			if _, err := tagStmt.Exec(m.ID, tag); err != nil {
				return fmt.Errorf("insert tag: %w", err)
			}
		}
	}

	return tx.Commit()
}

// Get retrieves a memory by ID and updates access tracking.
//
// The id may be a prefix of the stored id: writers that leave a recall pointer
// in a conversation abbreviate it (conversation.StashLargeContent names the
// first eight characters of a UUID), so the read that pointer names has to
// resolve the same row from less than the whole id.
//
// Everything after the row lookup therefore keys on the id the row actually
// carries, never on the prefix the caller passed. Keyed on the prefix, the tag
// query and the access update both match zero rows: Get returned a memory with
// no tags and reported an access-count bump that was never written, which is a
// return value lying about a write.
func (s *Store) Get(id string) (*Memory, error) {
	// An exact id beats a prefix of it. Without the ordering, a store holding
	// both "abc123" and "abc123-derived" answers Get("abc123") with whichever
	// row SQLite reaches first, so the caller silently reads the wrong memory.
	query := `
	SELECT id, content, summary, original_id, importance, access_count, last_accessed, created_at, source, embedding, always_load, expires_at, tokens, ref_type, ref_target, is_lazy, orchestrator
	FROM memories
	WHERE id = ? OR id LIKE ? ESCAPE '\'
	ORDER BY (id = ?) DESC
	`
	row := s.db.QueryRow(query, id, likePrefixPattern(id), id)

	var m Memory
	var summary, originalID, refTarget sql.NullString
	var embJSON []byte
	var lastAccessed, createdAt int64
	var expiresAt sql.NullInt64

	err := row.Scan(
		&m.ID, &m.Content, &summary, &originalID,
		&m.Importance, &m.AccessCount, &lastAccessed, &createdAt, &m.Source, &embJSON,
		&m.AlwaysLoad, &expiresAt, &m.Tokens,
		&m.RefType, &refTarget, &m.IsLazy, &m.Orchestrator,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("scan memory: %w", err)
	}

	m.Summary = summary.String
	m.OriginalID = originalID.String
	m.RefTarget = refTarget.String
	m.LastAccessed = time.Unix(lastAccessed, 0)
	m.CreatedAt = time.Unix(createdAt, 0)
	if expiresAt.Valid {
		exp := time.Unix(expiresAt.Int64, 0)
		m.ExpiresAt = &exp
	}
	
	// If this is a lazy-loaded reference, load content on-demand
	if m.IsLazy {
		if err := s.loadLazyContent(&m); err != nil {
			return nil, fmt.Errorf("load lazy content: %w", err)
		}
	}

	if err := json.Unmarshal(embJSON, &m.Embedding); err != nil {
		return nil, fmt.Errorf("unmarshal embedding: %w", err)
	}

	// Fetch tags. Keyed on the resolved row id, not the caller's, which may be
	// a prefix that matches no memory_tags row.
	tagRows, err := s.db.Query("SELECT tag FROM memory_tags WHERE memory_id = ?", m.ID)
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}
	defer tagRows.Close()

	m.Tags = []string{}
	for tagRows.Next() {
		var tag string
		if err := tagRows.Scan(&tag); err != nil {
			continue
		}
		m.Tags = append(m.Tags, tag)
	}

	// Update access tracking synchronously, on the resolved row id for the same
	// reason as the tags above.
	s.updateAccess(m.ID)

	// Update the returned struct to reflect the access tracking changes
	m.AccessCount++
	m.LastAccessed = time.Now()
	m.Importance = math.Min(1.0, m.Importance*1.01)

	return &m, nil
}