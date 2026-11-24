package memorystore

import "time"

// TrackMemoryUsage records when a memory was used during a session. The
// session_id is an opaque reference to the canonical session_id from
// llm-bridge-server's sessions table — memory-store does not maintain its
// own session metadata anymore (the dead `sessions` and `session_tags`
// tables were dropped in Phase II.B of llm-bridge MIGRATION-session-identity.md;
// per-session aggregates live in log-store's sessions projection).
func (s *Store) TrackMemoryUsage(memoryID, sessionID string, turnNumber int, usageType string) error {
	query := `
	INSERT INTO memory_usage (memory_id, session_id, turn_number, usage_type, accessed_at)
	VALUES (?, ?, ?, ?, ?)
	`
	_, err := s.db.Exec(query, memoryID, sessionID, turnNumber, usageType, time.Now().Unix())
	return err
}

// SessionsSchema returns the SQL schema for the memory_usage table.
// Despite the name (kept for backward compat with the bootstrap call site
// in store.go), this no longer creates the legacy sessions/session_tags
// tables — those are dropped in migrate(). memory_usage's session_id is
// no longer constrained by a FOREIGN KEY since the referenced table is gone.
func SessionsSchema() string {
	return `
	CREATE TABLE IF NOT EXISTS memory_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		turn_number INTEGER NOT NULL,
		usage_type TEXT NOT NULL,
		accessed_at INTEGER NOT NULL,
		FOREIGN KEY (memory_id) REFERENCES memories(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_memory_usage_memory_id ON memory_usage(memory_id);
	CREATE INDEX IF NOT EXISTS idx_memory_usage_session_id ON memory_usage(session_id);
	`
}
