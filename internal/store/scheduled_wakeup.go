package store

import (
	"database/sql"
	"errors"
	"time"
)

// claudeCodeScheduleWakeupToolName is Claude Code's tool for waking its own
// session later. The timer lives inside the claude process, so the process
// must be alive when it comes due.
const claudeCodeScheduleWakeupToolName = "ScheduleWakeup"

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

// LatestScheduledWakeup returns the session's most recent ScheduleWakeup call
// logged since callsLoggedSince, or nil when there is none, the call has no
// result yet, or the call was a stop. Each call replaces the wakeup before it
// (measured on Claude Code 2.1.283: two schedules then a stop reported
// "cancelled 1 pending wakeup"), so the most recent call is the whole truth.
// Its result carries `tool_use_result.scheduledFor` in epoch milliseconds,
// the time Claude Code itself fires at; a stop reports 0. A due time already
// past is returned as it is: the caller decides what a past wakeup means.
//
// events.created_at has whole seconds, so callsLoggedSince is compared at
// the second, which can admit a call from the second before.
func (s *Store) LatestScheduledWakeup(bridgeID string, callsLoggedSince time.Time) (*ScheduledWakeup, error) {
	var toolID, prompt sql.NullString
	var scheduledForMilliseconds sql.NullInt64
	err := s.db.QueryRow(`
		SELECT json_extract(call.data, '$.tool_call.tool_id'),
		       json_extract(call.data, '$.tool_call.input.prompt'),
		       json_extract(result.data, '$.raw.tool_use_result.scheduledFor')
		FROM (
			SELECT id, data
			FROM events
			WHERE session_id = ?
			  AND type = 'tool_call'
			  AND created_at >= ?
			  AND json_extract(data, '$.tool_call.name') = ?
			ORDER BY id DESC
			LIMIT 1) call
		JOIN events result
		  ON result.session_id = ?
		 AND result.type = 'tool_result'
		 AND result.id > call.id
		 AND json_extract(result.data, '$.raw.tool_use_result') IS NOT NULL
		 AND json_extract(result.data, '$.tool_result.tool_id') = json_extract(call.data, '$.tool_call.tool_id')
		ORDER BY result.id DESC
		LIMIT 1`,
		bridgeID, callsLoggedSince.UTC().Format("2006-01-02 15:04:05"), claudeCodeScheduleWakeupToolName, bridgeID,
	).Scan(&toolID, &prompt, &scheduledForMilliseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !scheduledForMilliseconds.Valid || scheduledForMilliseconds.Int64 <= 0 {
		return nil, nil
	}
	return &ScheduledWakeup{
		SessionID: bridgeID,
		ToolID:    toolID.String,
		Prompt:    prompt.String,
		DueAt:     time.UnixMilli(scheduledForMilliseconds.Int64).UTC(),
	}, nil
}

// UserMessageLoggedSince reports whether the session has a user_message
// logged at or after the second of since. A wakeup that fires logs its prompt
// as a user_message at the due time, so this is how a caller tells a wakeup
// that fired from one that was lost.
func (s *Store) UserMessageLoggedSince(bridgeID string, since time.Time) (bool, error) {
	var found bool
	err := s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM events
			WHERE session_id = ? AND type = 'user_message' AND created_at >= ?)`,
		bridgeID, since.UTC().Format("2006-01-02 15:04:05"),
	).Scan(&found)
	return found, err
}

// ListSessionIDsUpdatedSince returns the ids of sessions whose row changed at
// or after since, newest first.
func (s *Store) ListSessionIDsUpdatedSince(since time.Time) ([]string, error) {
	rows, err := s.dbRO.Query(`SELECT bridge_id FROM sessions WHERE updated_at >= ? ORDER BY updated_at DESC`, since.UTC())
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
