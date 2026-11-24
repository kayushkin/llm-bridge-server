package evalstore

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
)

// Run is a suite, the models that answer, the models that judge, the cases
// they answer, and the prices in force when it was made.
type Run struct {
	ID                    string                `json:"id"`
	SuiteID               string                `json:"suite_id"`
	CandidateModelIDs     []string              `json:"candidate_model_ids"`
	JudgeModelIDs         []string              `json:"judge_model_ids"`
	OriginalAnswerModelID string                `json:"original_answer_model_id"`
	CaseSelection         CaseSelection         `json:"case_selection"`
	CaseIDs               []string              `json:"case_ids"`
	Status                string                `json:"status"`
	StatusReason          string                `json:"status_reason"`
	BudgetLimits          []BudgetLimit         `json:"budget_limits"`
	Concurrency           int                   `json:"concurrency"`
	OrganizationID        string                `json:"organization_id"`
	ModelPrices           map[string]ModelPrice `json:"model_prices"`
	PricesNote            string                `json:"prices_note"`
	PredictionID          string                `json:"prediction_id"`
	Note                  string                `json:"note"`
	Origin                string                `json:"origin"`
	CreatedAt             int64                 `json:"created_at"`
	StartedAt             int64                 `json:"started_at"`
	FinishedAt            int64                 `json:"finished_at"`
	UpdatedAt             int64                 `json:"updated_at"`
}

// CaseSelection says which of the suite's cases a run answers.
type CaseSelection struct {
	// Mode is all or sample.
	Mode string `json:"mode"`
	Size int    `json:"size,omitempty"`
	Seed int64  `json:"seed,omitempty"`
}

// BudgetLimit stops a run once usage-store reports the window of the
// subscription above MaxUsedPercent.
type BudgetLimit struct {
	Subscription   string  `json:"subscription"`
	Window         string  `json:"window"`
	MaxUsedPercent float64 `json:"max_used_percent"`
}

// ModelPrice is a model as model-store described it when the run was made.
// Known is false when model-store gave no price; a report then says so
// rather than guessing.
type ModelPrice struct {
	Name                 string  `json:"name"`
	Provider             string  `json:"provider"`
	InputCostPerMillion  float64 `json:"input_cost_per_million"`
	OutputCostPerMillion float64 `json:"output_cost_per_million"`
	Known                bool    `json:"known"`
}

// Run statuses.
const (
	RunStatusCreated       = "created"
	RunStatusRunning       = "running"
	RunStatusStopping      = "stopping"
	RunStatusStopped       = "stopped"
	RunStatusStoppedBudget = "stopped_budget"
	RunStatusInterrupted   = "interrupted"
	RunStatusCompleted     = "completed"
	RunStatusFailed        = "failed"
)

// RunStatuses lists every status, for GET /vocabulary.
var RunStatuses = []string{RunStatusCreated, RunStatusRunning, RunStatusStopping, RunStatusStopped, RunStatusStoppedBudget,
	RunStatusInterrupted, RunStatusCompleted, RunStatusFailed}

// Run origins.
const (
	RunOriginLive         = "live"
	RunOriginLegacyImport = "legacy-import"
)

// Case selection modes.
const (
	CaseSelectionAll    = "all"
	CaseSelectionSample = "sample"
)

// NewRun is what POST /runs takes.
type NewRun struct {
	SuiteID               string        `json:"suite_id"`
	CandidateModelIDs     []string      `json:"candidate_model_ids"`
	JudgeModelIDs         []string      `json:"judge_model_ids"`
	OriginalAnswerModelID string        `json:"original_answer_model_id"`
	CaseSelection         CaseSelection `json:"case_selection"`
	BudgetLimits          []BudgetLimit `json:"budget_limits"`
	Concurrency           int           `json:"concurrency"`
	OrganizationID        string        `json:"organization_id"`
	PredictionID          string        `json:"prediction_id"`
	Note                  string        `json:"note"`
}

// maximumCandidates is how many labels a judge can be shown: A to Z.
const maximumCandidates = 26

