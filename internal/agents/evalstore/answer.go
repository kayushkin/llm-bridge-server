package evalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Answer is one model's answer to one case in one run.
type Answer struct {
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	CaseID           string          `json:"case_id"`
	ModelID          string          `json:"model_id"`
	Origin           string          `json:"origin"`
	Attempt          int             `json:"attempt"`
	Parsed           json.RawMessage `json:"parsed,omitempty"`
	Error            string          `json:"error"`
	InputTokens      int64           `json:"input_tokens"`
	CacheReadTokens  int64           `json:"cache_read_tokens"`
	CacheWriteTokens int64           `json:"cache_write_tokens"`
	OutputTokens     int64           `json:"output_tokens"`
	UsageKnown       bool            `json:"usage_known"`
	DurationMs       *int64          `json:"duration_ms"`
	OperationID      string          `json:"operation_id"`
	AnsweredBy       string          `json:"answered_by"`
	CreatedAt        int64           `json:"created_at"`
}

// Answer origins.
const (
	AnswerOriginReplayed     = "replayed"
	AnswerOriginCaseOriginal = "case-original"
	AnswerOriginLegacyImport = "legacy-import"
)

// TotalInputTokens is input as a price sees it: fresh input plus cache reads
// and writes, each of which the bridge also charges at the input price.
func (a *Answer) TotalInputTokens() int64 {
	return a.InputTokens + a.CacheReadTokens + a.CacheWriteTokens
}

// HasAnswer says whether the model produced something a judge can read.
func (a *Answer) HasAnswer() bool {
	return a.Error == "" && len(a.Parsed) > 0 && string(a.Parsed) != "null"
}

const answerColumns = `id, run_id, case_id, model_id, origin, attempt, parsed, error, input_tokens, cache_read_tokens, cache_write_tokens,
	output_tokens, usage_known, duration_ms, operation_id, answered_by, created_at`

