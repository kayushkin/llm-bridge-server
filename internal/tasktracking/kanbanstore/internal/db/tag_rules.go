package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/kanban-store/internal/model"
)

// ErrTagRuleNotOnBoard is a PUT naming a rule id that is not one of this
// board's rules — a caller mixing boards up, answered 400 rather than minting
// a fresh rule under the stale id.
var ErrTagRuleNotOnBoard = errors.New("tag rule id does not belong to this board")

func migrateBoardTagRules(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS board_tag_rules (
			id                   TEXT PRIMARY KEY,
			board_id             TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			position             INTEGER NOT NULL,
			tags                 TEXT NOT NULL,
			default_principal_id TEXT,
			default_agent_id     TEXT,
			default_instance_id  TEXT,
			default_bundle_id    TEXT,
			created_at           DATETIME NOT NULL,
			updated_at           DATETIME NOT NULL,
			UNIQUE (board_id, position)
		);
		CREATE INDEX IF NOT EXISTS idx_board_tag_rules_board ON board_tag_rules(board_id, position);
	`)
	return err
}

const boardTagRuleColumns = `id, board_id, position, tags, default_principal_id, default_agent_id, default_instance_id, default_bundle_id, created_at, updated_at`

// GetBoardTagRules is the board's rules in order, first rule first.
func (s *Store) GetBoardTagRules(boardID string) (*model.BoardTagRules, error) {
	if _, err := s.GetBoard(boardID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT `+boardTagRuleColumns+` FROM board_tag_rules WHERE board_id = ? ORDER BY position ASC`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &model.BoardTagRules{BoardID: boardID, Rules: []model.BoardTagRule{}}
	for rows.Next() {
		var rule model.BoardTagRule
		var tags string
		var principal, agent, instance, bundle sql.NullString
		if err := rows.Scan(&rule.ID, &rule.BoardID, &rule.Position, &tags,
			&principal, &agent, &instance, &bundle, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tags), &rule.Tags); err != nil {
			return nil, fmt.Errorf("board %s tag rule %s has unreadable tags: %w", boardID, rule.ID, err)
		}
		rule.DefaultPrincipalID, rule.DefaultAgentID = principal.String, agent.String
		rule.DefaultInstanceID, rule.DefaultBundleID = instance.String, bundle.String
		out.Rules = append(out.Rules, rule)
	}
	return out, rows.Err()
}

// SetBoardTagRules replaces the board's rules with inputs, in order, in one
// transaction. An input carrying the id of one of this board's rules keeps
// that id and its created_at; one without an id gets a new rule.
func (s *Store) SetBoardTagRules(boardID string, inputs []model.BoardTagRuleInput) (*model.BoardTagRules, error) {
	if _, err := s.GetBoard(boardID); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	createdAtByID := map[string]time.Time{}
	rows, err := tx.Query(`SELECT id, created_at FROM board_tag_rules WHERE board_id = ?`, boardID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var createdAt time.Time
		if err := rows.Scan(&id, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		createdAtByID[id] = createdAt
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, input := range inputs {
		if input.ID == "" {
			continue
		}
		if _, ok := createdAtByID[input.ID]; !ok {
			return nil, fmt.Errorf("%w: %s is not one of board %s's tag rules; omit id to create a new rule", ErrTagRuleNotOnBoard, input.ID, boardID)
		}
	}
	if _, err := tx.Exec(`DELETE FROM board_tag_rules WHERE board_id = ?`, boardID); err != nil {
		return nil, err
	}
	stamp := now()
	for position, input := range inputs {
		id, createdAt := input.ID, stamp
		if id == "" {
			id = uuid.NewString()
		} else {
			createdAt = createdAtByID[id]
		}
		tags, err := json.Marshal(input.Tags)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(
			`INSERT INTO board_tag_rules (`+boardTagRuleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, boardID, position, string(tags),
			nullableText(input.DefaultPrincipalID), nullableText(input.DefaultAgentID),
			nullableText(input.DefaultInstanceID), nullableText(input.DefaultBundleID),
			createdAt, stamp,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBoardTagRules(boardID)
}
