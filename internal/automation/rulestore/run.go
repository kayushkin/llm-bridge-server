package rulestore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Run causes: what made rule-store look at the subject.
const (
	CauseFeed  = "feed"
	CauseSweep = "sweep"
)

// Run outcomes.
const (
	// OutcomeShadowed is a shadow match: effects recorded, not called.
	OutcomeShadowed = "shadowed"
	// OutcomeApplying is a live match whose calls are being made. It is
	// written before the first call, so the run's uniqueness stops a second
	// decision from calling again.
	OutcomeApplying = "applying"
	// OutcomeApplied is a live match whose every call the owner accepted.
	OutcomeApplied = "applied"
	// OutcomeFailed is a live match an owner refused, or that got no answer.
	// Calls after the failed one were not made. Nothing retries it.
	OutcomeFailed = "failed"
	// OutcomeInterrupted is a live match that was applying when rule-store
	// stopped. Some calls may have been made; the run says which answered.
	OutcomeInterrupted = "interrupted"
)

// Outcomes is every outcome a run can have.
var Outcomes = []string{OutcomeShadowed, OutcomeApplying, OutcomeApplied, OutcomeFailed, OutcomeInterrupted}

// FinishRun records a live run's outcome and the owners' answers.
func (s *Store) FinishRun(runID, outcome string, effects []PlannedEffect) error {
	planned, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE rule_runs SET outcome = ?, planned_effects = ? WHERE id = ? AND outcome = ?`,
		outcome, string(planned), runID, OutcomeApplying)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("%w: run %s is not applying", ErrConflict, runID)
	}
	return nil
}

// MarkInterruptedRuns marks every run left applying by a stopped process.
func (s *Store) MarkInterruptedRuns() (int64, error) {
	result, err := s.db.Exec(`UPDATE rule_runs SET outcome = ? WHERE outcome = ?`, OutcomeInterrupted, OutcomeApplying)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Run is one rule revision's decision about one subject in one state episode.
type Run struct {
	ID             string          `json:"id"`
	RuleID         string          `json:"rule_id"`
	RuleRevisionID string          `json:"rule_revision_id"`
	SubjectType    string          `json:"subject_type"`
	SubjectID      string          `json:"subject_id"`
	TriggerKey     string          `json:"trigger_key"`
	Mode           string          `json:"mode"`
	Cause          string          `json:"cause"`
	Facts          json.RawMessage `json:"facts"`
	PlannedEffects []PlannedEffect `json:"planned_effects"`
	Outcome        string          `json:"outcome"`
	DecidedAt      int64           `json:"decided_at"`
}

// HasRun says whether a revision already decided about this subject in this
// state episode, so the reactor can skip the kanban-store reads.
func (s *Store) HasRun(revisionID, subjectType, subjectID, triggerKey string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM rule_runs WHERE rule_revision_id = ? AND subject_type = ? AND subject_id = ? AND trigger_key = ?`,
		revisionID, subjectType, subjectID, triggerKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RecordRun writes a decision. A second decision for the same revision,
// subject and trigger key is ErrConflict: the feed and the sweep can race to
// the same match, and only one of them may stand.
func (s *Store) RecordRun(run *Run) error {
	planned, err := json.Marshal(run.PlannedEffects)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "rule_runs")
	if err != nil {
		return err
	}
	run.ID = formatID("rule_run", seq)
	_, err = tx.Exec(`INSERT INTO rule_runs (id, seq, rule_id, rule_revision_id, subject_type, subject_id, trigger_key, mode, cause, facts, planned_effects, outcome, decided_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, seq, run.RuleID, run.RuleRevisionID, run.SubjectType, run.SubjectID, run.TriggerKey, run.Mode, run.Cause,
		string(run.Facts), string(planned), run.Outcome, run.DecidedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: rule_runs.rule_revision_id") {
			return fmt.Errorf("%w: %s already decided about %s %s at %s", ErrConflict, run.RuleRevisionID, run.SubjectType, run.SubjectID, run.TriggerKey)
		}
		return err
	}
	return tx.Commit()
}

// RunFilter selects runs. Limit 0 means 100.
type RunFilter struct {
	RuleID    string
	SubjectID string
	Since     int64
	Limit     int
}

// ListRuns returns runs newest first.
func (s *Store) ListRuns(f RunFilter) ([]Run, error) {
	var where []string
	var args []any
	if f.RuleID != "" {
		where, args = append(where, "rule_id = ?"), append(args, f.RuleID)
	}
	if f.SubjectID != "" {
		where, args = append(where, "subject_id = ?"), append(args, f.SubjectID)
	}
	if f.Since > 0 {
		where, args = append(where, "decided_at >= ?"), append(args, f.Since)
	}
	query := `SELECT id, rule_id, rule_revision_id, subject_type, subject_id, trigger_key, mode, cause, facts, planned_effects, outcome, decided_at FROM rule_runs`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(query+fmt.Sprintf(" ORDER BY seq DESC LIMIT %d", limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var run Run
		var facts, planned string
		if err := rows.Scan(&run.ID, &run.RuleID, &run.RuleRevisionID, &run.SubjectType, &run.SubjectID, &run.TriggerKey,
			&run.Mode, &run.Cause, &facts, &planned, &run.Outcome, &run.DecidedAt); err != nil {
			return nil, err
		}
		run.Facts = json.RawMessage(facts)
		if err := json.Unmarshal([]byte(planned), &run.PlannedEffects); err != nil {
			return nil, fmt.Errorf("run %s holds unreadable planned effects: %w", run.ID, err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// RuleRunCounts is how many runs a rule has, how many since a moment, and
// when the newest was decided.
type RuleRunCounts struct {
	Total         int
	Recent        int
	LastDecidedAt int64
}

// RunCountsByRule counts runs per rule in one query.
func (s *Store) RunCountsByRule(recentSince int64) (map[string]RuleRunCounts, error) {
	rows, err := s.db.Query(`SELECT rule_id, COUNT(*), SUM(CASE WHEN decided_at >= ? THEN 1 ELSE 0 END), MAX(decided_at) FROM rule_runs GROUP BY rule_id`, recentSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]RuleRunCounts{}
	for rows.Next() {
		var id string
		var c RuleRunCounts
		if err := rows.Scan(&id, &c.Total, &c.Recent, &c.LastDecidedAt); err != nil {
			return nil, err
		}
		counts[id] = c
	}
	return counts, rows.Err()
}
