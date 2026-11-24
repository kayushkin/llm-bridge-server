package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/llm-bridge/msg"
	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

var ErrBoardChanged = errors.New("board changed; reload before saving")

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateActivity(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateMessaging(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateBoardTagRules(db); err != nil {
		return nil, err
	}
	if err := migrateCardAttachments(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateBoardClassification(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateWorkStates(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS boards (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT DEFAULT '',
			archived    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_boards_archived ON boards(archived);

		CREATE TABLE IF NOT EXISTS columns (
			id          TEXT PRIMARY KEY,
			board_id    TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			name        TEXT NOT NULL,
			position    REAL NOT NULL,
			color       TEXT DEFAULT '',
			wip_limit   INTEGER,
			auto_status TEXT,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_columns_board ON columns(board_id, position);

		CREATE TABLE IF NOT EXISTS placements (
			card_id     TEXT NOT NULL,
			board_id    TEXT NOT NULL REFERENCES boards(id)  ON DELETE CASCADE,
			column_id   TEXT NOT NULL REFERENCES columns(id) ON DELETE CASCADE,
			position    REAL NOT NULL,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL,
			PRIMARY KEY (card_id, board_id)
		);
		CREATE INDEX IF NOT EXISTS idx_placements_board   ON placements(board_id, column_id, position);
		CREATE INDEX IF NOT EXISTS idx_placements_column  ON placements(column_id, position);
		CREATE INDEX IF NOT EXISTS idx_placements_card    ON placements(card_id);

		CREATE TABLE IF NOT EXISTS card_links (
			id          TEXT PRIMARY KEY,
			card_id     TEXT NOT NULL,
			entity_type TEXT NOT NULL,
			entity_ref  TEXT NOT NULL,
			label       TEXT DEFAULT '',
			created_at  DATETIME NOT NULL,
			UNIQUE (card_id, entity_type, entity_ref)
		);
		CREATE INDEX IF NOT EXISTS idx_card_links_card   ON card_links(card_id);
		CREATE INDEX IF NOT EXISTS idx_card_links_entity ON card_links(entity_type, entity_ref);

		CREATE TABLE IF NOT EXISTS card_assignments (
			card_id      TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			assigned_by  TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL,
			PRIMARY KEY (card_id, principal_id)
		);
		CREATE INDEX IF NOT EXISTS idx_card_assignments_principal ON card_assignments(principal_id);

		CREATE TABLE IF NOT EXISTS entity_tags (
			entity_type TEXT NOT NULL,
			entity_ref  TEXT NOT NULL,
			tag         TEXT NOT NULL,
			created_at  DATETIME NOT NULL,
			PRIMARY KEY (entity_type, entity_ref, tag)
		);
		CREATE INDEX IF NOT EXISTS idx_entity_tags_tag    ON entity_tags(tag);
		CREATE INDEX IF NOT EXISTS idx_entity_tags_entity ON entity_tags(entity_type, entity_ref);
	`)
	if err != nil {
		return err
	}
	return ensureTicketTables(db)
}

func now() time.Time { return time.Now().UTC() }

// ============================ Boards ============================

func (s *Store) CreateBoard(req *model.CreateBoardRequest) (*model.Board, error) {
	b := &model.Board{
		ID:        uuid.NewString(),
		Name:      req.Name,
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	if req.Description != nil {
		b.Description = *req.Description
	}
	_, err := s.db.Exec(
		`INSERT INTO boards (id, name, description, archived, created_at, updated_at) VALUES (?, ?, ?, 0, ?, ?)`,
		b.ID, b.Name, b.Description, b.CreatedAt, b.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Store) ListBoards(includeArchived bool) ([]*model.Board, error) {
	q := `SELECT ` + boardColumns + ` FROM boards`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBoard(id string) (*model.Board, error) {
	row := s.db.QueryRow(
		`SELECT `+boardColumns+` FROM boards WHERE id = ?`, id,
	)
	b, err := scanBoard(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// scanBoard reads a board row, decoding the business-hours JSON blob. A board
// that has never been given hours keeps a nil BusinessHours, which is what makes
// the business-hours figures absent rather than zero further down.
// boardColumns is the one spelling of the boards row every read shares.
// The taxonomy is the board's newest revision, which only
// SetBoardTaxonomy writes; see classification.go.
const boardColumns = `id, name, description, archived, business_hours,
	default_principal_id, default_agent_id, default_instance_id, default_bundle_id, classifier, assignment_pool, organization_id,
	(SELECT taxonomy FROM classification_taxonomy_revisions WHERE board_id = boards.id ORDER BY revision DESC LIMIT 1),
	(SELECT COALESCE(MAX(revision), 0) FROM classification_taxonomy_revisions WHERE board_id = boards.id),
	created_at, updated_at`

func scanBoard(r scanner) (*model.Board, error) {
	b := &model.Board{}
	var arch int
	var hours, defaultPrincipal, defaultAgent, defaultInstance, defaultBundle, classifier, assignmentPool, organization, taxonomy sql.NullString
	if err := r.Scan(&b.ID, &b.Name, &b.Description, &arch, &hours,
		&defaultPrincipal, &defaultAgent, &defaultInstance, &defaultBundle, &classifier, &assignmentPool, &organization,
		&taxonomy, &b.TaxonomyRevision, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.Archived = arch != 0
	if hours.Valid && strings.TrimSpace(hours.String) != "" {
		var bh model.BusinessHours
		if err := json.Unmarshal([]byte(hours.String), &bh); err != nil {
			return nil, fmt.Errorf("board %s has unreadable business_hours: %w", b.ID, err)
		}
		b.BusinessHours = &bh
	}
	b.DefaultPrincipalID = defaultPrincipal.String
	b.DefaultAgentID = defaultAgent.String
	b.DefaultInstanceID = defaultInstance.String
	b.DefaultBundleID = defaultBundle.String
	b.OrganizationID = organization.String
	if classifier.Valid && strings.TrimSpace(classifier.String) != "" {
		var cc model.ClassifierConfig
		if err := json.Unmarshal([]byte(classifier.String), &cc); err != nil {
			return nil, fmt.Errorf("board %s has unreadable classifier: %w", b.ID, err)
		}
		b.Classifier = &cc
	}
	if assignmentPool.Valid && strings.TrimSpace(assignmentPool.String) != "" {
		var pool model.AssignmentPool
		if err := json.Unmarshal([]byte(assignmentPool.String), &pool); err != nil {
			return nil, fmt.Errorf("board %s has unreadable assignment_pool: %w", b.ID, err)
		}
		b.AssignmentPool = &pool
	}
	if taxonomy.Valid && strings.TrimSpace(taxonomy.String) != "" {
		var classificationTaxonomy msg.ClassificationTaxonomy
		if err := json.Unmarshal([]byte(taxonomy.String), &classificationTaxonomy); err != nil {
			return nil, fmt.Errorf("board %s has unreadable taxonomy: %w", b.ID, err)
		}
		b.Taxonomy = &classificationTaxonomy
	}
	return b, nil
}

// nullableText stores "" as NULL so a cleared setting reads back as absent
// rather than as an empty id.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullableJSON encodes a settings object, or NULL when there is none.
func nullableJSON(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

func (s *Store) UpdateBoard(id string, req *model.UpdateBoardRequest) (*model.Board, error) {
	return s.UpdateBoardIfVersion(id, req, nil)
}

// UpdateBoardIfVersion compares the caller's version before editing and the
// version read at the database write. Even unconditional callers cannot replace
// fields copied from a row another writer has changed in the meantime.
func (s *Store) UpdateBoardIfVersion(id string, req *model.UpdateBoardRequest, expected *time.Time) (*model.Board, error) {
	b, err := s.GetBoard(id)
	if err != nil {
		return nil, err
	}
	previousVersion := b.UpdatedAt
	if expected != nil && !expected.Equal(previousVersion) {
		return nil, ErrBoardChanged
	}
	if req.Name != nil {
		b.Name = *req.Name
	}
	if req.Description != nil {
		b.Description = *req.Description
	}
	if req.Archived != nil {
		b.Archived = *req.Archived
	}
	if req.BusinessHours != nil {
		// A present-but-empty object clears the hours; anything else replaces them.
		// The API validates the value before it reaches here.
		if req.BusinessHours.TZID == "" {
			b.BusinessHours = nil
		} else {
			replacement := *req.BusinessHours
			// Holidays absent from the request are kept, not cleared; a list that
			// is present, even empty, replaces them. See BusinessHours.Holidays.
			if replacement.Holidays == nil && b.BusinessHours != nil {
				replacement.Holidays = b.BusinessHours.Holidays
			}
			replacement.Holidays = append([]string(nil), replacement.Holidays...)
			sort.Strings(replacement.Holidays)
			if len(replacement.Holidays) == 0 {
				replacement.Holidays = nil
			}
			b.BusinessHours = &replacement
		}
	}
	if req.DefaultPrincipalID != nil {
		b.DefaultPrincipalID = *req.DefaultPrincipalID
	}
	if req.DefaultAgentID != nil {
		b.DefaultAgentID = *req.DefaultAgentID
	}
	if req.DefaultInstanceID != nil {
		b.DefaultInstanceID = *req.DefaultInstanceID
	}
	if req.DefaultBundleID != nil {
		b.DefaultBundleID = *req.DefaultBundleID
	}
	if req.Classifier != nil {
		if req.Classifier.Cleared() {
			b.Classifier = nil
		} else {
			b.Classifier = req.Classifier
		}
	}
	if req.AssignmentPool != nil {
		if req.AssignmentPool.Cleared() {
			b.AssignmentPool = nil
		} else {
			b.AssignmentPool = req.AssignmentPool
		}
	}
	if req.OrganizationID != nil {
		b.OrganizationID = *req.OrganizationID
	}
	b.UpdatedAt = now()
	if !b.UpdatedAt.After(previousVersion) {
		b.UpdatedAt = previousVersion.Add(time.Nanosecond)
	}
	arch := 0
	if b.Archived {
		arch = 1
	}
	var hours any
	if b.BusinessHours != nil {
		encoded, err := json.Marshal(b.BusinessHours)
		if err != nil {
			return nil, err
		}
		hours = string(encoded)
	}
	var classifier any
	if b.Classifier != nil {
		encoded, err := nullableJSON(b.Classifier)
		if err != nil {
			return nil, err
		}
		classifier = encoded
	}
	var assignmentPool any
	if b.AssignmentPool != nil {
		encoded, err := nullableJSON(b.AssignmentPool)
		if err != nil {
			return nil, err
		}
		assignmentPool = encoded
	}
	result, err := s.db.Exec(
		`UPDATE boards SET name=?, description=?, archived=?, business_hours=?,
         default_principal_id=?, default_agent_id=?, default_instance_id=?, default_bundle_id=?, classifier=?, assignment_pool=?, organization_id=?, updated_at=? WHERE id=? AND updated_at=?`,
		b.Name, b.Description, arch, hours,
		nullableText(b.DefaultPrincipalID), nullableText(b.DefaultAgentID), nullableText(b.DefaultInstanceID), nullableText(b.DefaultBundleID), classifier, assignmentPool, nullableText(b.OrganizationID),
		b.UpdatedAt, id, previousVersion,
	)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrBoardChanged
	}
	return b, nil
}

func (s *Store) DeleteBoard(id string) error {
	res, err := s.db.Exec(`DELETE FROM boards WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ Columns ============================

func (s *Store) CreateColumn(boardID string, req *model.CreateColumnRequest) (*model.Column, error) {
	if _, err := s.GetBoard(boardID); err != nil {
		return nil, err
	}
	pos := 0.0
	if req.Position != nil {
		pos = *req.Position
	} else {
		// Append: max(position) + 1
		var maxPos sql.NullFloat64
		s.db.QueryRow(`SELECT MAX(position) FROM columns WHERE board_id = ?`, boardID).Scan(&maxPos)
		if maxPos.Valid {
			pos = maxPos.Float64 + 1.0
		}
	}
	c := &model.Column{
		ID:               uuid.NewString(),
		BoardID:          boardID,
		Name:             req.Name,
		Position:         pos,
		WIPLimit:         req.WIPLimit,
		AutoStatus:       req.AutoStatus,
		BudgetClockState: req.BudgetClockState,
		LifecycleState:   lifecycleStateFromRequest(req.LifecycleState),
		WorkState:        workStateFromRequest(req.WorkState),
		CreatedAt:        now(),
		UpdatedAt:        now(),
	}
	if req.Color != nil {
		c.Color = *req.Color
	}
	_, err := s.db.Exec(
		`INSERT INTO columns (id, board_id, name, position, color, wip_limit, auto_status, budget_clock_state, lifecycle_state, work_state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.BoardID, c.Name, c.Position, c.Color, c.WIPLimit, c.AutoStatus, clockStateValue(c.BudgetClockState),
		lifecycleStateValue(c.LifecycleState), workStateValue(c.WorkState), c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) ListColumns(boardID string) ([]*model.Column, error) {
	rows, err := s.db.Query(
		`SELECT id, board_id, name, position, color, wip_limit, auto_status, budget_clock_state, lifecycle_state, work_state, created_at, updated_at
		 FROM columns WHERE board_id = ? ORDER BY position ASC`, boardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Column
	for rows.Next() {
		c, err := scanColumn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetColumn(id string) (*model.Column, error) {
	row := s.db.QueryRow(
		`SELECT id, board_id, name, position, color, wip_limit, auto_status, budget_clock_state, lifecycle_state, work_state, created_at, updated_at
		 FROM columns WHERE id = ?`, id,
	)
	c, err := scanColumn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanColumn(r scanner) (*model.Column, error) {
	c := &model.Column{}
	var wip sql.NullInt64
	var auto, clock, lifecycle, work sql.NullString
	if err := r.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &c.Color, &wip, &auto, &clock, &lifecycle, &work, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if work.Valid && work.String != "" {
		state := model.WorkState(work.String)
		c.WorkState = &state
	}
	if lifecycle.Valid && lifecycle.String != "" {
		state := model.TicketLifecycleState(lifecycle.String)
		c.LifecycleState = &state
	}
	if clock.Valid && clock.String != "" {
		v := model.ClockState(clock.String)
		c.BudgetClockState = &v
	}
	if wip.Valid {
		v := int(wip.Int64)
		c.WIPLimit = &v
	}
	if auto.Valid {
		v := auto.String
		c.AutoStatus = &v
	}
	return c, nil
}

func (s *Store) UpdateColumn(id string, req *model.UpdateColumnRequest) (*model.Column, error) {
	c, err := s.GetColumn(id)
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.Position != nil {
		c.Position = *req.Position
	}
	if req.Color != nil {
		c.Color = *req.Color
	}
	if req.WIPLimit != nil {
		c.WIPLimit = req.WIPLimit
	}
	if req.AutoStatus != nil {
		c.AutoStatus = req.AutoStatus
	}
	if req.BudgetClockState != nil {
		// An empty string clears the setting, which puts the column back to leaving
		// the clock exactly where it found it.
		if *req.BudgetClockState == "" {
			c.BudgetClockState = nil
		} else {
			c.BudgetClockState = req.BudgetClockState
		}
	}
	if req.LifecycleState != nil {
		// An empty string clears it, and every ticket in the column then
		// reports no lifecycle rather than a stale one.
		c.LifecycleState = lifecycleStateFromRequest(req.LifecycleState)
	}
	if req.WorkState != nil {
		// An empty string clears it, and a card whose latest move was into
		// this column then reports no work_state rather than a stale one.
		c.WorkState = workStateFromRequest(req.WorkState)
	}
	c.UpdatedAt = now()
	_, err = s.db.Exec(
		`UPDATE columns SET name=?, position=?, color=?, wip_limit=?, auto_status=?, budget_clock_state=?, lifecycle_state=?, work_state=?, updated_at=? WHERE id=?`,
		c.Name, c.Position, c.Color, c.WIPLimit, c.AutoStatus, clockStateValue(c.BudgetClockState),
		lifecycleStateValue(c.LifecycleState), workStateValue(c.WorkState), c.UpdatedAt, id,
	)
	return c, err
}

func (s *Store) DeleteColumn(id string) error {
	res, err := s.db.Exec(`DELETE FROM columns WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ Placements ============================

// AttachCard inserts (card_id, board_id) -> (column_id, position). Idempotent
// per (card_id, board_id) — re-attaching the same card to the same board
// updates the placement (acts like a move).
func (s *Store) AttachCard(boardID, cardID string, req *model.AttachCardRequest) (*model.Placement, error) {
	col, err := s.GetColumn(req.ColumnID)
	if err != nil {
		return nil, err
	}
	if col.BoardID != boardID {
		return nil, fmt.Errorf("column %s does not belong to board %s", req.ColumnID, boardID)
	}
	pos := 0.0
	if req.Position != nil {
		pos = *req.Position
	} else {
		var maxPos sql.NullFloat64
		s.db.QueryRow(`SELECT MAX(position) FROM placements WHERE board_id=? AND column_id=?`, boardID, req.ColumnID).Scan(&maxPos)
		if maxPos.Valid {
			pos = maxPos.Float64 + 1.0
		}
	}
	p := &model.Placement{
		CardID:    cardID,
		BoardID:   boardID,
		ColumnID:  req.ColumnID,
		Position:  pos,
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	_, err = s.db.Exec(`
		INSERT INTO placements (card_id, board_id, column_id, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(card_id, board_id) DO UPDATE SET
			column_id  = excluded.column_id,
			position   = excluded.position,
			updated_at = excluded.updated_at
	`, p.CardID, p.BoardID, p.ColumnID, p.Position, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) MoveCard(cardID string, req *model.MoveCardRequest) (*model.Placement, error) {
	col, err := s.GetColumn(req.ColumnID)
	if err != nil {
		return nil, err
	}
	if col.BoardID != req.BoardID {
		return nil, fmt.Errorf("column %s does not belong to board %s", req.ColumnID, req.BoardID)
	}
	res, err := s.db.Exec(`
		UPDATE placements SET column_id=?, position=?, updated_at=?
		WHERE card_id=? AND board_id=?`,
		req.ColumnID, req.Position, now(), cardID, req.BoardID,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetPlacement(cardID, req.BoardID)
}

func (s *Store) GetPlacement(cardID, boardID string) (*model.Placement, error) {
	p := &model.Placement{}
	err := s.db.QueryRow(
		`SELECT card_id, board_id, column_id, position, created_at, updated_at
		 FROM placements WHERE card_id=? AND board_id=?`, cardID, boardID,
	).Scan(&p.CardID, &p.BoardID, &p.ColumnID, &p.Position, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) ListPlacementsByBoard(boardID string) ([]*model.Placement, error) {
	rows, err := s.db.Query(
		`SELECT card_id, board_id, column_id, position, created_at, updated_at
		 FROM placements WHERE board_id=? ORDER BY column_id, position ASC`, boardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Placement
	for rows.Next() {
		p := &model.Placement{}
		if err := rows.Scan(&p.CardID, &p.BoardID, &p.ColumnID, &p.Position, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListPlacementsByCard(cardID string) ([]*model.Placement, error) {
	rows, err := s.db.Query(
		`SELECT card_id, board_id, column_id, position, created_at, updated_at
		 FROM placements WHERE card_id=?`, cardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Placement
	for rows.Next() {
		p := &model.Placement{}
		if err := rows.Scan(&p.CardID, &p.BoardID, &p.ColumnID, &p.Position, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DetachCard removes a single (card_id, board_id) placement.
func (s *Store) DetachCard(boardID, cardID string) error {
	res, err := s.db.Exec(`DELETE FROM placements WHERE card_id=? AND board_id=?`, cardID, boardID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DetachCardEverywhere removes all placements for a card across all boards.
// Used when the underlying noteboard item is hard-deleted.
func (s *Store) DetachCardEverywhere(cardID string) error {
	_, err := s.db.Exec(`DELETE FROM placements WHERE card_id=?`, cardID)
	return err
}

// CountColumnCards returns how many cards currently sit in a column. Used to
// enforce WIP limits on the API edge.
func (s *Store) CountColumnCards(columnID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM placements WHERE column_id=?`, columnID).Scan(&n)
	return n, err
}

// ============================ Card links ============================

func (s *Store) CreateCardLink(cardID string, req *model.CreateCardLinkRequest) (*model.CardLink, error) {
	l := &model.CardLink{
		ID:         uuid.NewString(),
		CardID:     cardID,
		EntityType: req.EntityType,
		EntityRef:  req.EntityRef,
		CreatedAt:  now(),
	}
	if req.Label != nil {
		l.Label = *req.Label
	}
	_, err := s.db.Exec(
		`INSERT INTO card_links (id, card_id, entity_type, entity_ref, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(card_id, entity_type, entity_ref) DO UPDATE SET label=excluded.label`,
		l.ID, l.CardID, l.EntityType, l.EntityRef, l.Label, l.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) ListCardLinks(cardID string) ([]model.CardLink, error) {
	rows, err := s.db.Query(
		`SELECT id, card_id, entity_type, entity_ref, label, created_at
		 FROM card_links WHERE card_id=? ORDER BY created_at ASC`, cardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CardLink
	for rows.Next() {
		var l model.CardLink
		if err := rows.Scan(&l.ID, &l.CardID, &l.EntityType, &l.EntityRef, &l.Label, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCardLink(linkID string) error {
	res, err := s.db.Exec(`DELETE FROM card_links WHERE id=?`, linkID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ Card assignments ============================

const cardAssignmentColumns = `card_id, principal_id, assigned_by, created_at`

func scanCardAssignment(r scanner) (model.CardAssignment, error) {
	var a model.CardAssignment
	err := r.Scan(&a.CardID, &a.PrincipalID, &a.AssignedBy, &a.CreatedAt)
	return a, err
}

// AssignPrincipalToCard records that a principal is on a card. It is idempotent
// on (card_id, principal_id): created reports whether THIS call wrote the row,
// so the API can answer 201 for a new assignment and 200 for one already there.
// A repeat does not touch assigned_by or created_at — who put them on the card
// first, and when, is the fact worth keeping.
func (s *Store) AssignPrincipalToCard(cardID, principalID, assignedBy string) (assignment *model.CardAssignment, created bool, err error) {
	candidate := &model.CardAssignment{
		CardID: cardID, PrincipalID: principalID, AssignedBy: assignedBy, CreatedAt: now(),
	}
	res, err := s.db.Exec(
		`INSERT INTO card_assignments (`+cardAssignmentColumns+`) VALUES (?, ?, ?, ?)
		 ON CONFLICT(card_id, principal_id) DO NOTHING`,
		candidate.CardID, candidate.PrincipalID, candidate.AssignedBy, candidate.CreatedAt,
	)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 1 {
		return candidate, true, nil
	}
	existing, err := s.GetCardAssignment(cardID, principalID)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (s *Store) GetCardAssignment(cardID, principalID string) (*model.CardAssignment, error) {
	a, err := scanCardAssignment(s.db.QueryRow(
		`SELECT `+cardAssignmentColumns+` FROM card_assignments WHERE card_id=? AND principal_id=?`,
		cardID, principalID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListCardAssignments returns who is on a card, oldest assignment first. Ties
// break on principal_id so the order is total.
func (s *Store) ListCardAssignments(cardID string) ([]model.CardAssignment, error) {
	return s.queryCardAssignments(
		`SELECT `+cardAssignmentColumns+` FROM card_assignments WHERE card_id=?
		 ORDER BY created_at ASC, principal_id ASC`, cardID,
	)
}

// ListCardAssignmentsByPrincipal is the reverse lookup: every card a principal
// is on, across every board, oldest assignment first.
func (s *Store) ListCardAssignmentsByPrincipal(principalID string) ([]model.CardAssignment, error) {
	return s.queryCardAssignments(
		`SELECT `+cardAssignmentColumns+` FROM card_assignments WHERE principal_id=?
		 ORDER BY created_at ASC, card_id ASC`, principalID,
	)
}

// AssignmentOnBoard is one assignment on a card placed on a board, with
// whether the card's column stops the budget clock there — the column's own
// declaration that work in it is finished.
type AssignmentOnBoard struct {
	model.CardAssignment
	ColumnStopsTheClock bool
}

// ListAssignmentsOnBoard returns every assignment on every card placed on the
// board, for the assignment pool's strategies to weigh the members by.
func (s *Store) ListAssignmentsOnBoard(boardID string) ([]AssignmentOnBoard, error) {
	rows, err := s.db.Query(
		`SELECT ca.card_id, ca.principal_id, ca.assigned_by, ca.created_at, COALESCE(c.budget_clock_state, '')
		 FROM card_assignments ca
		 JOIN placements p ON p.card_id = ca.card_id AND p.board_id = ?
		 JOIN columns c ON c.id = p.column_id
		 ORDER BY ca.created_at ASC, ca.principal_id ASC`, boardID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssignmentOnBoard{}
	for rows.Next() {
		var row AssignmentOnBoard
		var clockState string
		if err := rows.Scan(&row.CardID, &row.PrincipalID, &row.AssignedBy, &row.CreatedAt, &clockState); err != nil {
			return nil, err
		}
		row.ColumnStopsTheClock = model.ClockState(clockState) == model.ClockStopped
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) queryCardAssignments(q string, args ...any) ([]model.CardAssignment, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CardAssignment{}
	for rows.Next() {
		a, err := scanCardAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UnassignPrincipalFromCard removes one assignment; ErrNotFound if it was not
// there, so a DELETE of a principal who was never on the card is a 404 rather
// than a 204 that claims to have changed something.
func (s *Store) UnassignPrincipalFromCard(cardID, principalID string) error {
	res, err := s.db.Exec(`DELETE FROM card_assignments WHERE card_id=? AND principal_id=?`, cardID, principalID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ Entity tags ============================

func (s *Store) AddEntityTag(entityType, entityRef, tag string) (*model.EntityTag, error) {
	t := &model.EntityTag{
		EntityType: entityType,
		EntityRef:  entityRef,
		Tag:        tag,
		CreatedAt:  now(),
	}
	_, err := s.db.Exec(
		`INSERT INTO entity_tags (entity_type, entity_ref, tag, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(entity_type, entity_ref, tag) DO NOTHING`,
		t.EntityType, t.EntityRef, t.Tag, t.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) ListEntityTags(entityType, entityRef string) ([]model.EntityTag, error) {
	rows, err := s.db.Query(
		`SELECT entity_type, entity_ref, tag, created_at FROM entity_tags
		 WHERE entity_type=? AND entity_ref=? ORDER BY tag ASC`,
		entityType, entityRef,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.EntityTag
	for rows.Next() {
		var t model.EntityTag
		if err := rows.Scan(&t.EntityType, &t.EntityRef, &t.Tag, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteEntityTag(entityType, entityRef, tag string) error {
	res, err := s.db.Exec(
		`DELETE FROM entity_tags WHERE entity_type=? AND entity_ref=? AND tag=?`,
		entityType, entityRef, tag,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FindEntitiesByTag returns (entity_type, entity_ref) pairs that carry a tag.
// If entityType is empty, searches across all types.
func (s *Store) FindEntitiesByTag(tag, entityType string) ([]model.EntityTag, error) {
	q := `SELECT entity_type, entity_ref, tag, created_at FROM entity_tags WHERE tag = ?`
	args := []any{tag}
	if entityType != "" {
		q += ` AND entity_type = ?`
		args = append(args, entityType)
	}
	q += ` ORDER BY entity_type, entity_ref`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.EntityTag
	for rows.Next() {
		var t model.EntityTag
		if err := rows.Scan(&t.EntityType, &t.EntityRef, &t.Tag, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAllEntityTags returns the set of unique tags in use across all entities,
// with a count of how many (entity_type, entity_ref) pairs carry each tag.
func (s *Store) ListAllEntityTags() ([]model.TagCount, error) {
	rows, err := s.db.Query(
		`SELECT tag, COUNT(*) FROM entity_tags GROUP BY tag ORDER BY tag ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TagCount
	for rows.Next() {
		var t model.TagCount
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ============================ Health ============================

func (s *Store) Counts() (map[string]int, error) {
	tables := []string{"boards", "columns", "placements", "card_links", "card_assignments", "entity_tags"}
	out := map[string]int{}
	for _, t := range tables {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// utility: stable ordering helper used for Position fan-out elsewhere
var _ = strings.Contains