func (n *NewRun) validate() error {
	var problems []string
	if len(n.CandidateModelIDs) < 2 {
		problems = append(problems, "candidate_model_ids needs at least two models to compare")
	}
	if len(n.CandidateModelIDs) > maximumCandidates {
		problems = append(problems, fmt.Sprintf("at most %d candidates, one per label A to Z", maximumCandidates))
	}
	if duplicate := firstDuplicate(n.CandidateModelIDs); duplicate != "" {
		problems = append(problems, fmt.Sprintf("candidate %s is named twice", duplicate))
	}
	if duplicate := firstDuplicate(n.JudgeModelIDs); duplicate != "" {
		problems = append(problems, fmt.Sprintf("judge %s is named twice", duplicate))
	}
	if n.OriginalAnswerModelID != "" && !slices.Contains(n.CandidateModelIDs, n.OriginalAnswerModelID) {
		problems = append(problems, "original_answer_model_id must be one of the candidates")
	}
	switch n.CaseSelection.Mode {
	case CaseSelectionAll:
	case CaseSelectionSample:
		if n.CaseSelection.Size <= 0 {
			problems = append(problems, "a sample case_selection needs a positive size")
		}
	default:
		problems = append(problems, fmt.Sprintf("case_selection.mode %q is not %s or %s", n.CaseSelection.Mode, CaseSelectionAll, CaseSelectionSample))
	}
	if n.Concurrency < 0 {
		problems = append(problems, "concurrency may not be negative")
	}
	for _, limit := range n.BudgetLimits {
		if limit.Subscription == "" || limit.Window == "" || limit.MaxUsedPercent <= 0 || limit.MaxUsedPercent > 100 {
			problems = append(problems, fmt.Sprintf("budget limit %+v needs a subscription, a window and a percent in (0, 100]", limit))
		}
	}
	if n.PredictionID != "" && !strings.HasPrefix(n.PredictionID, "prediction_") {
		problems = append(problems, "prediction_id must be a prediction-store id (prediction_000001)")
	}
	if len(problems) > 0 {
		return invalidf("run: %s", strings.Join(problems, "; "))
	}
	return nil
}

func firstDuplicate(values []string) string {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return value
		}
		seen[value] = true
	}
	return ""
}

// selectCases applies a selection to a suite's cases, which come in creation
// order. A sample is the first size cases of a shuffle seeded by seed, put
// back in creation order, so the same seed picks the same cases.
func selectCases(all []*Case, selection CaseSelection) ([]string, error) {
	ids := make([]string, len(all))
	for i, c := range all {
		ids[i] = c.ID
	}
	if selection.Mode == CaseSelectionAll {
		return ids, nil
	}
	if selection.Size > len(ids) {
		return nil, invalidf("a sample of %d from a suite of %d cases", selection.Size, len(ids))
	}
	order := make([]int, len(ids))
	for i := range order {
		order[i] = i
	}
	random := rand.New(rand.NewPCG(uint64(selection.Seed), 0x657661_6c5f73616d))
	random.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	picked := order[:selection.Size]
	slices.Sort(picked)
	sample := make([]string, len(picked))
	for i, index := range picked {
		sample[i] = ids[index]
	}
	return sample, nil
}

// insertRun stores a run in tx with its prices already read.
func (s *Store) insertRun(tx *sql.Tx, run *Run) (string, error) {
	now := s.unixNow()
	return insertWithID(tx, "runs", runIDPrefix,
		`suite_id, candidate_model_ids, judge_model_ids, original_answer_model_id, case_selection, case_ids, status, status_reason,
		 budget_limits, concurrency, organization_id, model_prices, prices_note, prediction_id, note, origin,
		 created_at, started_at, finished_at, updated_at`,
		"?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
		run.SuiteID, mustJSON(run.CandidateModelIDs), mustJSON(run.JudgeModelIDs), run.OriginalAnswerModelID, mustJSON(run.CaseSelection),
		mustJSON(run.CaseIDs), run.Status, run.StatusReason, mustJSON(run.BudgetLimits), run.Concurrency, run.OrganizationID,
		mustJSON(run.ModelPrices), run.PricesNote, run.PredictionID, run.Note, run.Origin, now, run.StartedAt, run.FinishedAt, now)
}

