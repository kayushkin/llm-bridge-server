package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/kanban-store/internal/model"
)

// migrateActivity adds the tables that make a card's history readable: the event
// log every figure is computed from, the notes written onto cards, and each
// board's priority ladder.
//
// It also widens two existing tables. Both ALTERs are guarded by a lookup rather
// than by swallowing the "duplicate column" error, so a genuinely failed
// migration still surfaces instead of being mistaken for one that already ran.
func migrateActivity(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS card_events (
			id             TEXT PRIMARY KEY,
			card_id        TEXT NOT NULL,
			board_id       TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			clock_state    TEXT NOT NULL,
			actor          TEXT NOT NULL DEFAULT '',
			summary        TEXT NOT NULL DEFAULT '',
			from_column_id TEXT NOT NULL DEFAULT '',
			to_column_id   TEXT NOT NULL DEFAULT '',
			note_id        TEXT NOT NULL DEFAULT '',
			detail         TEXT NOT NULL DEFAULT '',
			occurred_at    DATETIME NOT NULL,
			recorded_at    DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_card_events_card  ON card_events(card_id, occurred_at);
		CREATE INDEX IF NOT EXISTS idx_card_events_board ON card_events(board_id, occurred_at);

		CREATE TABLE IF NOT EXISTS card_notes (
			id         TEXT PRIMARY KEY,
			card_id    TEXT NOT NULL,
			board_id   TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT 'note',
			body       TEXT NOT NULL,
			actor      TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_card_notes_card ON card_notes(card_id, created_at);

		CREATE TABLE IF NOT EXISTS board_priority_levels (
			board_id       TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			priority_value INTEGER NOT NULL,
			label          TEXT NOT NULL,
			budget_seconds INTEGER,
			PRIMARY KEY (board_id, priority_value)
		);
	`)
	if err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "card_events", "supersedes_event_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// One successor per event, as a rule of the database and not of the code
	// that writes: two corrections of the same entry made at once cannot both
	// land, so an entry's history is a line and never a fork.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_card_events_supersedes
		ON card_events(supersedes_event_id) WHERE supersedes_event_id != ''`); err != nil {
		return err
	}
	// Every note written before visibility existed was written for the people
	// working the card, so the column's default is the right value for them.
	if err := addColumnIfMissing(db, "card_notes", "visibility", "TEXT NOT NULL DEFAULT 'internal'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "boards", "business_hours", "TEXT"); err != nil {
		return err
	}
	// Board settings a dispatcher, a classifier or llm-bridge-server reads
	// instead of taking as a flag. Every one is nullable: absent means "no
	// board-level answer".
	for _, column := range []string{"default_principal_id", "default_agent_id", "default_instance_id", "default_bundle_id", "classifier", "assignment_pool", "organization_id"} {
		if err := addColumnIfMissing(db, "boards", column, "TEXT"); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(db, "board_priority_levels", "default_auto_hold_at_usd", "REAL"); err != nil {
		return err
	}
	return addColumnIfMissing(db, "columns", "budget_clock_state", "TEXT")
}

func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// clockStateValue renders a column's clock setting for storage: an unset one is
// stored as NULL, not as an empty string that would read back as a state.
func clockStateValue(s *model.ClockState) any {
	if s == nil || *s == "" {
		return nil
	}
	return string(*s)
}

// ============================ Card events ============================

// RecordCardEvent appends one action to the log. The log is append-only by
// design: there is no update path, because rewriting an event would silently
// restate a card's history and every figure derived from it.
func (s *Store) RecordCardEvent(e *model.CardEvent) (*model.CardEvent, error) {
	if e.CardID == "" {
		return nil, errors.New("card_id is required")
	}
	if e.Kind == "" {
		return nil, errors.New("kind is required")
	}
	if !model.ValidClockState(e.ClockState) {
		return nil, fmt.Errorf("invalid clock_state %q", e.ClockState)
	}
	stored := *e
	stored.ID = uuid.NewString()
	stored.RecordedAt = now()
	if stored.OccurredAt.IsZero() {
		stored.OccurredAt = stored.RecordedAt
	}
	stored.OccurredAt = stored.OccurredAt.UTC()
	detail := ""
	if len(stored.Detail) > 0 {
		detail = string(stored.Detail)
	}
	_, err := s.db.Exec(
		`INSERT INTO card_events
		   (id, card_id, board_id, kind, clock_state, actor, summary, from_column_id, to_column_id, note_id, supersedes_event_id, detail, occurred_at, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stored.ID, stored.CardID, stored.BoardID, string(stored.Kind), string(stored.ClockState),
		stored.Actor, stored.Summary, stored.FromColumnID, stored.ToColumnID, stored.NoteID,
		stored.SupersedesEventID, detail, stored.OccurredAt, stored.RecordedAt,
	)
	if err != nil {
		// The only unique rule on this table besides the primary key is "one
		// successor per event", and the id above is freshly minted.
		if stored.SupersedesEventID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrEventAlreadySuperseded
		}
		return nil, err
	}
	return &stored, nil
}

// ErrEventAlreadySuperseded refuses a second successor for one event.
var ErrEventAlreadySuperseded = errors.New("that event has already been superseded")

// GetCardEvent reads one event by id.
func (s *Store) GetCardEvent(eventID string) (*model.CardEvent, error) {
	e, err := scanCardEvent(s.db.QueryRow(`SELECT `+cardEventColumns+` FROM card_events WHERE id = ?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// SupersederOfCardEvent is the event that replaced eventID, or ErrNotFound when
// nothing has.
func (s *Store) SupersederOfCardEvent(eventID string) (*model.CardEvent, error) {
	e, err := scanCardEvent(s.db.QueryRow(`SELECT `+cardEventColumns+` FROM card_events WHERE supersedes_event_id = ?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

const cardEventColumns = `id, card_id, board_id, kind, clock_state, actor, summary,
	from_column_id, to_column_id, note_id, supersedes_event_id, detail, occurred_at, recorded_at`

func scanCardEvent(r scanner) (model.CardEvent, error) {
	var e model.CardEvent
	var kind, state, detail string
	if err := r.Scan(&e.ID, &e.CardID, &e.BoardID, &kind, &state, &e.Actor, &e.Summary,
		&e.FromColumnID, &e.ToColumnID, &e.NoteID, &e.SupersedesEventID, &detail, &e.OccurredAt, &e.RecordedAt); err != nil {
		return e, err
	}
	e.Kind = model.EventKind(kind)
	e.ClockState = model.ClockState(state)
	if detail != "" {
		e.Detail = json.RawMessage(detail)
	}
	return e, nil
}

// ListCardEvents returns a card's actions oldest first.
//
// boardID scopes the answer to one board's timeline: board-specific actions on
// that board, plus the card-wide ones — an email arriving concerns the card
// wherever it sits, so it belongs on every board's timeline. An empty boardID
// returns everything.
func (s *Store) ListCardEvents(cardID, boardID string) ([]model.CardEvent, error) {
	q := `SELECT ` + cardEventColumns + ` FROM card_events WHERE card_id = ?`
	args := []any{cardID}
	if boardID != "" {
		q += ` AND (board_id = ? OR board_id = '')`
		args = append(args, boardID)
	}
	q += ` ORDER BY occurred_at ASC, recorded_at ASC, id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CardEvent{}
	for rows.Next() {
		e, err := scanCardEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListCardEventsForBoard fetches the events of every card placed on a board in
// one query, grouped by card. Assembling a board otherwise means one query per
// card, which is the shape that already makes the links fan-out expensive here.
func (s *Store) ListCardEventsForBoard(boardID string) (map[string][]model.CardEvent, error) {
	rows, err := s.db.Query(
		`SELECT `+cardEventColumns+` FROM card_events
		 WHERE (board_id = ? OR board_id = '')
		   AND card_id IN (SELECT card_id FROM placements WHERE board_id = ?)
		 ORDER BY occurred_at ASC, recorded_at ASC, id ASC`, boardID, boardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.CardEvent{}
	for rows.Next() {
		e, err := scanCardEvent(rows)
		if err != nil {
			return nil, err
		}
		out[e.CardID] = append(out[e.CardID], e)
	}
	return out, rows.Err()
}

// ============================ Card notes ============================

func (s *Store) CreateCardNote(cardID string, req *model.CreateCardNoteRequest) (*model.CardNote, error) {
	n := &model.CardNote{
		ID:        uuid.NewString(),
		CardID:    cardID,
		BoardID:   req.BoardID,
		Kind:      req.Kind,
		Body:      req.Body,
		Actor:     req.Actor,
		CreatedAt: now(),
	}
	if n.Kind == "" {
		n.Kind = "note"
	}
	n.Visibility = req.Visibility
	if n.Visibility == "" {
		n.Visibility = model.DefaultNoteVisibility
	}
	if req.OccurredAt != nil {
		n.CreatedAt = req.OccurredAt.UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO card_notes (id, card_id, board_id, kind, visibility, body, actor, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.CardID, n.BoardID, n.Kind, string(n.Visibility), n.Body, n.Actor, n.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// ListCardNotes lists a card's notes, oldest first. An empty visibility keeps
// every note; a visibility keeps only the notes written for it.
func (s *Store) ListCardNotes(cardID, boardID string, visibility model.NoteVisibility) ([]model.CardNote, error) {
	q := `SELECT id, card_id, board_id, kind, visibility, body, actor, created_at FROM card_notes WHERE card_id = ?`
	args := []any{cardID}
	if boardID != "" {
		q += ` AND (board_id = ? OR board_id = '')`
		args = append(args, boardID)
	}
	if visibility != "" {
		q += ` AND visibility = ?`
		args = append(args, string(visibility))
	}
	q += ` ORDER BY created_at ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CardNote{}
	for rows.Next() {
		var n model.CardNote
		if err := rows.Scan(&n.ID, &n.CardID, &n.BoardID, &n.Kind, &n.Visibility, &n.Body, &n.Actor, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCardNote(noteID string) error {
	res, err := s.db.Exec(`DELETE FROM card_notes WHERE id = ?`, noteID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ Priority ladder ============================

// GetPriorityLadder returns a board's rungs, top rung first — and the top rung is
// the highest priority_value, because that is the end of noteboard's scale the
// urgent work sits at.
func (s *Store) GetPriorityLadder(boardID string) (*model.PriorityLadder, error) {
	rows, err := s.db.Query(
		`SELECT board_id, priority_value, label, budget_seconds, default_auto_hold_at_usd FROM board_priority_levels
		 WHERE board_id = ? ORDER BY priority_value DESC`, boardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ladder := &model.PriorityLadder{BoardID: boardID, Levels: []model.BoardPriorityLevel{}}
	for rows.Next() {
		var lv model.BoardPriorityLevel
		var budget sql.NullInt64
		var defaultAutoHoldAtUSD sql.NullFloat64
		if err := rows.Scan(&lv.BoardID, &lv.PriorityValue, &lv.Label, &budget, &defaultAutoHoldAtUSD); err != nil {
			return nil, err
		}
		if defaultAutoHoldAtUSD.Valid {
			v := defaultAutoHoldAtUSD.Float64
			lv.DefaultAutoHoldAtUSD = &v
		}
		if budget.Valid {
			v := int(budget.Int64)
			lv.BudgetSeconds = &v
		}
		ladder.Levels = append(ladder.Levels, lv)
	}
	return ladder, rows.Err()
}

// SetPriorityLadder replaces a board's ladder wholesale. Rungs only mean anything
// relative to each other, so there is no way to edit one in isolation. An empty
// set of levels is a legitimate ladder: it means the board ignores priorities.
func (s *Store) SetPriorityLadder(boardID string, levels []model.BoardPriorityLevel) (*model.PriorityLadder, error) {
	if _, err := s.GetBoard(boardID); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM board_priority_levels WHERE board_id = ?`, boardID); err != nil {
		return nil, err
	}
	for _, lv := range levels {
		var budget any
		if lv.BudgetSeconds != nil {
			budget = *lv.BudgetSeconds
		}
		var defaultAutoHoldAtUSD any
		if lv.DefaultAutoHoldAtUSD != nil {
			defaultAutoHoldAtUSD = *lv.DefaultAutoHoldAtUSD
		}
		if _, err := tx.Exec(
			`INSERT INTO board_priority_levels (board_id, priority_value, label, budget_seconds, default_auto_hold_at_usd) VALUES (?, ?, ?, ?, ?)`,
			boardID, lv.PriorityValue, lv.Label, budget, defaultAutoHoldAtUSD,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPriorityLadder(boardID)
}

// ============================ Time helpers ============================

// EventTime picks the moment an action happened, defaulting to now. Callers hand
// it a caller-supplied timestamp that may be nil.
func EventTime(at *time.Time) time.Time {
	if at == nil {
		return now()
	}
	return at.UTC()
}

// ============================ Paged reads ============================

// ListPlacementsByColumn returns one column's cards in stored order, a page at a
// time. limit <= 0 means the whole column.
//
// Stored order is the only order this service can page in: what a board sorts by
// on screen — priority, due date, title — lives in noteboard, and kanban-store
// would have to fetch every item on the board to sort by it, which is the cost
// paging exists to avoid.
func (s *Store) ListPlacementsByColumn(columnID string, limit, offset int) ([]*model.Placement, error) {
	return s.ListPlacementsByColumnMatching(columnID, CardFilter{}, limit, offset)
}

// CardFilter narrows a board or column read to the cards that match every
// field set. Every field is a fact this store owns — who is on the card, and
// the ticket row — so the filter runs in the same query that pages the column
// and ColumnView.Total stays exact: "showing 25 of 310 matching".
//
// What a card says — its tags, its priority, its due date — lives in noteboard
// and is not filterable here: this store would have to fetch every item on the
// board to look, which is the cost paging exists to avoid.
type CardFilter struct {
	CardID string
	// AssigneePrincipalID keeps cards this principal is assigned to.
	AssigneePrincipalID string
	// Unassigned keeps cards nobody is assigned to.
	Unassigned bool
	// TicketsOnly keeps cards that are tickets.
	TicketsOnly bool
	// RequesterPrincipalID keeps tickets this contact asked for.
	RequesterPrincipalID string
	// Channel keeps tickets that arrived this way.
	Channel model.TicketChannel
}

// IsEmpty reports whether the filter keeps every card.
func (f CardFilter) IsEmpty() bool { return f == CardFilter{} }

// cardFilterConditions renders a filter as SQL over a placements row aliased p.
// One place, so the page and its count can never disagree about what matches.
func cardFilterConditions(filter CardFilter) (string, []any) {
	conditions := ""
	args := []any{}
	if filter.CardID != "" {
		conditions += ` AND p.card_id=?`
		args = append(args, filter.CardID)
	}
	if filter.AssigneePrincipalID != "" {
		conditions += ` AND EXISTS (SELECT 1 FROM card_assignments a WHERE a.card_id=p.card_id AND a.principal_id=?)`
		args = append(args, filter.AssigneePrincipalID)
	}
	if filter.Unassigned {
		conditions += ` AND NOT EXISTS (SELECT 1 FROM card_assignments a WHERE a.card_id=p.card_id)`
	}
	if filter.TicketsOnly || filter.RequesterPrincipalID != "" || filter.Channel != "" {
		conditions += ` AND EXISTS (SELECT 1 FROM tickets t WHERE t.card_id=p.card_id`
		if filter.RequesterPrincipalID != "" {
			conditions += ` AND t.requester_principal_id=?`
			args = append(args, filter.RequesterPrincipalID)
		}
		if filter.Channel != "" {
			conditions += ` AND t.channel=?`
			args = append(args, string(filter.Channel))
		}
		conditions += `)`
	}
	return conditions, args
}

// CountColumnCardsMatching is how many of a column's cards a filter keeps.
func (s *Store) CountColumnCardsMatching(columnID string, filter CardFilter) (int, error) {
	conditions, filterArgs := cardFilterConditions(filter)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM placements p WHERE p.column_id=?`+conditions,
		append([]any{columnID}, filterArgs...)...).Scan(&n)
	return n, err
}

// ListPlacementsByColumnMatching is ListPlacementsByColumn over the cards a
// filter keeps, paged after the filter so a page is never short for no reason.
func (s *Store) ListPlacementsByColumnMatching(columnID string, filter CardFilter, limit, offset int) ([]*model.Placement, error) {
	conditions, filterArgs := cardFilterConditions(filter)
	q := `SELECT p.card_id, p.board_id, p.column_id, p.position, p.created_at, p.updated_at
	      FROM placements p WHERE p.column_id=?` + conditions + ` ORDER BY p.position ASC, p.created_at ASC`
	args := append([]any{columnID}, filterArgs...)
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Placement{}
	for rows.Next() {
		p := &model.Placement{}
		if err := rows.Scan(&p.CardID, &p.BoardID, &p.ColumnID, &p.Position, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListCardLinksForCards fetches the links of many cards in one query, grouped by
// card. The board view used to ask per card, which is 6,466 queries on this
// host's largest board.
func (s *Store) ListCardLinksForCards(cardIDs []string) (map[string][]model.CardLink, error) {
	out := map[string][]model.CardLink{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	q := `SELECT id, card_id, entity_type, entity_ref, label, created_at FROM card_links
	      WHERE card_id IN (` + placeholders(len(cardIDs)) + `) ORDER BY created_at ASC`
	rows, err := s.db.Query(q, anySlice(cardIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l model.CardLink
		if err := rows.Scan(&l.ID, &l.CardID, &l.EntityType, &l.EntityRef, &l.Label, &l.CreatedAt); err != nil {
			return nil, err
		}
		out[l.CardID] = append(out[l.CardID], l)
	}
	return out, rows.Err()
}

// ListCardAssignmentsForCards fetches the assignments of many cards in one
// query, grouped by card and oldest first within each — the same shape as
// ListCardLinksForCards, for the same reason: a board view asks once, not once
// per card.
func (s *Store) ListCardAssignmentsForCards(cardIDs []string) (map[string][]model.CardAssignment, error) {
	out := map[string][]model.CardAssignment{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	q := `SELECT ` + cardAssignmentColumns + ` FROM card_assignments
	      WHERE card_id IN (` + placeholders(len(cardIDs)) + `) ORDER BY created_at ASC, principal_id ASC`
	rows, err := s.db.Query(q, anySlice(cardIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanCardAssignment(rows)
		if err != nil {
			return nil, err
		}
		out[a.CardID] = append(out[a.CardID], a)
	}
	return out, rows.Err()
}

// ListCardEventsForCards fetches the events of many cards in one query, scoped to
// a board the way ListCardEvents is: that board's events plus the card-wide ones.
func (s *Store) ListCardEventsForCards(cardIDs []string, boardID string) (map[string][]model.CardEvent, error) {
	out := map[string][]model.CardEvent{}
	if len(cardIDs) == 0 {
		return out, nil
	}
	q := `SELECT ` + cardEventColumns + ` FROM card_events
	      WHERE card_id IN (` + placeholders(len(cardIDs)) + `)`
	args := anySlice(cardIDs)
	if boardID != "" {
		q += ` AND (board_id = ? OR board_id = '')`
		args = append(args, boardID)
	}
	q += ` ORDER BY occurred_at ASC, recorded_at ASC, id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanCardEvent(rows)
		if err != nil {
			return nil, err
		}
		out[e.CardID] = append(out[e.CardID], e)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
