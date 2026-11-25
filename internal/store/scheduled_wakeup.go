package store

import "time"

// ClaudeCodeScheduleWakeupToolName is Claude Code's tool for waking its own
// session later. The timer lives inside the claude process, so the process
// must be alive when it comes due.
const ClaudeCodeScheduleWakeupToolName = "ScheduleWakeup"

// ScheduledWakeup is a ScheduleWakeup call whose result named a time.
type ScheduledWakeup struct {
	SessionID string
	ToolID    string
	// Prompt is the text Claude Code sends as a user message when the
	// wakeup fires (measured: the fired turn's user_message is the prompt,
	// unchanged).
	Prompt string
	DueAt  time.Time
}

// ListSessionIDsUpdatedSince returns the ids of sessions whose row changed at
// or after since, newest first.
func (s *Store) ListSessionIDsUpdatedSince(since time.Time) ([]string, error) {
	rows, err := s.dbRO.Query(`SELECT bridge_id FROM sessions WHERE updated_at >= $1 ORDER BY updated_at DESC`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs, rows.Err()
}
