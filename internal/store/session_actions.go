package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SessionAction is the canonical type from llm-bridge/msg/session_action.go.
type SessionAction = msg.SessionAction

// ErrSessionActionNotFound is an action id this session does not hold.
var ErrSessionActionNotFound = errors.New("session action not found")

// ErrSessionActionNotOffered is a confirm of an action that has already run
// or is running. An action runs once.
var ErrSessionActionNotOffered = errors.New("session action is not waiting to be confirmed")

const sessionActionIDPrefix = "session_action_"

// migrateSessionActions creates the table of actions agents offered in their
// sessions and how each run went. Called from migrate().
//
// The row is the log of a button press: who confirmed it, when, what it ran,
// and what came back. It is never deleted.
func (s *Store) migrateSessionActions() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS session_actions (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id          TEXT NOT NULL,
			label               TEXT NOT NULL,
			type                TEXT NOT NULL,
			repo_id             INTEGER NOT NULL DEFAULT 0,
			scheduler_job_id    INTEGER NOT NULL DEFAULT 0,
			message             TEXT NOT NULL DEFAULT '',
			command             TEXT NOT NULL,
			state               TEXT NOT NULL,
			offered_at          DATETIME NOT NULL,
			run_by_principal_id TEXT NOT NULL DEFAULT '',
			started_at          DATETIME,
			finished_at         DATETIME,
			output              TEXT NOT NULL DEFAULT '',
			error               TEXT NOT NULL DEFAULT '',
			result_session_id   TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_session_actions_session ON session_actions(session_id, id);
	`)
	if err != nil {
		return err
	}
	// Columns added on 2026-09-29 for run_command, model_call and
	// background_agent. Added only when missing, and any other failure is
	// returned, not ignored.
	existing := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(session_actions)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&position, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	for _, column := range []struct{ name, definition string }{
		{"shell_command", "TEXT NOT NULL DEFAULT ''"},
		{"working_directory", "TEXT NOT NULL DEFAULT ''"},
		{"result_format", "TEXT NOT NULL DEFAULT ''"},
		{"model", "TEXT NOT NULL DEFAULT ''"},
		{"maximum_cost_usd", "REAL NOT NULL DEFAULT 0"},
		{"review", "TEXT NOT NULL DEFAULT ''"},
		{"cost_usd", "REAL NOT NULL DEFAULT 0"},
	} {
		if existing[column.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE session_actions ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add session_actions.%s: %w", column.name, err)
		}
	}
	return nil
}

// InsertSessionAction records an offered action and sets its ActionID.
func (s *Store) InsertSessionAction(action *SessionAction) error {
	review, err := encodeSessionActionReview(action.Review)
	if err != nil {
		return err
	}
	offer := action.Offer
	result, err := s.db.Exec(
		`INSERT INTO session_actions (session_id, label, type, repo_id, scheduler_job_id, message, shell_command, working_directory,
		 result_format, model, maximum_cost_usd, command, state, offered_at, review)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		action.SessionID, offer.Label, string(offer.Type), offer.RepoID, offer.SchedulerJobID, offer.Message, offer.ShellCommand,
		offer.WorkingDirectory, string(offer.ResultFormat), offer.Model, offer.MaximumCostUSD,
		action.Command, string(action.State), action.OfferedAt.UTC(), review,
	)
	if err != nil {
		return fmt.Errorf("insert session action for %s: %w", action.SessionID, err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert session action for %s: read its id: %w", action.SessionID, err)
	}
	action.ActionID = sessionActionIDOf(rowID)
	return nil
}

