package evalstore

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// The Python bench this imports ran two tasks, which it called email and
// demo. Its judge instructions and failure kinds lived in its scripts, not
// its data files, so they are restated here as the bench's constants; every
// one of its judge scripts used the same text.
type legacyTask struct {
	command           string
	suiteName         string
	taskKind          string
	checkerName       string
	judgeInstructions string
	failureKinds      []string
	callsPerWeek      float64
	costModel         CostModel
}

func legacyTasks() []legacyTask {
	fixed := 3800.0
	return []legacyTask{
		{
			command: "email", suiteName: "email filing (scheduler email-classifier)", taskKind: "email-filing", checkerName: "email-filing",
			judgeInstructions: "Check every answer against the cards and emails actually in the prompt. The failures that matter: filing an email to the wrong card, a card_id that does not appear in the prompt, a deadline stated in the mail but missed or wrong, a wrong urgency/priority, an email with no filing or a filing for an email that is not there, and a new card created when a fitting card already exists. An answer is acceptable if a careful human operator would accept this filing as is.",
			failureKinds:      []string{"wrong_card", "invented_card_id", "missed_deadline", "wrong_deadline", "wrong_priority", "missing_or_extra_filing", "needless_new_card", "bad_card_update", "no_answer", "other", "none"},
			callsPerWeek:      190,
			costModel: CostModel{Features: []string{"cards", "emails"}, FixedInputTokens: &fixed,
				InputTokensPerFeature: map[string]float64{"cards": 70, "emails": 700}, OutputPerFeature: "emails",
				Batching: &Batching{Feature: "emails", TicksPerWeek: 672, MinimumPerCall: 1.4, MaximumPerCall: 10},
				Note:     "Coefficients pinned from the 2026-09-30..10-08 bench's cost_model.py. The classifier runs every 15 minutes, 672 ticks a week, and files whatever mail arrived since the last tick, at least 1.4 and at most 10 emails a call."},
		},
		{
			command: "demo", suiteName: "demo mail (scheduler demo-mail-generator)", taskKind: "demo-mail", checkerName: "demo-mail",
			judgeInstructions: "Check every email against the project facts in the prompt (packages, routes, pull requests, commits, files). A request the repository cannot satisfy, or an invented package, route, PR, commit or person, is a failure. Also judge realism: does it read like mail a real colleague would send, and does each message follow the brief it was given (sender, recipient, kind). An answer is acceptable if a careful human operator would send this set of emails as is.",
			failureKinds:      []string{"request_repo_cannot_satisfy", "invented_fact_or_name", "unrealistic_or_uniform", "wrong_or_missing_key", "ignores_brief", "no_answer", "other", "none"},
			callsPerWeek:      84,
			costModel:         CostModel{Note: "One fixed input per call, fitted to the baseline, as the bench's cost_model.py priced demo mail."},
		},
	}
}

const legacyJudgeMaxTokens = 16000

// legacyCase is one row of the bench's cases.json.
type legacyCase struct {
	ID      string          `json:"id"`
	Command string          `json:"cmd"`
	Model   json.RawMessage `json:"model"`
	System  string          `json:"system"`
	Prompt  string          `json:"prompt"`
	Answer  json.RawMessage `json:"answer"`
	TIn     int64           `json:"tin"`
	TCR     int64           `json:"tcr"`
	TCW     int64           `json:"tcw"`
	TOut    int64           `json:"tout"`
	TS0     string          `json:"ts0"`
	TS1     string          `json:"ts1"`
}

type legacyUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// legacyReplay is one row of replay-claude.jsonl or replay-codex.jsonl.
type legacyReplay struct {
	ID         string          `json:"id"`
	Command    string          `json:"cmd"`
	ModelReq   string          `json:"model_req"`
	Model      string          `json:"model"`
	Parsed     json.RawMessage `json:"parsed"`
	Usage      *legacyUsage    `json:"usage"`
	DurationMs *int64          `json:"duration_ms"`
	Error      string          `json:"error"`
}

