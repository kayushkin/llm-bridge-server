package evalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Suite is a task and how to judge it: the instructions and failure kinds a
// judge gets, the schema answers follow, and the checker that reads them.
type Suite struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	TaskKind          string            `json:"task_kind"`
	JudgeInstructions string            `json:"judge_instructions"`
	JudgeFailureKinds []string          `json:"judge_failure_kinds"`
	OutputSchema      json.RawMessage   `json:"output_schema"`
	MaxTokens         int               `json:"max_tokens"`
	JudgeMaxTokens    int               `json:"judge_max_tokens"`
	CheckerName       string            `json:"checker_name"`
	BaselineModelID   string            `json:"baseline_model_id"`
	WeeklyCallsSource WeeklyCallsSource `json:"weekly_calls_source"`
	CostModel         CostModel         `json:"cost_model"`
	CreatedAt         int64             `json:"created_at"`
	UpdatedAt         int64             `json:"updated_at"`
}

// Failure kinds every suite must offer: the judge prompt tells a judge to use
// them for a flawless answer and for a candidate that gave none.
const (
	FailureKindNone     = "none"
	FailureKindNoAnswer = "no_answer"
)

// WeeklyCallsSource says how many calls a week the task makes, which turns a
// price per call into a price per week.
type WeeklyCallsSource struct {
	// Kind is fixed (CallsPerWeek, with Note saying where it came from) or
	// llm-bridge-operations (count the operations Operations matches over the
	// last seven days, read from llm-bridge-server when a report asks).
	Kind         string            `json:"kind,omitempty"`
	CallsPerWeek float64           `json:"calls_per_week,omitempty"`
	Operations   *OperationsFilter `json:"operations,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// The kinds of WeeklyCallsSource.
const (
	WeeklyCallsSourceFixed               = "fixed"
	WeeklyCallsSourceLLMBridgeOperations = "llm-bridge-operations"
)

// CostModel says how GET /runs/{id}/cost-projection models a week of the
// task. Input tokens per call are
//
//	harness multiplier × (fixed + Σ input_tokens_per_feature[f] × feature f)
//
// where the multiplier is a model's mean input over the baseline's (the
// baseline's is 1). Each coefficient is pinned here or, when absent, fitted
// by least squares to the baseline's answers in the run. Output tokens are
// per unit of OutputPerFeature, or per call when that is empty.
//
// The task's specifics — which features exist, how calls batch — live here,
// on the suite, never in the projection code.
type CostModel struct {
	Features              []string           `json:"features,omitempty"`
	FixedInputTokens      *float64           `json:"fixed_input_tokens,omitempty"`
	InputTokensPerFeature map[string]float64 `json:"input_tokens_per_feature,omitempty"`
	OutputPerFeature      string             `json:"output_per_feature,omitempty"`
	Batching              *Batching          `json:"batching,omitempty"`
	Note                  string             `json:"note,omitempty"`
}

// Batching describes a task that runs on a timer and handles whatever units
// arrived since the last tick, e.g. mail filed every fifteen minutes. A week
// of V units is V / per_call calls, where per_call is V / ticks_per_week held
// between the two bounds.
type Batching struct {
	Feature        string  `json:"feature"`
	TicksPerWeek   float64 `json:"ticks_per_week"`
	MinimumPerCall float64 `json:"minimum_per_call"`
	MaximumPerCall float64 `json:"maximum_per_call"`
}

func (s *Suite) validate() error {
	var problems []string
	if strings.TrimSpace(s.Name) == "" {
		problems = append(problems, "name is required")
	}
	if strings.TrimSpace(s.TaskKind) == "" {
		problems = append(problems, "task_kind is required")
	}
	if strings.TrimSpace(s.JudgeInstructions) == "" {
		problems = append(problems, "judge_instructions is required")
	}
	if !slices.Contains(s.JudgeFailureKinds, FailureKindNone) || !slices.Contains(s.JudgeFailureKinds, FailureKindNoAnswer) {
		problems = append(problems, fmt.Sprintf("judge_failure_kinds must hold %q and %q, which the judge prompt names", FailureKindNone, FailureKindNoAnswer))
	}
	var schema map[string]any
	if len(s.OutputSchema) == 0 || json.Unmarshal(s.OutputSchema, &schema) != nil {
		problems = append(problems, "output_schema must be a JSON Schema object")
	}
	if s.MaxTokens <= 0 {
		problems = append(problems, "max_tokens must be positive")
	}
	if s.JudgeMaxTokens <= 0 {
		problems = append(problems, "judge_max_tokens must be positive")
	}
	if s.CheckerName != "" {
		if _, known := checkers[s.CheckerName]; !known {
			problems = append(problems, fmt.Sprintf("checker_name %q is not one of %v", s.CheckerName, CheckerNames()))
		}
	}
	switch s.WeeklyCallsSource.Kind {
	case "":
	case WeeklyCallsSourceFixed:
		if s.WeeklyCallsSource.CallsPerWeek <= 0 || s.WeeklyCallsSource.Note == "" {
			problems = append(problems, "a fixed weekly_calls_source needs calls_per_week and a note saying where the number came from")
		}
	case WeeklyCallsSourceLLMBridgeOperations:
		if s.WeeklyCallsSource.Operations == nil {
			problems = append(problems, "an llm-bridge-operations weekly_calls_source needs operations, the filter to count")
		}
	default:
		problems = append(problems, fmt.Sprintf("weekly_calls_source.kind %q is not %s or %s", s.WeeklyCallsSource.Kind, WeeklyCallsSourceFixed, WeeklyCallsSourceLLMBridgeOperations))
	}
	problems = append(problems, s.CostModel.problems()...)
	if len(problems) > 0 {
		return invalidf("suite: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (c CostModel) problems() []string {
	var problems []string
	for feature := range c.InputTokensPerFeature {
		if !slices.Contains(c.Features, feature) {
			problems = append(problems, fmt.Sprintf("cost_model pins a coefficient for %q, which is not in cost_model.features", feature))
		}
	}
	if c.Batching != nil {
		b := c.Batching
		if !slices.Contains(c.Features, b.Feature) {
			problems = append(problems, fmt.Sprintf("cost_model.batching.feature %q is not in cost_model.features", b.Feature))
		}
		if b.TicksPerWeek <= 0 || b.MinimumPerCall <= 0 || b.MaximumPerCall < b.MinimumPerCall {
			problems = append(problems, "cost_model.batching needs ticks_per_week > 0 and 0 < minimum_per_call <= maximum_per_call")
		}
	}
	return problems
}

// SuitePatch carries only the fields a caller mentioned.
type SuitePatch struct {
	Name              *string            `json:"name"`
	Description       *string            `json:"description"`
	TaskKind          *string            `json:"task_kind"`
	JudgeInstructions *string            `json:"judge_instructions"`
	JudgeFailureKinds *[]string          `json:"judge_failure_kinds"`
	OutputSchema      *json.RawMessage   `json:"output_schema"`
	MaxTokens         *int               `json:"max_tokens"`
	JudgeMaxTokens    *int               `json:"judge_max_tokens"`
	CheckerName       *string            `json:"checker_name"`
	BaselineModelID   *string            `json:"baseline_model_id"`
	WeeklyCallsSource *WeeklyCallsSource `json:"weekly_calls_source"`
	CostModel         *CostModel         `json:"cost_model"`
}

const suiteColumns = `id, name, description, task_kind, judge_instructions, judge_failure_kinds, output_schema, max_tokens,
	judge_max_tokens, checker_name, baseline_model_id, weekly_calls_source, cost_model, created_at, updated_at`

func scanSuite(row interface{ Scan(...any) error }) (*Suite, error) {
	var suite Suite
	var failureKinds, schema, weekly, costModel string
	if err := row.Scan(&suite.ID, &suite.Name, &suite.Description, &suite.TaskKind, &suite.JudgeInstructions, &failureKinds, &schema,
		&suite.MaxTokens, &suite.JudgeMaxTokens, &suite.CheckerName, &suite.BaselineModelID, &weekly, &costModel, &suite.CreatedAt, &suite.UpdatedAt); err != nil {
		return nil, err
	}
	suite.OutputSchema = json.RawMessage(schema)
	if err := decodeJSONColumn(failureKinds, &suite.JudgeFailureKinds, "judge_failure_kinds"); err != nil {
		return nil, err
	}
	if err := decodeJSONColumn(weekly, &suite.WeeklyCallsSource, "weekly_calls_source"); err != nil {
		return nil, err
	}
	if err := decodeJSONColumn(costModel, &suite.CostModel, "cost_model"); err != nil {
		return nil, err
	}
	return &suite, nil
}

// CreateSuite validates and stores a suite.
func (s *Store) CreateSuite(suite *Suite) (*Suite, error) {
	if err := suite.validate(); err != nil {
		return nil, err
	}
	var id string
	err := s.inTransaction(func(tx *sql.Tx) error {
		var err error
		now := s.unixNow()
		id, err = insertWithID(tx, "suites", suiteIDPrefix,
			`name, description, task_kind, judge_instructions, judge_failure_kinds, output_schema, max_tokens, judge_max_tokens,
			 checker_name, baseline_model_id, weekly_calls_source, cost_model, created_at, updated_at`,
			"?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
			suite.Name, suite.Description, suite.TaskKind, suite.JudgeInstructions, mustJSON(suite.JudgeFailureKinds), string(suite.OutputSchema),
			suite.MaxTokens, suite.JudgeMaxTokens, suite.CheckerName, suite.BaselineModelID, mustJSON(suite.WeeklyCallsSource), mustJSON(suite.CostModel), now, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create suite: %w", err)
	}
	return s.GetSuite(id)
}

// GetSuite reads one suite.
func (s *Store) GetSuite(id string) (*Suite, error) {
	suite, err := scanSuite(s.db.QueryRow("SELECT "+suiteColumns+" FROM suites WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: suite %s", ErrNotFound, id)
	}
	return suite, err
}

// ListSuites reads every suite, oldest first.
func (s *Store) ListSuites() ([]*Suite, error) {
	rows, err := s.db.Query("SELECT " + suiteColumns + " FROM suites ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suites := []*Suite{}
	for rows.Next() {
		suite, err := scanSuite(rows)
		if err != nil {
			return nil, err
		}
		suites = append(suites, suite)
	}
	return suites, rows.Err()
}

// PatchSuite changes the fields the patch names and validates the result.
func (s *Store) PatchSuite(id string, patch SuitePatch) (*Suite, error) {
	suite, err := s.GetSuite(id)
	if err != nil {
		return nil, err
	}
	setIf(&suite.Name, patch.Name)
	setIf(&suite.Description, patch.Description)
	setIf(&suite.TaskKind, patch.TaskKind)
	setIf(&suite.JudgeInstructions, patch.JudgeInstructions)
	setIf(&suite.JudgeFailureKinds, patch.JudgeFailureKinds)
	setIf(&suite.OutputSchema, patch.OutputSchema)
	setIf(&suite.MaxTokens, patch.MaxTokens)
	setIf(&suite.JudgeMaxTokens, patch.JudgeMaxTokens)
	setIf(&suite.CheckerName, patch.CheckerName)
	setIf(&suite.BaselineModelID, patch.BaselineModelID)
	setIf(&suite.WeeklyCallsSource, patch.WeeklyCallsSource)
	setIf(&suite.CostModel, patch.CostModel)
	if err := suite.validate(); err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`UPDATE suites SET name=?, description=?, task_kind=?, judge_instructions=?, judge_failure_kinds=?, output_schema=?,
		max_tokens=?, judge_max_tokens=?, checker_name=?, baseline_model_id=?, weekly_calls_source=?, cost_model=?, updated_at=? WHERE id=?`,
		suite.Name, suite.Description, suite.TaskKind, suite.JudgeInstructions, mustJSON(suite.JudgeFailureKinds), string(suite.OutputSchema),
		suite.MaxTokens, suite.JudgeMaxTokens, suite.CheckerName, suite.BaselineModelID, mustJSON(suite.WeeklyCallsSource), mustJSON(suite.CostModel),
		s.unixNow(), id)
	if err != nil {
		return nil, fmt.Errorf("patch suite %s: %w", id, err)
	}
	return s.GetSuite(id)
}

// DeleteSuite removes a suite and its cases. A suite that has runs is
// refused: the runs' reports would lose their cases.
func (s *Store) DeleteSuite(id string) error {
	if _, err := s.GetSuite(id); err != nil {
		return err
	}
	var runs int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM runs WHERE suite_id = ?", id).Scan(&runs); err != nil {
		return err
	}
	if runs > 0 {
		return conflictf("suite %s has %d runs; delete them first", id, runs)
	}
	_, err := s.db.Exec("DELETE FROM suites WHERE id = ?", id)
	return err
}

func setIf[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}
