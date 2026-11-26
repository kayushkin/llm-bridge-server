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

// InsertSessionAction records an offered action and sets its ActionID.
func (s *Store) InsertSessionAction(action *SessionAction) error {
	review, err := encodeSessionActionReview(action.Review)
	if err != nil {
		return err
	}
	offer := action.Offer
	inputs := ""
	if len(offer.Inputs) > 0 {
		encoded, err := json.Marshal(offer.Inputs)
		if err != nil {
			return fmt.Errorf("encode session action inputs: %w", err)
		}
		inputs = string(encoded)
	}
	var rowID int64
	if err := s.db.QueryRow(
		`INSERT INTO session_actions (session_id, label, type, repo_id, scheduler_job_id, message, shell_command, working_directory,
		 result_format, model, maximum_cost_usd, command, state, offered_at, review, inputs, target_session_id, report_success)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING id`,
		action.SessionID, offer.Label, string(offer.Type), offer.RepoID, offer.SchedulerJobID, offer.Message, offer.ShellCommand,
		offer.WorkingDirectory, string(offer.ResultFormat), offer.Model, offer.MaximumCostUSD,
		action.Command, string(action.State), action.OfferedAt.UTC(), review, inputs, offer.TargetSessionID, offer.ReportSuccess,
	).Scan(&rowID); err != nil {
		return fmt.Errorf("insert session action for %s: %w", action.SessionID, err)
	}
	action.ActionID = sessionActionIDOf(rowID)
	return nil
}

// ListSessionActions returns every action offered in a session, oldest first.
func (s *Store) ListSessionActions(sessionID string) ([]SessionAction, error) {
	rows, err := s.db.Query(sessionActionSelect+` WHERE session_id = $1 ORDER BY id`, sessionID)
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
	action, err := scanSessionAction(s.db.QueryRow(sessionActionSelect+` WHERE session_id = $1 AND id = $2`, sessionID, rowID))
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
		`UPDATE session_actions SET state = $1, run_by_principal_id = $2, started_at = $3
		 WHERE session_id = $4 AND id = $5 AND state = $6`,
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
		`UPDATE session_actions SET state = $1, output = $2, error = $3, result_session_id = $4, cost_usd = $5, finished_at = $6
		 WHERE session_id = $7 AND id = $8 AND state = $9`,
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
	result, err := s.db.Exec(`UPDATE session_actions SET result_session_id = $1 WHERE session_id = $2 AND id = $3 AND state = $4`,
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
	rows, err := s.db.Query(sessionActionSelect+` WHERE state = $1 ORDER BY id`, string(msg.SessionActionRunning))
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
	result_session_id, review, cost_usd, inputs, target_session_id, report_success FROM session_actions`

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
	var actionType, resultFormat, state, review, inputs string
	var offeredAt time.Time
	var startedAt, finishedAt sql.NullTime
	var reportSuccess sql.NullBool
	if err := scanner.Scan(&rowID, &action.SessionID, &action.Offer.Label, &actionType, &action.Offer.RepoID, &action.Offer.SchedulerJobID,
		&action.Offer.Message, &action.Offer.ShellCommand, &action.Offer.WorkingDirectory, &resultFormat, &action.Offer.Model,
		&action.Offer.MaximumCostUSD, &action.Command, &state, &offeredAt, &action.RunByPrincipalID, &startedAt, &finishedAt,
		&action.Output, &action.Error, &action.ResultSessionID, &review, &action.CostUSD, &inputs, &action.Offer.TargetSessionID, &reportSuccess); err != nil {
		return SessionAction{}, err
	}
	if reportSuccess.Valid {
		action.Offer.ReportSuccess = &reportSuccess.Bool
	}
	if inputs != "" {
		if err := json.Unmarshal([]byte(inputs), &action.Offer.Inputs); err != nil {
			return SessionAction{}, fmt.Errorf("decode the inputs of %s: %w", sessionActionIDOf(rowID), err)
		}
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

// SessionHasRunningCommandAction reports whether a run_command button of the
// session is running now: a person confirmed it, and its shell holds the
// session's posting token.
func (s *Store) SessionHasRunningCommandAction(sessionID string) (bool, error) {
	var running bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM session_actions WHERE session_id = $1 AND type = $2 AND state = $3)`,
		sessionID, string(msg.SessionActionRunCommand), string(msg.SessionActionRunning)).Scan(&running)
	return running, err
}