const runColumns = `id, suite_id, candidate_model_ids, judge_model_ids, original_answer_model_id, case_selection, case_ids, status,
	status_reason, budget_limits, concurrency, organization_id, model_prices, prices_note, prediction_id, note, origin,
	created_at, started_at, finished_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var run Run
	var candidates, judges, selection, caseIDs, limits, prices string
	if err := row.Scan(&run.ID, &run.SuiteID, &candidates, &judges, &run.OriginalAnswerModelID, &selection, &caseIDs, &run.Status,
		&run.StatusReason, &limits, &run.Concurrency, &run.OrganizationID, &prices, &run.PricesNote, &run.PredictionID,
		&run.Note, &run.Origin, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.UpdatedAt); err != nil {
		return nil, err
	}
	for _, column := range []struct {
		raw    string
		target any
		name   string
	}{{candidates, &run.CandidateModelIDs, "candidate_model_ids"}, {judges, &run.JudgeModelIDs, "judge_model_ids"},
		{selection, &run.CaseSelection, "case_selection"}, {caseIDs, &run.CaseIDs, "case_ids"}, {limits, &run.BudgetLimits, "budget_limits"},
		{prices, &run.ModelPrices, "model_prices"}} {
		if err := decodeJSONColumn(column.raw, column.target, column.name); err != nil {
			return nil, err
		}
	}
	return &run, nil
}

// GetRun reads one run.
func (s *Store) GetRun(id string) (*Run, error) {
	run, err := scanRun(s.db.QueryRow("SELECT "+runColumns+" FROM runs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return run, err
}

// ListRuns reads runs, newest first, optionally of one suite.
func (s *Store) ListRuns(suiteID string) ([]*Run, error) {
	query, arguments := "SELECT "+runColumns+" FROM runs", []any{}
	if suiteID != "" {
		query += " WHERE suite_id = ?"
		arguments = append(arguments, suiteID)
	}
	rows, err := s.db.Query(query+" ORDER BY seq DESC", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []*Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// setRunStatus moves a run to status with a reason. finished stamps
// finished_at; started stamps started_at and records who the calls are for.
func (s *Store) setRunStatus(id, status, reason string) error {
	now := s.unixNow()
	finishedAt := int64(0)
	switch status {
	case RunStatusStopped, RunStatusStoppedBudget, RunStatusCompleted, RunStatusFailed, RunStatusInterrupted:
		finishedAt = now
	}
	_, err := s.db.Exec("UPDATE runs SET status=?, status_reason=?, finished_at=?, updated_at=? WHERE id=?", status, reason, finishedAt, now, id)
	return err
}

func (s *Store) markRunStarted(id, organizationID string) error {
	now := s.unixNow()
	_, err := s.db.Exec(`UPDATE runs SET status=?, status_reason='', organization_id=?, started_at=?, finished_at=0, updated_at=?
		WHERE id=?`, RunStatusRunning, organizationID, now, now, id)
	return err
}

// MarkInterruptedRuns is called at boot: a run the last process left running
// or stopping did not finish, and nothing is working on it now.
func (s *Store) MarkInterruptedRuns() (int64, error) {
	result, err := s.db.Exec("UPDATE runs SET status=?, status_reason=?, finished_at=?, updated_at=? WHERE status IN (?, ?)",
		RunStatusInterrupted, "the service stopped while the run was going; resume it", s.unixNow(), s.unixNow(), RunStatusRunning, RunStatusStopping)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteRun removes a run that is not going, with its answers, verdicts and
// checks.
func (s *Store) DeleteRun(id string) error {
	run, err := s.GetRun(id)
	if err != nil {
		return err
	}
	if run.Status == RunStatusRunning || run.Status == RunStatusStopping {
		return conflictf("run %s is %s; stop it first", id, run.Status)
	}
	_, err = s.db.Exec("DELETE FROM runs WHERE id = ?", id)
	return err
}

// labelsFor is the labels a judge sees for n candidates: A, B, C, …
func labelsFor(n int) []string {
	labels := make([]string, n)
	for i := range labels {
		labels[i] = string(rune('A' + i))
	}
	return labels
}

// RunPatch carries the fields of a run that may change after it is made.
type RunPatch struct {
	PredictionID *string `json:"prediction_id"`
	Note         *string `json:"note"`
}

// PatchRun changes a run's prediction link or note; nothing else about a run
// moves after it is made.
func (s *Store) PatchRun(id string, patch RunPatch) (*Run, error) {
	run, err := s.GetRun(id)
	if err != nil {
		return nil, err
	}
	setIf(&run.PredictionID, patch.PredictionID)
	setIf(&run.Note, patch.Note)
	if run.PredictionID != "" && !strings.HasPrefix(run.PredictionID, "prediction_") {
		return nil, invalidf("prediction_id must be a prediction-store id (prediction_000001)")
	}
	if _, err := s.db.Exec("UPDATE runs SET prediction_id=?, note=?, updated_at=? WHERE id=?", run.PredictionID, run.Note, s.unixNow(), id); err != nil {
		return nil, err
	}
	return s.GetRun(id)
}
