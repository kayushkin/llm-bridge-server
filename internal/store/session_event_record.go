package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SessionEventRecord is one session event as the harness manager stores it:
// its body and the values a reader selects it by, which the session log keeps
// in columns beside the body.
type SessionEventRecord struct {
	SessionID        string
	Type             string
	MessageID        string
	HarnessMessageID string
	// ToolUseID is the tool call's id on a tool_call or tool_result event,
	// and ToolName the tool's name on a tool_call.
	ToolUseID string
	ToolName  string
	Data      []byte
	// Event is the event Data is the JSON of.
	Event *msg.Event
}

// SessionLogStatement is one statement the session log runs in the
// transaction that stores an event: a write of a row of this module that
// belongs with the event.
type SessionLogStatement struct {
	Query     string
	Arguments []any
}

// Writes of this module's rows the session log makes in the transaction of
// their event, so the two are kept together or not at all:
//
//   - a status event and its session row (SessionRowLockForStatus, then
//     SessionStatusRowWrite);
//   - a runner spawn's numbered entry and the spawn's stored-through number
//     (RunnerSpawnAdvance).
//
// The session log's writer connection acts as the session log's own role,
// which may read and write only the columns these statements name: see
// GrantSessionLogWrites. The table names carry the schema, since that
// connection resolves bare names in the session log's schema.

// SessionRowLockForStatus locks the session's row for the status the
// transaction is about to write onto it. It answers one row when the session
// exists and none when it does not; a session with no row gets no status
// event. The lock keeps the row from being deleted between the event and the
// write.
func SessionRowLockForStatus(bridgeID string) SessionLogStatement {
	return SessionLogStatement{
		Query:     `SELECT bridge_id FROM ` + ModuleSchema + `.sessions WHERE bridge_id = $1 FOR NO KEY UPDATE`,
		Arguments: []any{bridgeID},
	}
}

// SessionStatusRowWrite writes status, whose AsOf is its event's id, and its
// state onto the session row. updated_at moves to now unless touchUpdatedAt is
// false: the startup reconcile leaves it, since FileInactive and ArchiveOld
// read it as the row's last real change. A row holding a status as of a later
// event keeps it, so an older status never replaces a newer one.
func SessionStatusRowWrite(bridgeID string, status *msg.SessionStatus, touchUpdatedAt bool) (SessionLogStatement, error) {
	data, err := json.Marshal(status)
	if err != nil {
		return SessionLogStatement{}, fmt.Errorf("marshal session status: %w", err)
	}
	set := `state = $1, status = $2`
	arguments := []any{string(status.State), string(data), bridgeID, status.AsOf}
	if touchUpdatedAt {
		set += `, updated_at = $5`
		arguments = append(arguments, time.Now().UTC())
	}
	return SessionLogStatement{
		Query: `UPDATE ` + ModuleSchema + `.sessions SET ` + set + `
			WHERE bridge_id = $3 AND COALESCE((NULLIF(status, '')::jsonb ->> 'as_of')::bigint, 0) < $4`,
		Arguments: arguments,
	}, nil
}

// RunnerSpawnAdvance records that the server stored a runner spawn's output
// through sequence, as AdvanceRunnerSpawn does.
func RunnerSpawnAdvance(sessionID, spawnID string, sequence uint64) SessionLogStatement {
	return SessionLogStatement{
		Query:     `UPDATE ` + ModuleSchema + `.runner_spawns SET last_sequence = $1 WHERE session_id = $2 AND spawn_id = $3 AND last_sequence < $1`,
		Arguments: []any{int64(sequence), sessionID, spawnID},
	}
}

// GrantSessionLogWrites lets role, the session log's own, run the statements
// above and nothing more: read a session's id and status and write its state,
// status and updated_at, and read and advance a runner spawn's number. It runs
// on this module's writer, whose role owns the tables. Every grant is
// idempotent, so every start runs it, after both modules are migrated.
func (s *Store) GrantSessionLogWrites(ctx context.Context, role string) error {
	for _, statement := range []string{
		`GRANT USAGE ON SCHEMA ` + ModuleSchema + ` TO ` + role,
		`GRANT SELECT (bridge_id, status), UPDATE (state, status, updated_at) ON ` + ModuleSchema + `.sessions TO ` + role,
		`GRANT SELECT (session_id, spawn_id, last_sequence), UPDATE (last_sequence) ON ` + ModuleSchema + `.runner_spawns TO ` + role,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("let the session log write the rows of its events (%s): %w", statement, err)
		}
	}
	return nil
}

// ModuleHoldsRecords says whether the module at db holds this server's
// records: whether a server has opened it, or an import copied a bridge
// database into it. db is the module's writer pool, before New.
func ModuleHoldsRecords(ctx context.Context, db *sql.DB) (bool, error) {
	var holds bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM server_identity)`).Scan(&holds)
	return holds, err
}

// HighestArchivedEventID is the highest id the bridge's SQLite events table
// handed out, as the import recorded it, and whether a number was recorded.
// The session log numbers its own events above it.
func (s *Store) HighestArchivedEventID() (int64, bool, error) {
	var highest int64
	err := s.dbRO.QueryRow(`SELECT highest_archived_event_id FROM archived_event_numbering WHERE only_row = 1`).Scan(&highest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return highest, true, nil
}

// ResetStaleSessions sets pid 0 on every session in one of states that no
// runner may still be holding, and answers them as they were. It is the
// first half of the startup reconcile; the session log writes each one's
// status.
func (s *Store) ResetStaleSessions(states ...msg.SessionState) ([]Session, error) {
	if len(states) == 0 {
		return nil, nil
	}
	stateNames := make([]string, len(states))
	for i, st := range states {
		stateNames[i] = string(st)
	}
	// A session whose process a runner may still be holding is left as it is:
	// the runner says, when it reconnects, whether the process is alive. So
	// is a harness subagent of such a session, which lives in that process.
	// The watchdog deals with either if no runner ever claims it.
	stale := ` state = ANY($1) AND session_id NOT IN (SELECT session_id FROM runner_spawns WHERE NOT ended)
		AND NOT (controlled_by = $2
			AND manager_session_id IN (SELECT session_id FROM runner_spawns WHERE NOT ended))`
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+sessionColumns+` FROM sessions WHERE`+stale+` FOR UPDATE`, stateNames, string(msg.ControlledByHarness))
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
		return nil, tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE sessions SET pid=0 WHERE`+stale, stateNames, string(msg.ControlledByHarness)); err != nil {
		return nil, err
	}
	return sessions, tx.Commit()
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
