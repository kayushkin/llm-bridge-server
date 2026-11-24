package evalstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// RunDefaults are what a run gets when it names nothing, and the settings
// every run shares.
type RunDefaults struct {
	Concurrency           int
	BudgetLimits          []BudgetLimit
	BudgetCheckEveryCalls int
	AnswerTimeout         time.Duration
	JudgeTimeout          time.Duration
	OrganizationID        string
}

// Runner creates runs and carries them out: candidates answer every case,
// the suite's checker checks the answers, and every judge ranks every case's
// answers. It holds the runs going in this process.
type Runner struct {
	store    *Store
	caller   ModelCaller
	catalog  ModelCatalog
	usage    UsageReader
	defaults RunDefaults

	mutex   sync.Mutex
	going   map[string]*goingRun
	waiting sync.WaitGroup
	// shutdown ends every run when the process stops.
	shutdown       context.Context
	cancelShutdown context.CancelFunc
}

type goingRun struct {
	stopRequested bool
}

// NewRunner builds a runner.
func NewRunner(store *Store, caller ModelCaller, catalog ModelCatalog, usage UsageReader, defaults RunDefaults) *Runner {
	shutdown, cancel := context.WithCancel(context.Background())
	return &Runner{store: store, caller: caller, catalog: catalog, usage: usage, defaults: defaults,
		going: map[string]*goingRun{}, shutdown: shutdown, cancelShutdown: cancel}
}

// Close ends every going run without canceling the calls in flight on the
// bridge: they finish there, and a resumed run collects them by their
// idempotency keys. The runs are left interrupted.
func (r *Runner) Close() {
	r.cancelShutdown()
	r.waiting.Wait()
}

// Wait blocks until no run is going. Tests use it.
func (r *Runner) Wait() { r.waiting.Wait() }

