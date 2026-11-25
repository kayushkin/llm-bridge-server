package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SessionEventRecord is one session event as the harness manager stores it:
// its body and the values a reader selects it by. The bridge database keeps
// the message ids in columns of its own; the session log keeps those and the
// tool use id.
type SessionEventRecord struct {
	SessionID        string
	Type             string
	MessageID        string
	HarnessMessageID string
	// ToolUseID is the tool call's id on a tool_call or tool_result event,
	// which the bridge database reads from the body instead.
	ToolUseID string
	Data      []byte
	// Event is the event Data is the JSON of.
	Event *msg.Event
}

// StoreSessionEvent stores a record in the events table, as
// StoreEventReturningID does.
func (s *Store) StoreSessionEvent(record SessionEventRecord) (int64, error) {
	return s.StoreEventReturningID(record.SessionID, record.Type, record.MessageID, record.HarnessMessageID, record.Data)
}

// StoreSessionEventAdvancingRunnerSpawn stores a record that came as a
// runner spawn's numbered entry, as StoreEventAdvancingRunnerSpawn does.
func (s *Store) StoreSessionEventAdvancingRunnerSpawn(record SessionEventRecord, spawnSessionID, spawnID string, sequence uint64) (int64, error) {
	return s.StoreEventAdvancingRunnerSpawn(record.SessionID, record.Type, record.MessageID, record.HarnessMessageID, record.Data, spawnSessionID, spawnID, sequence)
}

// SessionExists reports whether a session row exists.
func (s *Store) SessionExists(bridgeID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sessions WHERE bridge_id = ?)`, bridgeID).Scan(&exists)
	return exists, err
}

// WriteSessionStatusRow writes a status onto its session row, unless the row
// already holds a status as of a later event. It is the row half of a
// status whose event the session log stored; written second, it can be
// written again, and an older status never replaces a newer one.
//
// It answers whether the row changed, and sql.ErrNoRows when no such session
// exists.
func (s *Store) WriteSessionStatusRow(bridgeID string, status *msg.SessionStatus, touchUpdatedAt bool) (bool, error) {
	data, err := json.Marshal(status)
	if err != nil {
		return false, fmt.Errorf("marshal session status: %w", err)
	}
	set := `state = ?, status = ?`
	args := []any{string(status.State), string(data)}
	if touchUpdatedAt {
		set += `, updated_at = ?`
		args = append(args, time.Now().UTC())
	}
	args = append(args, bridgeID, status.AsOf)
	result, err := s.db.Exec(`UPDATE sessions SET `+set+`
		WHERE bridge_id = ? AND COALESCE(json_extract(NULLIF(status, ''), '$.as_of'), 0) < ?`, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		exists, err := s.SessionExists(bridgeID)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, sql.ErrNoRows
		}
		return false, nil
	}
	s.notifyChanged(bridgeID)
	return true, nil
}

// HighestEventID is the highest id the events table ever handed out: its
// AUTOINCREMENT counter, or the highest id present if that is higher.
func (s *Store) HighestEventID() (int64, error) {
	var highestPresent int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&highestPresent); err != nil {
		return 0, err
	}
	var counter sql.NullInt64
	err := s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&counter)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return max(highestPresent, counter.Int64), nil
}

// RaiseEventNumberingAbove makes the next id the events table hands out
// larger than highest. The server calls it when it runs the old path after
// the session log has been the only copy, so the statuses and cursors it
// hands out stay above every id a client already holds. It changes the
// table's AUTOINCREMENT counter only, never a row.
func (s *Store) RaiseEventNumberingAbove(highest int64) error {
	current, err := s.HighestEventID()
	if err != nil {
		return err
	}
	if current >= highest {
		return nil
	}
	result, err := s.db.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'events'`, highest)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		_, err = s.db.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('events', ?)`, highest)
	}
	return err
}

// ResetStaleSessions sets pid 0 on every session in one of states that no
// runner may still be holding, and answers them as they were. It is the
// first half of ReconcileSessions, for a caller that writes each session's
// status itself.
func (s *Store) ResetStaleSessions(states ...msg.SessionState) ([]Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	in := strings.Join(placeholders, ",")
	// A session whose process a runner may still be holding is left as it is:
	// the runner says, when it reconnects, whether the process is alive. So
	// is a harness subagent of such a session, which lives in that process.
	// The watchdog deals with either if no runner ever claims it.
	notHeld := ` AND session_id NOT IN (SELECT session_id FROM runner_spawns WHERE ended = 0)
		AND NOT (controlled_by = '` + string(msg.ControlledByHarness) + `'
			AND COALESCE(manager_session_id, '') IN (SELECT session_id FROM runner_spawns WHERE ended = 0))`
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions WHERE state IN (`+in+`)`+notHeld, args...)
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, *sess)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	if _, err := s.db.Exec(`UPDATE sessions SET pid=0 WHERE state IN (`+in+`)`+notHeld, args...); err != nil {
		return nil, err
	}
	return sessions, nil
}

// StartupReconcileStatus is the status the startup reconcile records for a
// session a previous process left active: idle, or for a harness subagent,
// which ran inside its parent's process and will never report an end, the
// error it is.
func StartupReconcileStatus(sess *Session) *msg.SessionStatus {
	if sess.ControlledBy == msg.ControlledByHarness {
		return &msg.SessionStatus{State: msg.SessionError, Reason: msg.ReasonParentProcessExited, ChangedAt: time.Now()}
	}
	return &msg.SessionStatus{State: msg.SessionIdle, Reason: msg.ReasonStartupReconcile, ChangedAt: time.Now()}
}
