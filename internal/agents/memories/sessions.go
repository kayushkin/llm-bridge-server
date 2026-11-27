package memories

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
	VALUES ($1, $2, $3, $4, $5)
	`
	_, err := s.db.Exec(query, memoryID, sessionID, turnNumber, usageType, time.Now().Unix())
	return err
}