// CreateRun validates a new run, freezes its cases and every model's price
// from model-store, and stores it as created.
func (r *Runner) CreateRun(ctx context.Context, request NewRun) (*Run, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	suite, err := r.store.GetSuite(request.SuiteID)
	if err != nil {
		return nil, err
	}
	if len(request.JudgeModelIDs) == 0 {
		return nil, invalidf("run: judge_model_ids needs at least one judge")
	}
	all, err := r.store.ListCases(suite.ID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, invalidf("suite %s has no cases", suite.ID)
	}
	caseIDs, err := selectCases(all, request.CaseSelection)
	if err != nil {
		return nil, err
	}
	catalog, err := r.catalog.Models(ctx)
	if err != nil {
		return nil, err
	}
	prices := map[string]ModelPrice{}
	var unknown []string
	for _, id := range append(append([]string{}, request.CandidateModelIDs...), request.JudgeModelIDs...) {
		price, ok := catalog[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		prices[id] = price
	}
	if len(unknown) > 0 {
		return nil, invalidf("model-store does not know %v; a run names models by model-store id", unknown)
	}
	limits := request.BudgetLimits
	if len(limits) == 0 {
		limits = r.defaults.BudgetLimits
	}
	concurrency := request.Concurrency
	if concurrency == 0 {
		concurrency = r.defaults.Concurrency
	}
	run := &Run{SuiteID: suite.ID, CandidateModelIDs: request.CandidateModelIDs, JudgeModelIDs: request.JudgeModelIDs,
		OriginalAnswerModelID: request.OriginalAnswerModelID, CaseSelection: request.CaseSelection, CaseIDs: caseIDs, Status: RunStatusCreated,
		BudgetLimits: limits, Concurrency: concurrency, OrganizationID: request.OrganizationID, ModelPrices: prices,
		PricesNote: "list prices read from model-store when the run was created", PredictionID: request.PredictionID, Note: request.Note,
		Origin: RunOriginLive}
	var id string
	if err := r.store.inTransaction(func(tx *sql.Tx) error {
		var err error
		id, err = r.store.insertRun(tx, run)
		return err
	}); err != nil {
		return nil, err
	}
	return r.store.GetRun(id)
}

// StartRun starts a created run, or resumes one that stopped: only answers
// and verdicts that are missing are made. retryFailed also makes again the
// ones that failed, under a new idempotency key.
func (r *Runner) StartRun(id string, retryFailed bool) (*Run, error) {
	run, err := r.store.GetRun(id)
	if err != nil {
		return nil, err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, going := r.going[id]; going {
		return nil, conflictf("run %s is already going", id)
	}
	if run.Status == RunStatusRunning || run.Status == RunStatusStopping {
		return nil, conflictf("run %s is %s in another process", id, run.Status)
	}
	organizationID := run.OrganizationID
	if organizationID == "" {
		organizationID = r.defaults.OrganizationID
	}
	if organizationID == "" {
		return nil, invalidf("run %s names no organization and EVAL_STORE_ORGANIZATION_ID is not set: the bridge needs a principal-store group to bill", id)
	}
	if err := r.store.markRunStarted(id, organizationID); err != nil {
		return nil, err
	}
	state := &goingRun{}
	r.going[id] = state
	r.waiting.Add(1)
	go func() {
		defer r.waiting.Done()
		status, reason := r.execute(r.shutdown, id, retryFailed, state)
		r.mutex.Lock()
		delete(r.going, id)
		r.mutex.Unlock()
		if err := r.store.setRunStatus(id, status, reason); err != nil {
			log.Printf("run %s: record status %s: %v", id, status, err)
		}
		log.Printf("run %s: %s %s", id, status, reason)
	}()
	return r.store.GetRun(id)
}

// StopRun asks a going run to stop after the calls in flight. It ends
// stopped, and can be resumed.
func (r *Runner) StopRun(id string) (*Run, error) {
	r.mutex.Lock()
	state, going := r.going[id]
	if going {
		state.stopRequested = true
	}
	r.mutex.Unlock()
	if !going {
		return nil, conflictf("run %s is not going in this process", id)
	}
	if err := r.store.setRunStatus(id, RunStatusStopping, "stop asked for; finishing the calls in flight"); err != nil {
		return nil, err
	}
	return r.store.GetRun(id)
}

func (r *Runner) stopRequested(id string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	state, going := r.going[id]
	return going && state.stopRequested
}

// budgetErrorPrefix marks a failure the bridge gave for money. A resume
// retries those without being asked: they say nothing about the model.
const budgetErrorPrefix = "budget: "

// call is one model call the run still has to make.
type call struct {
	caseID string
	// modelID is the candidate, or the judge for a verdict.
	modelID string
	attempt int
	judge   bool
}

// execute carries a run out and returns the status it ends in.
func (r *Runner) execute(ctx context.Context, id string, retryFailed bool, state *goingRun) (string, string) {
	run, err := r.store.GetRun(id)
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	suite, err := r.store.GetSuite(run.SuiteID)
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	cases, err := r.store.casesWithBodies(run.CaseIDs)
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	caseByID := map[string]*Case{}
	for _, c := range cases {
		caseByID[c.ID] = c
	}
	if err := r.copyOriginalAnswers(run, cases); err != nil {
		return RunStatusFailed, err.Error()
	}

	answers, err := r.store.answersByKey(id)
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	var answerCalls []call
	for _, c := range cases {
		for _, model := range run.CandidateModelIDs {
			if model == run.OriginalAnswerModelID {
				continue
			}
			existing := answers[answerKey{c.ID, model}]
			switch {
			case existing == nil:
				answerCalls = append(answerCalls, call{caseID: c.ID, modelID: model, attempt: 1})
			case existing.Error != "" && (retryFailed || strings.HasPrefix(existing.Error, budgetErrorPrefix)):
				answerCalls = append(answerCalls, call{caseID: c.ID, modelID: model, attempt: existing.Attempt + 1})
			}
		}
	}
	if status, reason, done := r.makeCalls(ctx, run, suite, caseByID, answerCalls); done {
		return status, reason
	}

	if suite.CheckerName != "" {
		if err := r.store.CheckRun(id); err != nil {
			return RunStatusFailed, err.Error()
		}
	}

	answers, err = r.store.answersByKey(id)
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	verdicts, err := r.store.ListVerdicts(id, VerdictFilter{})
	if err != nil {
		return RunStatusFailed, err.Error()
	}
	verdictByKey := map[answerKey]*Verdict{}
	for _, v := range verdicts {
		verdictByKey[answerKey{v.CaseID, v.JudgeModelID}] = v
	}
	var judgeCalls []call
	for _, c := range cases {
		for _, judge := range run.JudgeModelIDs {
			existing := verdictByKey[answerKey{c.ID, judge}]
			switch {
			case existing == nil:
				judgeCalls = append(judgeCalls, call{caseID: c.ID, modelID: judge, attempt: 1, judge: true})
			case (existing.Error != "" || !existing.Valid) && (retryFailed || strings.HasPrefix(existing.Error, budgetErrorPrefix)):
				judgeCalls = append(judgeCalls, call{caseID: c.ID, modelID: judge, attempt: existing.Attempt + 1, judge: true})
			}
		}
	}
	if status, reason, done := r.makeCalls(ctx, run, suite, caseByID, judgeCalls); done {
		return status, reason
	}
	return RunStatusCompleted, r.completionSummary(id)
}

// copyOriginalAnswers writes the cases' original answers as the answers of
// the run's original-answer model, once.
func (r *Runner) copyOriginalAnswers(run *Run, cases []*Case) error {
	if run.OriginalAnswerModelID == "" {
		return nil
	}
	existing, err := r.store.answersByKey(run.ID)
	if err != nil {
		return err
	}
	for _, c := range cases {
		if existing[answerKey{c.ID, run.OriginalAnswerModelID}] != nil {
			continue
		}
		if err := r.store.saveAnswer(originalAnswerOf(run.ID, run.OriginalAnswerModelID, c)); err != nil {
			return err
		}
	}
	return nil
}

func originalAnswerOf(runID, modelID string, c *Case) *Answer {
	a := &Answer{RunID: runID, CaseID: c.ID, ModelID: modelID, Origin: AnswerOriginCaseOriginal, Attempt: 1, Parsed: c.OriginalAnswer,
		InputTokens: c.OriginalInputTokens, CacheReadTokens: c.OriginalCacheReadTokens, CacheWriteTokens: c.OriginalCacheWriteTokens,
		OutputTokens: c.OriginalOutputTokens, UsageKnown: c.OriginalUsageKnown, DurationMs: c.OriginalDurationMs, AnsweredBy: c.OriginalModelID}
	if len(c.OriginalAnswer) == 0 {
		a.Error = "the case has no original answer"
	}
	return a
}

// makeCalls makes the calls in batches, reading the budget before each.
// done is true when the run must end here, with status and reason.
func (r *Runner) makeCalls(ctx context.Context, run *Run, suite *Suite, caseByID map[string]*Case, calls []call) (status, reason string, done bool) {
	batchSize := r.defaults.BudgetCheckEveryCalls
	if batchSize <= 0 {
		batchSize = 1
	}
	for start := 0; start < len(calls); start += batchSize {
		if ctx.Err() != nil {
			return RunStatusInterrupted, "the service stopped while the run was going; resume it", true
		}
		if r.stopRequested(run.ID) {
			return RunStatusStopped, "stopped on request; resume to make the missing calls", true
		}
		limits, err := r.usage.Limits(ctx)
		if err != nil {
			return RunStatusStoppedBudget, "could not read usage, so the budget cannot be checked: " + err.Error(), true
		}
		if verdict := budgetVerdict(limits, run.BudgetLimits, r.store.now()); verdict != "" {
			return RunStatusStoppedBudget, verdict, true
		}
		batch := calls[start:min(start+batchSize, len(calls))]
		budgetRefusals, err := r.makeBatch(ctx, run, suite, caseByID, batch)
		if err != nil {
			return RunStatusFailed, err.Error(), true
		}
		if budgetRefusals != "" {
			return RunStatusStoppedBudget, "llm-bridge-server refused for budget: " + budgetRefusals, true
		}
	}
	return "", "", false
}

// makeBatch makes a batch of calls, at most the run's concurrency at once,
// and records each outcome. It returns the bridge's budget refusals, and an
// error only when an outcome could not be recorded.
func (r *Runner) makeBatch(ctx context.Context, run *Run, suite *Suite, caseByID map[string]*Case, batch []call) (string, error) {
	concurrency := max(run.Concurrency, 1)
	slots := make(chan struct{}, concurrency)
	var group sync.WaitGroup
	var mutex sync.Mutex
	var refusals []string
	var recordErrors []error
	for _, next := range batch {
		group.Add(1)
		slots <- struct{}{}
		go func(c call) {
			defer group.Done()
			defer func() { <-slots }()
			refused, err := r.makeCall(ctx, run, suite, caseByID[c.caseID], c)
			mutex.Lock()
			defer mutex.Unlock()
			if refused != "" {
				refusals = append(refusals, refused)
			}
			if err != nil {
				recordErrors = append(recordErrors, err)
			}
		}(next)
	}
	group.Wait()
	return strings.Join(refusals, "; "), errors.Join(recordErrors...)
}

// idempotencyKey names one call to the bridge. The attempt is part of it so
// that a retry after a failure is a new operation; everything else is fixed,
// so a resumed run sends the same key and is given the first receipt.
func idempotencyKey(runID, caseID, modelID string, judge bool, attempt int) string {
	kind := "answer"
	if judge {
		kind = "judge"
	}
	return fmt.Sprintf("eval-store:%s:%s:%s:%s:%d", runID, caseID, kind, modelID, attempt)
}

// makeCall makes one call and records it. refused is set when the bridge
// refused for budget; the error is a failure to record.
func (r *Runner) makeCall(ctx context.Context, run *Run, suite *Suite, c *Case, next call) (refused string, err error) {
	request := CompletionRequest{OrganizationID: run.OrganizationID, CorrelationID: run.ID, Model: next.modelID,
		IdempotencyKey: idempotencyKey(run.ID, c.ID, next.modelID, next.judge, next.attempt)}
	var mapping map[string]string
	if next.judge {
		answers, err := r.store.ListAnswers(run.ID, AnswerFilter{CaseID: c.ID})
		if err != nil {
			return "", err
		}
		byModel := map[string]*Answer{}
		for _, a := range answers {
			byModel[a.ModelID] = a
		}
		mapping = BlindLabelMapping(c.ID, run.CandidateModelIDs)
		prompt, err := judgeUserPrompt(suite, c, mapping, byModel)
		if err != nil {
			return "", err
		}
		request.SystemPrompt, request.Prompt = judgeSystemPrompt(len(mapping)), prompt
		request.Schema = judgeSchema(len(mapping), suite.JudgeFailureKinds)
		request.MaxTokens, request.Timeout = suite.JudgeMaxTokens, r.defaults.JudgeTimeout
	} else {
		request.SystemPrompt, request.Prompt, request.Schema = c.SystemPrompt, c.UserPrompt, suite.OutputSchema
		request.MaxTokens, request.Timeout = suite.MaxTokens, r.defaults.AnswerTimeout
	}

	outcome, callError := r.caller.Complete(ctx, request)
	if callError != nil && ctx.Err() != nil {
		// The process is stopping. The call goes on at the bridge; the
		// resumed run collects it under the same key.
		return "", nil
	}
	if callError != nil && errors.Is(callError, ErrBridgeBudgetRefused) {
		refused = fmt.Sprintf("%s on %s: %v", next.modelID, c.ID, callError)
		if outcome.OperationID == "" {
			// Refused before it was accepted: nothing to record, and the same
			// key is free for the resume.
			return refused, nil
		}
	}
	errorText := trimmedError(callError)
	if refused != "" {
		errorText = budgetErrorPrefix + errorText
	}
	if callError == nil && outcome.AnsweredBy != "" && outcome.AnsweredBy != next.modelID {
		errorText = fmt.Sprintf("asked for %s and %s answered", next.modelID, outcome.AnsweredBy)
	}
	if next.judge {
		verdict := &Verdict{RunID: run.ID, CaseID: c.ID, JudgeModelID: next.modelID, JudgeProvider: run.ModelPrices[next.modelID].Provider,
			Attempt: next.attempt, LabelMapping: mapping, Parsed: outcome.Parsed, Error: errorText,
			InputTokens: outcome.InputTokens + outcome.CacheReadTokens + outcome.CacheWriteTokens, OutputTokens: outcome.OutputTokens,
			DurationMs: outcome.DurationMs, OperationID: outcome.OperationID}
		return refused, r.store.saveVerdict(verdict)
	}
	answer := &Answer{RunID: run.ID, CaseID: c.ID, ModelID: next.modelID, Origin: AnswerOriginReplayed, Attempt: next.attempt,
		Parsed: outcome.Parsed, Error: errorText, InputTokens: outcome.InputTokens, CacheReadTokens: outcome.CacheReadTokens,
		CacheWriteTokens: outcome.CacheWriteTokens, OutputTokens: outcome.OutputTokens, UsageKnown: outcome.UsageKnown,
		DurationMs: outcome.DurationMs, OperationID: outcome.OperationID, AnsweredBy: outcome.AnsweredBy}
	return refused, r.store.saveAnswer(answer)
}

func (r *Runner) completionSummary(runID string) string {
	progress, err := r.store.RunProgress(runID)
	if err != nil {
		return err.Error()
	}
	if progress.AnswerErrors == 0 && progress.VerdictErrors == 0 && progress.VerdictsMalformed == 0 {
		return ""
	}
	return fmt.Sprintf("%d answers failed, %d verdicts failed, %d verdicts malformed; resume with retry_failed to make them again",
		progress.AnswerErrors, progress.VerdictErrors, progress.VerdictsMalformed)
}

// CheckRun runs the suite's checker over every answer of a run, replacing
// earlier results.
func (s *Store) CheckRun(runID string) error {
	run, err := s.GetRun(runID)
	if err != nil {
		return err
	}
	suite, err := s.GetSuite(run.SuiteID)
	if err != nil {
		return err
	}
	if suite.CheckerName == "" {
		return invalidf("suite %s names no checker", suite.ID)
	}
	checker := checkers[suite.CheckerName]
	answers, err := s.ListAnswers(runID, AnswerFilter{})
	if err != nil {
		return err
	}
	caseByID := map[string]*Case{}
	var results []CheckResult
	for _, a := range answers {
		c := caseByID[a.CaseID]
		if c == nil {
			if c, err = s.GetCase(a.CaseID); err != nil {
				return err
			}
			caseByID[a.CaseID] = c
		}
		for _, result := range runChecker(checker, c, a) {
			result.RunID, result.CaseID, result.ModelID = runID, a.CaseID, a.ModelID
			results = append(results, result)
		}
	}
	return s.replaceChecks(runID, results)
}

// RunProgress counts what a run has and what it still needs.
type RunProgress struct {
	Cases             int `json:"cases"`
	AnswersExpected   int `json:"answers_expected"`
	AnswersRecorded   int `json:"answers_recorded"`
	AnswerErrors      int `json:"answer_errors"`
	VerdictsExpected  int `json:"verdicts_expected"`
	VerdictsRecorded  int `json:"verdicts_recorded"`
	VerdictErrors     int `json:"verdict_errors"`
	VerdictsMalformed int `json:"verdicts_malformed"`
}

// RunProgress counts a run's answers and verdicts.
func (s *Store) RunProgress(runID string) (RunProgress, error) {
	run, err := s.GetRun(runID)
	if err != nil {
		return RunProgress{}, err
	}
	progress := RunProgress{Cases: len(run.CaseIDs), AnswersExpected: len(run.CaseIDs) * len(run.CandidateModelIDs),
		VerdictsExpected: len(run.CaseIDs) * len(run.JudgeModelIDs)}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(error != ''), 0) FROM answers WHERE run_id = ?`, runID).
		Scan(&progress.AnswersRecorded, &progress.AnswerErrors); err != nil {
		return RunProgress{}, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(error != ''), 0), COALESCE(SUM(error = '' AND valid = 0), 0) FROM verdicts WHERE run_id = ?`, runID).
		Scan(&progress.VerdictsRecorded, &progress.VerdictErrors, &progress.VerdictsMalformed); err != nil {
		return RunProgress{}, err
	}
	return progress, nil
}
