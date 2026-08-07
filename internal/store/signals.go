package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Signal is the canonical type from llm-bridge/msg/signal.go. As with
// Session, the API type lives in msg/ so the generated TypeScript stays in
// sync; this alias keeps store code reading naturally.
type Signal = msg.Signal

// SignalFilter narrows a ListSignals query. A zero filter lists everything.
type SignalFilter struct {
	SessionID string
	State     msg.SignalState
	Surface   msg.SignalSurface
	Kind      msg.SignalKind
	// LinkedTodoID narrows to the signals propagated to one noteboard todo.
	// This is the query a todo surface makes to decide whether to show a
	// badge, so it must never match the unlinked rows: an empty filter means
	// "don't narrow", never "the ones with no link".
	LinkedTodoID string
	// Limit caps the returned rows. Zero means no cap.
	Limit int
}

// migrateSignals creates the signal table. Called from migrate().
func (s *Store) migrateSignals() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS signals (
			id             TEXT PRIMARY KEY,
			session_id     TEXT NOT NULL,
			session_type   TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			source         TEXT NOT NULL,
			request_id     TEXT NOT NULL DEFAULT '',
			surface        TEXT NOT NULL,
			title          TEXT NOT NULL,
			body           TEXT NOT NULL DEFAULT '',
			options        TEXT NOT NULL DEFAULT '',
			allow_freeform INTEGER NOT NULL DEFAULT 0,
			answer         TEXT NOT NULL DEFAULT '',
			severity       TEXT NOT NULL DEFAULT '',
			state          TEXT NOT NULL,
			linked_todo_id TEXT NOT NULL DEFAULT '',
			created_at     DATETIME NOT NULL,
			resolved_at    DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_signals_session ON signals(session_id);
		CREATE INDEX IF NOT EXISTS idx_signals_state ON signals(state);
		CREATE INDEX IF NOT EXISTS idx_signals_request ON signals(session_id, request_id) WHERE request_id != '';
		CREATE INDEX IF NOT EXISTS idx_signals_todo ON signals(linked_todo_id) WHERE linked_todo_id != '';
	`)
	return err
}

// signalColumns selects the fields scanSignal reads, in its order.
const signalColumns = `id, session_id, session_type, kind, source, request_id, surface, title, body, options, allow_freeform, answer, severity, state, linked_todo_id, created_at, resolved_at`

func scanSignal(sc interface{ Scan(...any) error }) (*Signal, error) {
	var sig Signal
	var sessionType, kind, source, surface, severity, state string
	var options, answer string
	var resolvedAt sql.NullTime
	err := sc.Scan(
		&sig.ID, &sig.SessionID, &sessionType, &kind, &source, &sig.RequestID,
		&surface, &sig.Title, &sig.Body, &options, &sig.AllowFreeform, &answer,
		&severity, &state, &sig.LinkedTodoID, &sig.CreatedAt, &resolvedAt,
	)
	if err != nil {
		return nil, err
	}
	sig.SessionType = msg.SessionType(sessionType)
	sig.Kind = msg.SignalKind(kind)
	sig.Source = msg.SignalSource(source)
	sig.Surface = msg.SignalSurface(surface)
	sig.Severity = msg.SignalSeverity(severity)
	sig.State = msg.SignalState(state)
	if options != "" {
		if err := json.Unmarshal([]byte(options), &sig.Options); err != nil {
			return nil, fmt.Errorf("signal %s: unmarshal options: %w", sig.ID, err)
		}
	}
	if answer != "" {
		var parsed msg.SignalAnswer
		if err := json.Unmarshal([]byte(answer), &parsed); err != nil {
			return nil, fmt.Errorf("signal %s: unmarshal answer: %w", sig.ID, err)
		}
		sig.Answer = &parsed
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		sig.ResolvedAt = &t
	}
	return &sig, nil
}

// CreateSignal persists a new signal. The caller supplies ID; CreatedAt and
// State default to now/open when left zero.
func (s *Store) CreateSignal(sig *Signal) error {
	if sig.ID == "" {
		return fmt.Errorf("signal id is required")
	}
	if sig.SessionID == "" {
		return fmt.Errorf("signal session_id is required")
	}
	if sig.CreatedAt.IsZero() {
		sig.CreatedAt = time.Now().UTC()
	}
	if sig.State == "" {
		sig.State = msg.SignalStateOpen
	}
	options := ""
	if len(sig.Options) > 0 {
		raw, err := json.Marshal(sig.Options)
		if err != nil {
			return fmt.Errorf("marshal signal options: %w", err)
		}
		options = string(raw)
	}
	answer := ""
	if sig.Answer != nil {
		raw, err := json.Marshal(sig.Answer)
		if err != nil {
			return fmt.Errorf("marshal signal answer: %w", err)
		}
		answer = string(raw)
	}
	var resolvedAt any
	if sig.ResolvedAt != nil {
		resolvedAt = *sig.ResolvedAt
	}
	_, err := s.db.Exec(
		`INSERT INTO signals (`+signalColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sig.ID, sig.SessionID, string(sig.SessionType), string(sig.Kind), string(sig.Source),
		sig.RequestID, string(sig.Surface), sig.Title, sig.Body, options, sig.AllowFreeform,
		answer, string(sig.Severity), string(sig.State), sig.LinkedTodoID, sig.CreatedAt, resolvedAt,
	)
	return err
}

// GetSignal looks up one signal by id.
func (s *Store) GetSignal(id string) (*Signal, error) {
	return scanSignal(s.dbRO.QueryRow(`SELECT `+signalColumns+` FROM signals WHERE id=?`, id))
}

// ListSignals returns signals matching the filter, newest first.
func (s *Store) ListSignals(filter SignalFilter) ([]Signal, error) {
	query := `SELECT ` + signalColumns + ` FROM signals WHERE 1=1`
	var args []any
	if filter.SessionID != "" {
		query += ` AND session_id=?`
		args = append(args, filter.SessionID)
	}
	if filter.State != "" {
		query += ` AND state=?`
		args = append(args, string(filter.State))
	}
	if filter.Surface != "" {
		query += ` AND surface=?`
		args = append(args, string(filter.Surface))
	}
	if filter.Kind != "" {
		query += ` AND kind=?`
		args = append(args, string(filter.Kind))
	}
	if filter.LinkedTodoID != "" {
		query += ` AND linked_todo_id=?`
		args = append(args, filter.LinkedTodoID)
	}
	// created_at ties are common (one AskUserQuestion mints a row per
	// question in the same millisecond), so id breaks the tie — without it
	// SQLite picks, and a LIMIT would drop arbitrary rows.
	query += ` ORDER BY created_at DESC, id`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := s.dbRO.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var signals []Signal
	for rows.Next() {
		sig, err := scanSignal(rows)
		if err != nil {
			return nil, err
		}
		signals = append(signals, *sig)
	}
	return signals, rows.Err()
}

// ListSignalsByRequestID returns the signals minted from one parked hook
// request. A single AskUserQuestion carries an array of questions and mints
// one signal per question, so this is a set, not a single row.
func (s *Store) ListSignalsByRequestID(sessionID, requestID string) ([]Signal, error) {
	if requestID == "" {
		return nil, nil
	}
	rows, err := s.dbRO.Query(
		`SELECT `+signalColumns+` FROM signals WHERE session_id=? AND request_id=? ORDER BY created_at, id`,
		sessionID, requestID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var signals []Signal
	for rows.Next() {
		sig, err := scanSignal(rows)
		if err != nil {
			return nil, err
		}
		signals = append(signals, *sig)
	}
	return signals, rows.Err()
}

// ResolveSignal moves a signal out of the open state, stamping the answer
// and resolved_at. Passing a nil answer resolves without one (a dismissal,
// or a notification acknowledgement). Resolving an already-resolved signal
// is a no-op: the first resolution is the one that happened, and a duplicate
// resolve click must not overwrite it.
func (s *Store) ResolveSignal(id string, state msg.SignalState, answer *msg.SignalAnswer) error {
	if state == msg.SignalStateOpen {
		return fmt.Errorf("ResolveSignal: %q is not a resolved state", state)
	}
	payload := ""
	if answer != nil {
		raw, err := json.Marshal(answer)
		if err != nil {
			return fmt.Errorf("marshal signal answer: %w", err)
		}
		payload = string(raw)
	}
	_, err := s.db.Exec(
		`UPDATE signals SET state=?, answer=?, resolved_at=? WHERE id=? AND state=?`,
		string(state), payload, time.Now().UTC(), id, string(msg.SignalStateOpen),
	)
	return err
}