func scanAnswer(row interface{ Scan(...any) error }) (*Answer, error) {
	var a Answer
	var parsed sql.NullString
	var usageKnown int
	var duration sql.NullInt64
	if err := row.Scan(&a.ID, &a.RunID, &a.CaseID, &a.ModelID, &a.Origin, &a.Attempt, &parsed, &a.Error, &a.InputTokens, &a.CacheReadTokens,
		&a.CacheWriteTokens, &a.OutputTokens, &usageKnown, &duration, &a.OperationID, &a.AnsweredBy, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.Parsed, a.UsageKnown, a.DurationMs = rawFromNullable(parsed), usageKnown == 1, intFromNullable(duration)
	return &a, nil
}

// saveAnswer writes the answer for (run, case, model), replacing an earlier
// one: a retry keeps the row and its id.
func (s *Store) saveAnswer(a *Answer) error {
	return s.inTransaction(func(tx *sql.Tx) error { return s.saveAnswerIn(tx, a) })
}

func (s *Store) saveAnswerIn(tx *sql.Tx, a *Answer) error {
	values := []any{a.Origin, a.Attempt, nullableRaw(a.Parsed), a.Error, a.InputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.OutputTokens,
		boolToInt(a.UsageKnown), nullableInt(a.DurationMs), a.OperationID, a.AnsweredBy}
	result, err := tx.Exec(`UPDATE answers SET origin=?, attempt=?, parsed=?, error=?, input_tokens=?, cache_read_tokens=?, cache_write_tokens=?,
		output_tokens=?, usage_known=?, duration_ms=?, operation_id=?, answered_by=? WHERE run_id=? AND case_id=? AND model_id=?`,
		append(values, a.RunID, a.CaseID, a.ModelID)...)
	if err != nil {
		return fmt.Errorf("save answer: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed > 0 {
		return nil
	}
	_, err = insertWithID(tx, "answers", answerIDPrefix,
		`run_id, case_id, model_id, origin, attempt, parsed, error, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
		 usage_known, duration_ms, operation_id, answered_by, created_at`,
		"?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
		append(append([]any{a.RunID, a.CaseID, a.ModelID}, values...), s.unixNow())...)
	if err != nil {
		return fmt.Errorf("save answer: %w", err)
	}
	return nil
}

// AnswerFilter selects answers. Empty fields filter nothing.
type AnswerFilter struct {
	CaseID  string
	ModelID string
}

// ListAnswers reads a run's answers in case and model order.
func (s *Store) ListAnswers(runID string, filter AnswerFilter) ([]*Answer, error) {
	query, arguments := "SELECT "+answerColumns+" FROM answers WHERE run_id = ?", []any{runID}
	if filter.CaseID != "" {
		query += " AND case_id = ?"
		arguments = append(arguments, filter.CaseID)
	}
	if filter.ModelID != "" {
		query += " AND model_id = ?"
		arguments = append(arguments, filter.ModelID)
	}
	rows, err := s.db.Query(query+" ORDER BY seq", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	answers := []*Answer{}
	for rows.Next() {
		a, err := scanAnswer(rows)
		if err != nil {
			return nil, err
		}
		answers = append(answers, a)
	}
	return answers, rows.Err()
}

// answerKey names an answer within a run.
type answerKey struct{ caseID, modelID string }

func (s *Store) answersByKey(runID string) (map[answerKey]*Answer, error) {
	answers, err := s.ListAnswers(runID, AnswerFilter{})
	if err != nil {
		return nil, err
	}
	byKey := make(map[answerKey]*Answer, len(answers))
	for _, a := range answers {
		byKey[answerKey{a.CaseID, a.ModelID}] = a
	}
	return byKey, nil
}

// Verdict is one judge's verdict on every candidate of one case.
type Verdict struct {
	ID            string             `json:"id"`
	RunID         string             `json:"run_id"`
	CaseID        string             `json:"case_id"`
	JudgeModelID  string             `json:"judge_model_id"`
	JudgeProvider string             `json:"judge_provider"`
	Attempt       int                `json:"attempt"`
	LabelMapping  map[string]string  `json:"label_mapping"`
	Parsed        json.RawMessage    `json:"parsed,omitempty"`
	Valid         bool               `json:"valid"`
	InvalidReason string             `json:"invalid_reason"`
	Error         string             `json:"error"`
	InputTokens   int64              `json:"input_tokens"`
	OutputTokens  int64              `json:"output_tokens"`
	DurationMs    *int64             `json:"duration_ms"`
	OperationID   string             `json:"operation_id"`
	CreatedAt     int64              `json:"created_at"`
	Candidates    []VerdictCandidate `json:"candidates,omitempty"`
}

// VerdictCandidate is one candidate's line of a valid verdict, unshuffled.
type VerdictCandidate struct {
	ModelID     string `json:"model_id"`
	Label       string `json:"label"`
	Rank        int    `json:"rank"`
	Acceptable  bool   `json:"acceptable"`
	FailureKind string `json:"failure_kind"`
	Reason      string `json:"reason"`
}

// judgeAnswer is the shape the judge schema forces.
type judgeAnswer struct {
	Ranking  []string           `json:"ranking"`
	Verdicts []judgeVerdictLine `json:"verdicts"`
}

type judgeVerdictLine struct {
	Label       string `json:"label"`
	Acceptable  bool   `json:"acceptable"`
	FailureKind string `json:"failure_kind"`
	Reason      string `json:"reason"`
}

// interpretVerdict unshuffles a judge's answer through the label mapping. It
// holds the answer to the rule the Python bench held it to: the ranking and
// the verdicts must each name every label exactly once. Anything else is
// malformed and left out of every report figure.
func interpretVerdict(parsed json.RawMessage, mapping map[string]string) ([]VerdictCandidate, error) {
	var answer judgeAnswer
	if err := json.Unmarshal(parsed, &answer); err != nil {
		return nil, fmt.Errorf("not a ranking and verdicts: %v", err)
	}
	labels := make([]string, 0, len(mapping))
	for label := range mapping {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	ranking := slices.Sorted(slices.Values(answer.Ranking))
	if !slices.Equal(ranking, labels) {
		return nil, fmt.Errorf("ranking %v does not name each of %v once", answer.Ranking, labels)
	}
	verdictLabels := make([]string, len(answer.Verdicts))
	for i, v := range answer.Verdicts {
		verdictLabels[i] = v.Label
	}
	slices.Sort(verdictLabels)
	if !slices.Equal(verdictLabels, labels) {
		return nil, fmt.Errorf("verdicts name %v, not each of %v once", verdictLabels, labels)
	}
	candidates := make([]VerdictCandidate, 0, len(labels))
	for _, label := range labels {
		v := answer.Verdicts[slices.IndexFunc(answer.Verdicts, func(line judgeVerdictLine) bool { return line.Label == label })]
		candidates = append(candidates, VerdictCandidate{ModelID: mapping[label], Label: label, Rank: slices.Index(answer.Ranking, label) + 1,
			Acceptable: v.Acceptable, FailureKind: v.FailureKind, Reason: v.Reason})
	}
	return candidates, nil
}

// saveVerdict writes the verdict for (run, case, judge), replacing an
// earlier one, and its candidate lines when it is valid.
func (s *Store) saveVerdict(v *Verdict) error {
	return s.inTransaction(func(tx *sql.Tx) error { return s.saveVerdictIn(tx, v) })
}

func (s *Store) saveVerdictIn(tx *sql.Tx, v *Verdict) error {
	v.Valid, v.InvalidReason, v.Candidates = false, "", nil
	if v.Error == "" {
		candidates, err := interpretVerdict(v.Parsed, v.LabelMapping)
		if err != nil {
			v.InvalidReason = err.Error()
		} else {
			v.Valid, v.Candidates = true, candidates
		}
	}
	values := []any{v.JudgeProvider, v.Attempt, mustJSON(v.LabelMapping), nullableRaw(v.Parsed), boolToInt(v.Valid), v.InvalidReason, v.Error,
		v.InputTokens, v.OutputTokens, nullableInt(v.DurationMs), v.OperationID}
	var id string
	err := tx.QueryRow("SELECT id FROM verdicts WHERE run_id=? AND case_id=? AND judge_model_id=?", v.RunID, v.CaseID, v.JudgeModelID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = insertWithID(tx, "verdicts", verdictIDPrefix,
			`run_id, case_id, judge_model_id, judge_provider, attempt, label_mapping, parsed, valid, invalid_reason, error, input_tokens,
			 output_tokens, duration_ms, operation_id, created_at`,
			"?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
			append(append([]any{v.RunID, v.CaseID, v.JudgeModelID}, values...), s.unixNow())...)
		if err != nil {
			return fmt.Errorf("save verdict: %w", err)
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`UPDATE verdicts SET judge_provider=?, attempt=?, label_mapping=?, parsed=?, valid=?, invalid_reason=?, error=?,
			input_tokens=?, output_tokens=?, duration_ms=?, operation_id=? WHERE id=?`, append(values, id)...); err != nil {
			return fmt.Errorf("save verdict: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM verdict_candidates WHERE verdict_id = ?", id); err != nil {
			return err
		}
	}
	v.ID = id
	for _, c := range v.Candidates {
		if _, err := tx.Exec(`INSERT INTO verdict_candidates (verdict_id, model_id, label, rank, acceptable, failure_kind, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, c.ModelID, c.Label, c.Rank, boolToInt(c.Acceptable), c.FailureKind, c.Reason); err != nil {
			return fmt.Errorf("save verdict line: %w", err)
		}
	}
	return nil
}

const verdictColumns = `id, run_id, case_id, judge_model_id, judge_provider, attempt, label_mapping, parsed, valid, invalid_reason, error,
	input_tokens, output_tokens, duration_ms, operation_id, created_at`

// VerdictFilter selects verdicts. Empty fields filter nothing.
type VerdictFilter struct {
	CaseID       string
	JudgeModelID string
}

// ListVerdicts reads a run's verdicts with their candidate lines.
func (s *Store) ListVerdicts(runID string, filter VerdictFilter) ([]*Verdict, error) {
	query, arguments := "SELECT "+verdictColumns+" FROM verdicts WHERE run_id = ?", []any{runID}
	if filter.CaseID != "" {
		query += " AND case_id = ?"
		arguments = append(arguments, filter.CaseID)
	}
	if filter.JudgeModelID != "" {
		query += " AND judge_model_id = ?"
		arguments = append(arguments, filter.JudgeModelID)
	}
	rows, err := s.db.Query(query+" ORDER BY seq", arguments...)
	if err != nil {
		return nil, err
	}
	verdicts := []*Verdict{}
	byID := map[string]*Verdict{}
	for rows.Next() {
		var v Verdict
		var mapping string
		var parsed sql.NullString
		var valid int
		var duration sql.NullInt64
		if err := rows.Scan(&v.ID, &v.RunID, &v.CaseID, &v.JudgeModelID, &v.JudgeProvider, &v.Attempt, &mapping, &parsed, &valid, &v.InvalidReason,
			&v.Error, &v.InputTokens, &v.OutputTokens, &duration, &v.OperationID, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if err := decodeJSONColumn(mapping, &v.LabelMapping, "label_mapping"); err != nil {
			rows.Close()
			return nil, err
		}
		v.Parsed, v.Valid, v.DurationMs = rawFromNullable(parsed), valid == 1, intFromNullable(duration)
		verdicts = append(verdicts, &v)
		byID[v.ID] = &v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lines, err := s.db.Query(`SELECT c.verdict_id, c.model_id, c.label, c.rank, c.acceptable, c.failure_kind, c.reason
		FROM verdict_candidates c JOIN verdicts v ON v.id = c.verdict_id WHERE v.run_id = ? ORDER BY c.verdict_id, c.rank`, runID)
	if err != nil {
		return nil, err
	}
	defer lines.Close()
	for lines.Next() {
		var verdictID string
		var c VerdictCandidate
		var acceptable int
		if err := lines.Scan(&verdictID, &c.ModelID, &c.Label, &c.Rank, &acceptable, &c.FailureKind, &c.Reason); err != nil {
			return nil, err
		}
		c.Acceptable = acceptable == 1
		if v, ok := byID[verdictID]; ok {
			v.Candidates = append(v.Candidates, c)
		}
	}
	return verdicts, lines.Err()
}

// CheckResult is one deterministic check of one answer.
type CheckResult struct {
	RunID     string `json:"run_id"`
	CaseID    string `json:"case_id"`
	ModelID   string `json:"model_id"`
	CheckName string `json:"check_name"`
	Passed    bool   `json:"passed"`
	Count     int    `json:"count"`
	Detail    string `json:"detail"`
}

func (s *Store) replaceChecks(runID string, results []CheckResult) error {
	return s.inTransaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM checks WHERE run_id = ?", runID); err != nil {
			return err
		}
		for _, r := range results {
			if _, err := tx.Exec(`INSERT INTO checks (run_id, case_id, model_id, check_name, passed, count, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, r.CaseID, r.ModelID, r.CheckName, boolToInt(r.Passed), r.Count, r.Detail); err != nil {
				return fmt.Errorf("save check %s: %w", r.CheckName, err)
			}
		}
		return nil
	})
}

// ListChecks reads a run's check results.
func (s *Store) ListChecks(runID string) ([]CheckResult, error) {
	rows, err := s.db.Query(`SELECT k.run_id, k.case_id, k.model_id, k.check_name, k.passed, k.count, k.detail
		FROM checks k JOIN cases c ON c.id = k.case_id WHERE k.run_id = ? ORDER BY c.seq, k.model_id, k.check_name`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []CheckResult{}
	for rows.Next() {
		var r CheckResult
		var passed int
		if err := rows.Scan(&r.RunID, &r.CaseID, &r.ModelID, &r.CheckName, &passed, &r.Count, &r.Detail); err != nil {
			return nil, err
		}
		r.Passed = passed == 1
		results = append(results, r)
	}
	return results, rows.Err()
}

func trimmedError(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}