// ListSessionActions returns every action offered in a session, oldest first.
func (s *Store) ListSessionActions(sessionID string) ([]SessionAction, error) {
	rows, err := s.db.Query(sessionActionSelect+` WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	actions := []SessionAction{}
	for rows.Next() {
		action, err := scanSessionAction(rows)
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

// GetSessionAction returns one action of one session. An action of another
// session is ErrSessionActionNotFound, the same answer as a missing one: the
// session in the path is what access was checked against.
func (s *Store) GetSessionAction(sessionID, actionID string) (SessionAction, error) {
	rowID, ok := sessionActionRowIDOf(actionID)
	if !ok {
		return SessionAction{}, ErrSessionActionNotFound
	}
	action, err := scanSessionAction(s.db.QueryRow(sessionActionSelect+` WHERE session_id = ? AND id = ?`, sessionID, rowID))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionAction{}, ErrSessionActionNotFound
	}
	return action, err
}

// StartSessionAction moves an offered action to running, recording who
// confirmed it and when, and returns the record as it now is. Only one caller
// can win: a second confirm, or a confirm of an action that already ran, is
// ErrSessionActionNotOffered.
func (s *Store) StartSessionAction(sessionID, actionID, runByPrincipalID string, startedAt time.Time) (SessionAction, error) {
	rowID, ok := sessionActionRowIDOf(actionID)
	if !ok {
		return SessionAction{}, ErrSessionActionNotFound
	}
	result, err := s.db.Exec(
		`UPDATE session_actions SET state = ?, run_by_principal_id = ?, started_at = ?
		 WHERE session_id = ? AND id = ? AND state = ?`,
		string(msg.SessionActionRunning), runByPrincipalID, startedAt.UTC(), sessionID, rowID, string(msg.SessionActionOffered))
	if err != nil {
		return SessionAction{}, fmt.Errorf("start session action %s: %w", actionID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return SessionAction{}, fmt.Errorf("start session action %s: %w", actionID, err)
	}
	if changed == 0 {
		if _, err := s.GetSessionAction(sessionID, actionID); err != nil {
			return SessionAction{}, err
		}
		return SessionAction{}, ErrSessionActionNotOffered
	}
	return s.GetSessionAction(sessionID, actionID)
}

// SessionActionEnd is how a run ended.
type SessionActionEnd struct {
	State           msg.SessionActionState
	Output          string
	Error           string
	ResultSessionID string
	CostUSD         float64
}

// FinishSessionAction records how a running action's run ended and returns
// the record as it now is.
func (s *Store) FinishSessionAction(sessionID, actionID string, end SessionActionEnd, finishedAt time.Time) (SessionAction, error) {
	rowID, ok := sessionActionRowIDOf(actionID)
	if !ok {
		return SessionAction{}, ErrSessionActionNotFound
	}
	result, err := s.db.Exec(
		`UPDATE session_actions SET state = ?, output = ?, error = ?, result_session_id = ?, cost_usd = ?, finished_at = ?
		 WHERE session_id = ? AND id = ? AND state = ?`,
		string(end.State), end.Output, end.Error, end.ResultSessionID, end.CostUSD, finishedAt.UTC(), sessionID, rowID, string(msg.SessionActionRunning))
	if err != nil {
		return SessionAction{}, fmt.Errorf("finish session action %s: %w", actionID, err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return SessionAction{}, fmt.Errorf("finish session action %s: it is not running (rows changed %d, %v)", actionID, changed, err)
	}
	return s.GetSessionAction(sessionID, actionID)
}

// SetSessionActionResultSession records the session a running action started,
// as soon as it exists, and returns the record as it now is.
func (s *Store) SetSessionActionResultSession(sessionID, actionID, resultSessionID string) (SessionAction, error) {
	rowID, ok := sessionActionRowIDOf(actionID)
	if !ok {
		return SessionAction{}, ErrSessionActionNotFound
	}
	result, err := s.db.Exec(`UPDATE session_actions SET result_session_id = ? WHERE session_id = ? AND id = ? AND state = ?`,
		resultSessionID, sessionID, rowID, string(msg.SessionActionRunning))
	if err != nil {
		return SessionAction{}, fmt.Errorf("record the session %s started: %w", actionID, err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return SessionAction{}, fmt.Errorf("record the session %s started: it is not running (rows changed %d, %v)", actionID, changed, err)
	}
	return s.GetSessionAction(sessionID, actionID)
}

// MarkRunningSessionActionsOutcomeUnknown settles every action still running —
// which, called at startup, means one whose run this process's predecessor
// never saw end — as outcome_unknown, and returns them as they now are.
func (s *Store) MarkRunningSessionActionsOutcomeUnknown(explanation string, now time.Time) ([]SessionAction, error) {
	rows, err := s.db.Query(sessionActionSelect+` WHERE state = ? ORDER BY id`, string(msg.SessionActionRunning))
	if err != nil {
		return nil, err
	}
	running := []SessionAction{}
	for rows.Next() {
		action, err := scanSessionAction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		running = append(running, action)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	settled := make([]SessionAction, 0, len(running))
	for _, action := range running {
		after, err := s.FinishSessionAction(action.SessionID, action.ActionID, SessionActionEnd{
			State: msg.SessionActionOutcomeUnknown, Output: action.Output, Error: explanation,
			ResultSessionID: action.ResultSessionID, CostUSD: action.CostUSD,
		}, now)
		if err != nil {
			return settled, err
		}
		settled = append(settled, after)
	}
	return settled, nil
}

const sessionActionSelect = `SELECT id, session_id, label, type, repo_id, scheduler_job_id, message, shell_command, working_directory,
	result_format, model, maximum_cost_usd, command, state, offered_at, run_by_principal_id, started_at, finished_at, output, error,
	result_session_id, review, cost_usd FROM session_actions`

func sessionActionIDOf(rowID int64) string {
	return fmt.Sprintf("%s%06d", sessionActionIDPrefix, rowID)
}

func sessionActionRowIDOf(actionID string) (int64, bool) {
	digits, hasPrefix := strings.CutPrefix(actionID, sessionActionIDPrefix)
	if !hasPrefix {
		return 0, false
	}
	rowID, err := strconv.ParseInt(digits, 10, 64)
	return rowID, err == nil && rowID > 0
}

type sessionActionScanner interface {
	Scan(dest ...any) error
}

func scanSessionAction(scanner sessionActionScanner) (SessionAction, error) {
	var action SessionAction
	var rowID int64
	var actionType, resultFormat, state, review string
	var offeredAt time.Time
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(&rowID, &action.SessionID, &action.Offer.Label, &actionType, &action.Offer.RepoID, &action.Offer.SchedulerJobID,
		&action.Offer.Message, &action.Offer.ShellCommand, &action.Offer.WorkingDirectory, &resultFormat, &action.Offer.Model,
		&action.Offer.MaximumCostUSD, &action.Command, &state, &offeredAt, &action.RunByPrincipalID, &startedAt, &finishedAt,
		&action.Output, &action.Error, &action.ResultSessionID, &review, &action.CostUSD); err != nil {
		return SessionAction{}, err
	}
	action.ActionID = sessionActionIDOf(rowID)
	action.Offer.Type = msg.SessionActionType(actionType)
	action.Offer.ResultFormat = msg.SessionActionResultFormat(resultFormat)
	action.State = msg.SessionActionState(state)
	action.OfferedAt = offeredAt.UTC()
	if startedAt.Valid {
		started := startedAt.Time.UTC()
		action.StartedAt = &started
	}
	if finishedAt.Valid {
		finished := finishedAt.Time.UTC()
		action.FinishedAt = &finished
	}
	if review != "" {
		var decoded msg.SessionActionReview
		if err := json.Unmarshal([]byte(review), &decoded); err != nil {
			return SessionAction{}, fmt.Errorf("session action %s: stored review is not a review: %w", action.ActionID, err)
		}
		action.Review = &decoded
	}
	return action, nil
}

func encodeSessionActionReview(review *msg.SessionActionReview) (string, error) {
	if review == nil {
		return "", nil
	}
	encoded, err := json.Marshal(review)
	if err != nil {
		return "", fmt.Errorf("encode session action review: %w", err)
	}
	return string(encoded), nil
}