// legacyJudgment is one row of a judge*-<model>.jsonl file.
type legacyJudgment struct {
	ID         string            `json:"id"`
	Command    string            `json:"cmd"`
	Mapping    map[string]string `json:"mapping"`
	Parsed     json.RawMessage   `json:"parsed"`
	Usage      *legacyUsage      `json:"usage"`
	DurationMs *int64            `json:"duration_ms"`
	Error      string            `json:"error"`
}

// legacyJudgeFile matches judge-claude-opus-5-5.jsonl, judge8-gpt-6-sol.jsonl…
var legacyJudgeFile = regexp.MustCompile(`^(judge\d*)-(.+)\.jsonl$`)

// LegacyImportResult says what the import made.
type LegacyImportResult struct {
	Suites []string          `json:"suites"`
	Cases  int               `json:"cases"`
	Runs   []LegacyImportRun `json:"runs"`
}

// LegacyImportRun is one rebuilt judging.
type LegacyImportRun struct {
	RunID      string   `json:"run_id"`
	SuiteID    string   `json:"suite_id"`
	Judging    string   `json:"judging"`
	Candidates []string `json:"candidates"`
	Judges     []string `json:"judges"`
}

// ImportLegacyBench imports the Python bench in dir: its cases become two
// suites' cases (source claude-code-transcript), and each of its judgings
// (judge-, judge5-, judge7-, judge8-) becomes one completed run per suite,
// with the original answers standing as the original model's.
func (s *Store) ImportLegacyBench(ctx context.Context, dir string, catalog ModelCatalog) (*LegacyImportResult, error) {
	var cases []legacyCase
	if err := readJSONFile(filepath.Join(dir, "cases.json"), &cases); err != nil {
		return nil, err
	}
	var emailSchemas struct {
		Report json.RawMessage `json:"report"`
		Max    int             `json:"max"`
	}
	if err := readJSONFile(filepath.Join(dir, "email_schemas.json"), &emailSchemas); err != nil {
		return nil, err
	}
	var demoSchema struct {
		Schema json.RawMessage `json:"schema"`
		Max    int             `json:"max"`
	}
	if err := readJSONFile(filepath.Join(dir, "demo_schema.json"), &demoSchema); err != nil {
		return nil, err
	}
	schemas := map[string]struct {
		schema    json.RawMessage
		maxTokens int
	}{"email": {emailSchemas.Report, emailSchemas.Max}, "demo": {demoSchema.Schema, demoSchema.Max}}

	replays := map[answerKey]legacyReplay{}
	var replayOrder []string
	for _, name := range []string{"replay-codex.jsonl", "replay-claude.jsonl"} {
		rows, err := readJSONLines[legacyReplay](filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := answerKey{row.ID, row.ModelReq}
			if _, duplicate := replays[key]; duplicate {
				return nil, fmt.Errorf("%s: case %s has two replays by %s", name, row.ID, row.ModelReq)
			}
			replays[key] = row
			if !slices.Contains(replayOrder, row.ModelReq) {
				replayOrder = append(replayOrder, row.ModelReq)
			}
		}
	}

	judgings, err := readLegacyJudgings(dir)
	if err != nil {
		return nil, err
	}

	casesByCommand := map[string][]legacyCase{}
	originalModels := map[string]bool{}
	for _, c := range cases {
		if _, known := schemas[c.Command]; !known {
			return nil, fmt.Errorf("cases.json: case %s has cmd %q; the bench knew email and demo", c.ID, c.Command)
		}
		casesByCommand[c.Command] = append(casesByCommand[c.Command], c)
		modelID, err := legacyModelID(c.Model)
		if err != nil {
			return nil, fmt.Errorf("cases.json: case %s: %w", c.ID, err)
		}
		originalModels[modelID] = true
	}
	if len(originalModels) != 1 {
		return nil, fmt.Errorf("cases.json: the original answers come from %d models; the bench treated them as one model's", len(originalModels))
	}
	var originalModel string
	for model := range originalModels {
		originalModel = model
	}

	prices, err := catalog.Models(ctx)
	if err != nil {
		return nil, err
	}
	pricesNote := fmt.Sprintf("list prices read from model-store at import on %s, not on the days the bench ran (2026-09-30 to 2026-10-08)",
		s.now().UTC().Format("2006-01-02"))

	result := &LegacyImportResult{Suites: []string{}, Runs: []LegacyImportRun{}}
	err = s.inTransaction(func(tx *sql.Tx) error {
		for _, c := range cases {
			var existing string
			err := tx.QueryRow("SELECT id FROM cases WHERE source_kind = ? AND source_ref = ?", SourceKindClaudeCodeTranscript, c.ID).Scan(&existing)
			if err == nil {
				return conflictf("the bench is already imported: its case %s is %s", c.ID, existing)
			}
			if err != sql.ErrNoRows {
				return err
			}
		}
		now := s.unixNow()
		for _, task := range legacyTasks() {
			taskCases := casesByCommand[task.command]
			if len(taskCases) == 0 {
				continue
			}
			suite := &Suite{Name: task.suiteName, Description: "Imported from the Python model-comparison bench of 2026-09-30..10-08.",
				TaskKind: task.taskKind, JudgeInstructions: task.judgeInstructions, JudgeFailureKinds: task.failureKinds,
				OutputSchema: schemas[task.command].schema, MaxTokens: schemas[task.command].maxTokens, JudgeMaxTokens: legacyJudgeMaxTokens,
				CheckerName: task.checkerName, BaselineModelID: originalModel,
				WeeklyCallsSource: WeeklyCallsSource{Kind: WeeklyCallsSourceFixed, CallsPerWeek: task.callsPerWeek,
					Note: "the bench's analyze.py WEEK figure, from scheduler's run history in the week before 2026-09-30"},
				CostModel: task.costModel}
			if err := suite.validate(); err != nil {
				return err
			}
			suiteID, err := insertWithID(tx, "suites", suiteIDPrefix,
				`name, description, task_kind, judge_instructions, judge_failure_kinds, output_schema, max_tokens, judge_max_tokens,
				 checker_name, baseline_model_id, weekly_calls_source, cost_model, created_at, updated_at`,
				"?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?",
				suite.Name, suite.Description, suite.TaskKind, suite.JudgeInstructions, mustJSON(suite.JudgeFailureKinds), string(suite.OutputSchema),
				suite.MaxTokens, suite.JudgeMaxTokens, suite.CheckerName, suite.BaselineModelID, mustJSON(suite.WeeklyCallsSource), mustJSON(suite.CostModel), now, now)
			if err != nil {
				return err
			}
			suite.ID = suiteID
			result.Suites = append(result.Suites, suiteID)

			caseIDs := make([]string, 0, len(taskCases))
			caseIDByLegacyID := map[string]string{}
			for _, legacy := range taskCases {
				c, err := legacy.toCase(originalModel)
				if err != nil {
					return err
				}
				id, err := insertCase(tx, suite, c, now)
				if err != nil {
					return err
				}
				caseIDs = append(caseIDs, id)
				caseIDByLegacyID[legacy.ID] = id
				result.Cases++
			}

			for _, judging := range judgings {
				if !judging.candidates[originalModel] {
					return fmt.Errorf("%s-* did not judge the original answers (%s); the bench always did", judging.prefix, originalModel)
				}
				candidates := judging.candidateOrder(originalModel, judgings, replayOrder)
				run := &Run{SuiteID: suiteID, CandidateModelIDs: candidates, JudgeModelIDs: judging.judges, OriginalAnswerModelID: originalModel,
					CaseSelection: CaseSelection{Mode: CaseSelectionAll}, CaseIDs: caseIDs, Status: RunStatusCompleted,
					BudgetLimits: []BudgetLimit{}, ModelPrices: map[string]ModelPrice{}, PricesNote: pricesNote, Origin: RunOriginLegacyImport,
					Note: fmt.Sprintf("Rebuilt from the bench's %s-*.jsonl files: %d candidates, judged by %s.", judging.prefix, len(candidates),
						strings.Join(judging.judges, " and ")),
					StartedAt: now, FinishedAt: now}
				for _, model := range append(append([]string{}, candidates...), judging.judges...) {
					price, known := prices[model]
					if !known {
						return fmt.Errorf("model-store does not know %s, which the bench used", model)
					}
					run.ModelPrices[model] = price
				}
				runID, err := s.insertRun(tx, run)
				if err != nil {
					return err
				}
				result.Runs = append(result.Runs, LegacyImportRun{RunID: runID, SuiteID: suiteID, Judging: judging.prefix, Candidates: candidates, Judges: judging.judges})

				for i, legacy := range taskCases {
					c, err := legacy.toCase(originalModel)
					if err != nil {
						return err
					}
					c.ID = caseIDs[i]
					if err := s.saveAnswerIn(tx, originalAnswerOf(runID, originalModel, c)); err != nil {
						return err
					}
					for _, model := range candidates {
						if model == originalModel {
							continue
						}
						if err := s.saveAnswerIn(tx, legacyAnswer(runID, caseIDs[i], model, replays[answerKey{legacy.ID, model}])); err != nil {
							return err
						}
					}
				}
				for _, judge := range judging.judges {
					for _, row := range judging.rows[judge] {
						if row.Command != task.command {
							continue
						}
						caseID, known := caseIDByLegacyID[row.ID]
						if !known {
							return fmt.Errorf("%s-%s.jsonl judges case %s, which cases.json does not have", judging.prefix, judge, row.ID)
						}
						verdict := &Verdict{RunID: runID, CaseID: caseID, JudgeModelID: judge, JudgeProvider: prices[judge].Provider, Attempt: 1,
							LabelMapping: row.Mapping, Parsed: row.Parsed, Error: row.Error, DurationMs: row.DurationMs}
						if row.Error == "" && (len(row.Parsed) == 0 || string(row.Parsed) == "null") {
							verdict.Error = "the bench recorded no parsed verdict"
						}
						if row.Usage != nil {
							verdict.InputTokens = row.Usage.InputTokens + row.Usage.CacheReadTokens + row.Usage.CacheWriteTokens
							verdict.OutputTokens = row.Usage.OutputTokens
						}
						if err := s.saveVerdictIn(tx, verdict); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, run := range result.Runs {
		if err := s.CheckRun(run.RunID); err != nil {
			return nil, fmt.Errorf("check %s: %w", run.RunID, err)
		}
	}
	return result, nil
}

func legacyAnswer(runID, caseID, model string, row legacyReplay) *Answer {
	a := &Answer{RunID: runID, CaseID: caseID, ModelID: model, Origin: AnswerOriginLegacyImport, Attempt: 1, AnsweredBy: row.Model,
		DurationMs: row.DurationMs, Error: row.Error}
	if row.ModelReq == "" {
		a.Error = "the bench has no replay of this case by this model"
		return a
	}
	if len(row.Parsed) > 0 && string(row.Parsed) != "null" {
		a.Parsed = row.Parsed
	} else if a.Error == "" {
		a.Error = "the bench recorded no parsed answer"
	}
	if row.Usage != nil {
		a.InputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.OutputTokens = row.Usage.InputTokens, row.Usage.CacheReadTokens,
			row.Usage.CacheWriteTokens, row.Usage.OutputTokens
		a.UsageKnown = true
	}
	return a
}

func (c legacyCase) toCase(originalModel string) (*Case, error) {
	result := &Case{SourceKind: SourceKindClaudeCodeTranscript, SourceRef: c.ID, SystemPrompt: c.System, UserPrompt: c.Prompt,
		OriginalModelID: originalModel, OriginalUsageKnown: true, OriginalInputTokens: c.TIn, OriginalCacheReadTokens: c.TCR,
		OriginalCacheWriteTokens: c.TCW, OriginalOutputTokens: c.TOut}
	if len(c.Answer) > 0 && string(c.Answer) != "null" {
		result.OriginalAnswer = c.Answer
	}
	if c.TS0 != "" {
		start, err := time.Parse(time.RFC3339Nano, c.TS0)
		if err != nil {
			return nil, fmt.Errorf("case %s: ts0: %w", c.ID, err)
		}
		result.OriginalAt = start.Unix()
		if c.TS1 != "" {
			end, err := time.Parse(time.RFC3339Nano, c.TS1)
			if err != nil {
				return nil, fmt.Errorf("case %s: ts1: %w", c.ID, err)
			}
			milliseconds := int64(math.Round(float64(end.Sub(start).Microseconds()) / 1000))
			result.OriginalDurationMs = &milliseconds
		}
	}
	return result, nil
}

// legacyModelID reads the model a transcript named: a model-store id, or
// the transcript's model attachment, whose identity carries it.
func legacyModelID(raw json.RawMessage) (string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil && id != "" {
		return id, nil
	}
	var attachment struct {
		Identity struct {
			ModelID string `json:"modelId"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(raw, &attachment); err != nil || attachment.Identity.ModelID == "" {
		return "", fmt.Errorf("model %s names no model id", string(raw))
	}
	return attachment.Identity.ModelID, nil
}

// legacyJudging is one prefix's judge files: the same candidates, judged by
// each judge.
type legacyJudging struct {
	prefix     string
	judges     []string
	candidates map[string]bool
	rows       map[string][]legacyJudgment
}

func readLegacyJudgings(dir string) ([]*legacyJudging, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byPrefix := map[string]*legacyJudging{}
	for _, entry := range entries {
		match := legacyJudgeFile.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		prefix, judge := match[1], match[2]
		judging := byPrefix[prefix]
		if judging == nil {
			judging = &legacyJudging{prefix: prefix, rows: map[string][]legacyJudgment{}}
			byPrefix[prefix] = judging
		}
		rows, err := readJSONLines[legacyJudgment](filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, row := range rows {
			if seen[row.ID] {
				return nil, fmt.Errorf("%s judges case %s twice", entry.Name(), row.ID)
			}
			seen[row.ID] = true
			candidates := map[string]bool{}
			for _, model := range row.Mapping {
				candidates[model] = true
			}
			if judging.candidates == nil {
				judging.candidates = candidates
			} else if !sameKeys(judging.candidates, candidates) {
				return nil, fmt.Errorf("%s: case %s was judged on other candidates than the rest of %s-*", entry.Name(), row.ID, prefix)
			}
		}
		judging.judges = append(judging.judges, judge)
		judging.rows[judge] = rows
	}
	judgings := make([]*legacyJudging, 0, len(byPrefix))
	for _, judging := range byPrefix {
		sort.Strings(judging.judges)
		judgings = append(judgings, judging)
	}
	sort.Slice(judgings, func(i, j int) bool { return len(judgings[i].candidates) < len(judgings[j].candidates) })
	if len(judgings) == 0 {
		return nil, fmt.Errorf("%s holds no judge*-<model>.jsonl files", dir)
	}
	return judgings, nil
}

// candidateOrder lists a judging's candidates the way the bench listed them:
// the original model first, then each model in the judging where it first
// appeared, smallest judging first, and within one judging in the order the
// replay files first name it.
func (judging *legacyJudging) candidateOrder(originalModel string, all []*legacyJudging, replayOrder []string) []string {
	order := []string{originalModel}
	for _, earlier := range all {
		var added []string
		for model := range earlier.candidates {
			if judging.candidates[model] && !slices.Contains(order, model) {
				added = append(added, model)
			}
		}
		sort.Slice(added, func(i, j int) bool { return orderIndex(replayOrder, added[i]) < orderIndex(replayOrder, added[j]) })
		order = append(order, added...)
	}
	return order
}

func orderIndex(order []string, value string) int {
	if index := slices.Index(order, value); index >= 0 {
		return index
	}
	return len(order)
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readJSONLines[T any](path string) ([]T, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var rows []T
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var row T
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}
