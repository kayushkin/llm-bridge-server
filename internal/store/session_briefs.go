package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Session briefs: what a chat is for, where it stands, what each turn changed,
// and what it said that nobody answered. The types are msg's (session_brief.go);
// the brief pass in the server writes them at the end of each chat turn.

// ErrLooseEndNotOpen is returned when a loose end is closed twice. Two surfaces
// closing the same one race here by design, and the first close is the one
// that happened.
var ErrLooseEndNotOpen = errors.New("loose end is not open")

// migrateSessionBriefs creates the three brief tables. Called from migrate().
func (s *Store) migrateSessionBriefs() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS session_briefs (
			session_id     TEXT PRIMARY KEY,
			overall_goal   TEXT NOT NULL,
			current_step   TEXT NOT NULL,
			revision_count INTEGER NOT NULL,
			created_at     DATETIME NOT NULL,
			updated_at     DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_brief_revisions (
			id           TEXT PRIMARY KEY,
			session_id   TEXT NOT NULL,
			overall_goal TEXT NOT NULL,
			current_step TEXT NOT NULL,
			goal_changed INTEGER NOT NULL,
			turn_change  TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_session_brief_revisions_session ON session_brief_revisions(session_id, created_at);
		CREATE TABLE IF NOT EXISTS loose_ends (
			id          TEXT PRIMARY KEY,
			session_id  TEXT NOT NULL,
			kind        TEXT NOT NULL,
			text        TEXT NOT NULL,
			context     TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL,
			resolution  TEXT NOT NULL DEFAULT '',
			created_at  DATETIME NOT NULL,
			resolved_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_loose_ends_session ON loose_ends(session_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_loose_ends_state ON loose_ends(state);
	`)
	return err
}

// SessionBriefUpdate is one turn's update to a session's brief: the brief as
// the turn left it, the loose ends it settled and the ones it raised. Applied
// in one transaction, so a reader never sees the new brief beside the old
// loose ends.
type SessionBriefUpdate struct {
	SessionID string
	Revision  msg.SessionBriefRevision
	// Settled maps a loose end's id to what the turn did that settled it.
	Settled map[string]string
	Raised  []msg.LooseEnd
}

// ApplySessionBriefUpdate writes the revision, moves the brief to it, closes
// the settled loose ends that are still open and adds the raised ones.
func (s *Store) ApplySessionBriefUpdate(update SessionBriefUpdate) error {
	if update.SessionID == "" || update.Revision.ID == "" {
		return fmt.Errorf("a brief update needs a session id and a revision id")
	}
	if update.Revision.CreatedAt.IsZero() {
		update.Revision.CreatedAt = time.Now().UTC()
	}
	now := update.Revision.CreatedAt
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	revision := update.Revision
	if _, err := tx.Exec(
		`INSERT INTO session_brief_revisions (id, session_id, overall_goal, current_step, goal_changed, turn_change, created_at) VALUES (?,?,?,?,?,?,?)`,
		revision.ID, update.SessionID, revision.OverallGoal, revision.CurrentStep, revision.GoalChanged, revision.TurnChange, now,
	); err != nil {
		return fmt.Errorf("insert brief revision: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO session_briefs (session_id, overall_goal, current_step, revision_count, created_at, updated_at) VALUES (?,?,?,1,?,?)
		 ON CONFLICT(session_id) DO UPDATE SET overall_goal=excluded.overall_goal, current_step=excluded.current_step,
		   revision_count=session_briefs.revision_count+1, updated_at=excluded.updated_at`,
		update.SessionID, revision.OverallGoal, revision.CurrentStep, now, now,
	); err != nil {
		return fmt.Errorf("upsert brief: %w", err)
	}
	for id, resolution := range update.Settled {
		// Only an open loose end of this session moves. An id the model made
		// up, or one the person closed while the turn ran, changes nothing.
		if _, err := tx.Exec(
			`UPDATE loose_ends SET state=?, resolution=?, resolved_at=? WHERE id=? AND session_id=? AND state=?`,
			string(msg.LooseEndSettled), resolution, now, id, update.SessionID, string(msg.LooseEndOpen),
		); err != nil {
			return fmt.Errorf("settle loose end %s: %w", id, err)
		}
	}
	for _, looseEnd := range update.Raised {
		if looseEnd.ID == "" {
			return fmt.Errorf("a raised loose end needs an id")
		}
		if _, err := tx.Exec(
			`INSERT INTO loose_ends (id, session_id, kind, text, context, state, created_at) VALUES (?,?,?,?,?,?,?)`,
			looseEnd.ID, update.SessionID, string(looseEnd.Kind), looseEnd.Text, looseEnd.Context, string(msg.LooseEndOpen), now,
		); err != nil {
			return fmt.Errorf("insert loose end: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifySessionBriefChanged(update.SessionID)
	return nil
}

// CloseLooseEnd moves one open loose end to answered or dismissed. Returns
// ErrLooseEndNotOpen when it is already closed and sql.ErrNoRows when there is
// no such loose end.
func (s *Store) CloseLooseEnd(id string, state msg.LooseEndState, resolution string) error {
	if state != msg.LooseEndAnswered && state != msg.LooseEndDismissed && state != msg.LooseEndSettled {
		return fmt.Errorf("CloseLooseEnd: %q is not a closed state", state)
	}
	looseEnd, err := s.GetLooseEnd(id)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE loose_ends SET state=?, resolution=?, resolved_at=? WHERE id=? AND state=?`,
		string(state), resolution, time.Now().UTC(), id, string(msg.LooseEndOpen),
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrLooseEndNotOpen
	}
	s.notifySessionBriefChanged(looseEnd.SessionID)
	return nil
}

const looseEndColumns = `id, session_id, kind, text, context, state, resolution, created_at, resolved_at`

func scanLooseEnd(sc interface{ Scan(...any) error }) (*msg.LooseEnd, error) {
	var looseEnd msg.LooseEnd
	var kind, state string
	var resolvedAt sql.NullTime
	if err := sc.Scan(&looseEnd.ID, &looseEnd.SessionID, &kind, &looseEnd.Text, &looseEnd.Context,
		&state, &looseEnd.Resolution, &looseEnd.CreatedAt, &resolvedAt); err != nil {
		return nil, err
	}
	looseEnd.Kind = msg.LooseEndKind(kind)
	looseEnd.State = msg.LooseEndState(state)
	if resolvedAt.Valid {
		t := resolvedAt.Time
		looseEnd.ResolvedAt = &t
	}
	return &looseEnd, nil
}

// GetLooseEnd reads one loose end; sql.ErrNoRows when there is none.
func (s *Store) GetLooseEnd(id string) (*msg.LooseEnd, error) {
	return scanLooseEnd(s.dbRO.QueryRow(`SELECT `+looseEndColumns+` FROM loose_ends WHERE id=?`, id))
}

// SessionBriefFilter narrows ListSessionBriefs.
type SessionBriefFilter struct {
	// SessionIDs narrows to these sessions. Empty means every briefed session.
	SessionIDs []string
	// LooseEndState narrows the loose ends returned beside each brief.
	// Empty returns them all.
	LooseEndState msg.LooseEndState
	// OwnedByPrincipalID and OwnedThroughPeerPrincipalID narrow to the
	// sessions a restricted caller may see, as on SignalFilter.
	OwnedByPrincipalID          string
	OwnedThroughPeerPrincipalID string
	// UpdatedSince drops briefs not updated since then. Zero means no bound.
	UpdatedSince time.Time
	// Limit caps the briefs, newest first. Zero means no cap.
	Limit int
}

// ListSessionBriefs returns each matching session's brief with its revisions
// and loose ends, the most recently updated brief first. A session that has
// loose ends but no brief cannot occur: both are written by one transaction.
func (s *Store) ListSessionBriefs(filter SessionBriefFilter) ([]msg.SessionBriefView, error) {
	query := `SELECT session_id, overall_goal, current_step, revision_count, created_at, updated_at FROM session_briefs WHERE 1=1`
	var args []any
	if len(filter.SessionIDs) > 0 {
		query += ` AND session_id IN (` + placeholders(len(filter.SessionIDs)) + `)`
		for _, id := range filter.SessionIDs {
			args = append(args, id)
		}
	}
	if filter.OwnedByPrincipalID != "" {
		query += ` AND session_id IN (SELECT bridge_id FROM sessions WHERE principal_id=?)`
		args = append(args, filter.OwnedByPrincipalID)
	}
	if filter.OwnedThroughPeerPrincipalID != "" {
		query += ` AND session_id IN (SELECT session_id FROM session_peer_origins WHERE peer_principal_id=?)`
		args = append(args, filter.OwnedThroughPeerPrincipalID)
	}
	if !filter.UpdatedSince.IsZero() {
		query += ` AND updated_at >= ?`
		args = append(args, filter.UpdatedSince.UTC())
	}
	query += ` ORDER BY updated_at DESC, session_id`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := s.dbRO.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var views []msg.SessionBriefView
	index := map[string]int{}
	for rows.Next() {
		var brief msg.SessionBrief
		if err := rows.Scan(&brief.SessionID, &brief.OverallGoal, &brief.CurrentStep, &brief.RevisionCount, &brief.CreatedAt, &brief.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		index[brief.SessionID] = len(views)
		briefCopy := brief
		views = append(views, msg.SessionBriefView{Brief: &briefCopy, Revisions: []msg.SessionBriefRevision{}, LooseEnds: []msg.LooseEnd{}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return []msg.SessionBriefView{}, nil
	}

	sessionIDs := make([]any, 0, len(views))
	for _, view := range views {
		sessionIDs = append(sessionIDs, view.Brief.SessionID)
	}
	revisionRows, err := s.dbRO.Query(
		`SELECT id, session_id, overall_goal, current_step, goal_changed, turn_change, created_at FROM session_brief_revisions
		 WHERE session_id IN (`+placeholders(len(sessionIDs))+`) ORDER BY created_at, id`, sessionIDs...)
	if err != nil {
		return nil, err
	}
	for revisionRows.Next() {
		var revision msg.SessionBriefRevision
		if err := revisionRows.Scan(&revision.ID, &revision.SessionID, &revision.OverallGoal, &revision.CurrentStep,
			&revision.GoalChanged, &revision.TurnChange, &revision.CreatedAt); err != nil {
			revisionRows.Close()
			return nil, err
		}
		position := index[revision.SessionID]
		views[position].Revisions = append(views[position].Revisions, revision)
	}
	revisionRows.Close()
	if err := revisionRows.Err(); err != nil {
		return nil, err
	}

	looseEndQuery := `SELECT ` + looseEndColumns + ` FROM loose_ends WHERE session_id IN (` + placeholders(len(sessionIDs)) + `)`
	looseEndArgs := append([]any{}, sessionIDs...)
	if filter.LooseEndState != "" {
		looseEndQuery += ` AND state=?`
		looseEndArgs = append(looseEndArgs, string(filter.LooseEndState))
	}
	looseEndRows, err := s.dbRO.Query(looseEndQuery+` ORDER BY created_at, id`, looseEndArgs...)
	if err != nil {
		return nil, err
	}
	defer looseEndRows.Close()
	for looseEndRows.Next() {
		looseEnd, err := scanLooseEnd(looseEndRows)
		if err != nil {
			return nil, err
		}
		position := index[looseEnd.SessionID]
		views[position].LooseEnds = append(views[position].LooseEnds, *looseEnd)
	}
	return views, looseEndRows.Err()
}

// GetSessionBriefView reads one session's brief, history and loose ends. A
// session no turn has briefed yet answers with a nil Brief and empty lists.
func (s *Store) GetSessionBriefView(sessionID string) (msg.SessionBriefView, error) {
	views, err := s.ListSessionBriefs(SessionBriefFilter{SessionIDs: []string{sessionID}})
	if err != nil {
		return msg.SessionBriefView{}, err
	}
	if len(views) == 0 {
		return msg.SessionBriefView{Revisions: []msg.SessionBriefRevision{}, LooseEnds: []msg.LooseEnd{}}, nil
	}
	return views[0], nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// CurrentTurnText returns what the person said to start the current turn and
// every piece of text the assistant has written since, oldest first. Thinking
// and tool calls are left out: the brief is about what was said to the person.
// The current turn starts at the session's last user_message; a session with
// none yet answers with an empty message and the text of everything.
func (s *Store) CurrentTurnText(sessionID string) (userMessage string, assistantTexts []string, err error) {
	var lastUserMessageID int64
	var lastUserMessage sql.NullString
	err = s.dbRO.QueryRow(
		`SELECT id, json_extract(data, '$.result.text') FROM events WHERE session_id=? AND type='user_message' ORDER BY id DESC LIMIT 1`,
		sessionID,
	).Scan(&lastUserMessageID, &lastUserMessage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	rows, err := s.dbRO.Query(
		`SELECT json_extract(data, '$.block.block.text_block.text') FROM events
		 WHERE session_id=? AND id > ? AND type='block' AND json_extract(data, '$.block.block.type')='text'
		 ORDER BY id`,
		sessionID, lastUserMessageID,
	)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var text sql.NullString
		if err := rows.Scan(&text); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(text.String) != "" {
			assistantTexts = append(assistantTexts, text.String)
		}
	}
	return lastUserMessage.String, assistantTexts, rows.Err()
}

// FirstUserMessage returns the text of the session's first user_message, or ""
// when it has none. The brief pass shows it on every turn, so a brief first
// written late in a chat, or on a turn an automated report started, still
// knows what the person asked for.
func (s *Store) FirstUserMessage(sessionID string) (string, error) {
	var text sql.NullString
	err := s.dbRO.QueryRow(
		`SELECT json_extract(data, '$.result.text') FROM events WHERE session_id=? AND type='user_message' ORDER BY id LIMIT 1`,
		sessionID,
	).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text.String, err
}
