package evalstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTestRunner(store *Store, caller ModelCaller, usage UsageReader) *Runner {
	return NewRunner(store, caller, syntheticCatalog(), usage, RunDefaults{Concurrency: 2, BudgetLimits: syntheticLimits, BudgetCheckEveryCalls: 2,
		AnswerTimeout: time.Minute, JudgeTimeout: time.Minute, OrganizationID: "principal_000900"})
}

// threeCaseRun makes a suite of three synthetic cases and a run of two
// replayed candidates plus the original, judged by two judges.
func threeCaseRun(t *testing.T, store *Store, runner *Runner) *Run {
	t.Helper()
	suite := emailSuite(t, store)
	for _, ref := range []string{"one", "two", "three"} {
		addEmailCase(t, store, suite, ref)
	}
	run, err := runner.CreateRun(context.Background(), NewRun{SuiteID: suite.ID, CandidateModelIDs: []string{"model-original", "model-small", "model-large"},
		JudgeModelIDs: []string{"judge-a", "judge-b"}, OriginalAnswerModelID: "model-original", CaseSelection: CaseSelection{Mode: CaseSelectionAll}})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func startAndWait(t *testing.T, runner *Runner, id string, retryFailed bool) *Run {
	t.Helper()
	if _, err := runner.StartRun(id, retryFailed); err != nil {
		t.Fatal(err)
	}
	runner.Wait()
	run, err := runner.store.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func distinct(keys []string) map[string]int {
	counts := map[string]int{}
	for _, key := range keys {
		counts[key]++
	}
	return counts
}

func TestARunAnswersChecksAndJudgesEveryCaseOnce(t *testing.T) {
	store := openTestStore(t)
	caller := newFakeCaller()
	runner := newTestRunner(store, caller, &fakeUsage{})
	run := threeCaseRun(t, store, runner)
	if run.Status != RunStatusCreated || len(run.CaseIDs) != 3 || run.ModelPrices["judge-b"].Provider != "openai" {
		t.Fatalf("created run %+v", run)
	}
	run = startAndWait(t, runner, run.ID, false)
	if run.Status != RunStatusCompleted || run.StatusReason != "" {
		t.Fatalf("run ended %s: %s", run.Status, run.StatusReason)
	}
	if run.OrganizationID != "principal_000900" {
		t.Errorf("run did not record who it spent for: %+v", run)
	}
	// 3 cases × 2 replayed candidates, then 3 cases × 2 judges.
	keys := caller.keys()
	if len(keys) != 12 || len(distinct(keys)) != 12 {
		t.Fatalf("%d calls with %d distinct keys, want 12 and 12: %v", len(keys), len(distinct(keys)), keys)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "eval-store:"+run.ID+":eval_case_") || !strings.HasSuffix(key, ":1") {
			t.Errorf("idempotency key %s does not name run, case, model and attempt", key)
		}
	}
	for _, request := range caller.calls {
		if request.Model == "model-original" {
			t.Error("the original answers were replayed")
		}
		if request.CorrelationID != run.ID || request.OrganizationID != "principal_000900" {
			t.Errorf("request %+v", request)
		}
	}
	answers, _ := store.ListAnswers(run.ID, AnswerFilter{})
	origins := map[string]int{}
	for _, a := range answers {
		origins[a.Origin]++
	}
	if len(answers) != 9 || origins[AnswerOriginCaseOriginal] != 3 || origins[AnswerOriginReplayed] != 6 {
		t.Errorf("answers by origin %v", origins)
	}
	verdicts, _ := store.ListVerdicts(run.ID, VerdictFilter{})
	for _, v := range verdicts {
		if !v.Valid || len(v.Candidates) != 3 || v.LabelMapping == nil {
			t.Errorf("verdict %+v", v)
		}
		if want := BlindLabelMapping(v.CaseID, run.CandidateModelIDs); !maps(want, v.LabelMapping) {
			t.Errorf("verdict mapping %v is not the case's blind shuffle %v", v.LabelMapping, want)
		}
	}
	checks, _ := store.ListChecks(run.ID)
	if len(checks) == 0 {
		t.Error("no checks after the answers")
	}
	for _, c := range checks {
		if !c.Passed && c.CheckName != CheckNewCards && c.CheckName != CheckCardUpdates {
			t.Errorf("check failed on a faithful filing: %+v", c)
		}
	}

	// Resuming a complete run makes no call at all.
	startAndWait(t, runner, run.ID, false)
	if len(caller.keys()) != 12 {
		t.Errorf("resuming a complete run made %d new calls", len(caller.keys())-12)
	}
}

func TestResumeRetriesAFailedAnswerOnlyWhenAskedAndUnderANewKey(t *testing.T) {
	store := openTestStore(t)
	caller := newFakeCaller()
	caller.failures["model-large"] = errors.New("operation ended failed: model_call_failed")
	runner := newTestRunner(store, caller, &fakeUsage{})
	run := threeCaseRun(t, store, runner)
	run = startAndWait(t, runner, run.ID, false)
	if run.Status != RunStatusCompleted || !strings.Contains(run.StatusReason, "3 answers failed") {
		t.Fatalf("run ended %s: %s", run.Status, run.StatusReason)
	}
	afterFirst := len(caller.keys())

	startAndWait(t, runner, run.ID, false)
	if len(caller.keys()) != afterFirst {
		t.Fatalf("a plain resume made %d calls; failed answers are retried only when asked", len(caller.keys())-afterFirst)
	}

	delete(caller.failures, "model-large")
	run = startAndWait(t, runner, run.ID, true)
	retried := caller.keys()[afterFirst:]
	answerRetries := 0
	for _, key := range retried {
		if strings.Contains(key, ":answer:model-large:") {
			answerRetries++
			if !strings.HasSuffix(key, ":2") {
				t.Errorf("retry key %s reuses the failed attempt's key", key)
			}
		}
	}
	if answerRetries != 3 {
		t.Errorf("%d answers retried, want 3: %v", answerRetries, retried)
	}
	answers, _ := store.ListAnswers(run.ID, AnswerFilter{ModelID: "model-large"})
	for _, a := range answers {
		if a.Error != "" || a.Attempt != 2 || !a.HasAnswer() {
			t.Errorf("retried answer %+v", a)
		}
	}
}

func TestABudgetStopIsResumableAndNothingIsPaidForTwice(t *testing.T) {
	store := openTestStore(t)
	caller := newFakeCaller()
	// Readings before each batch of two calls: fine, fine, then over the
	// 40% anthropic limit.
	usage := &fakeUsage{readings: []float64{10, 20, 45}}
	runner := newTestRunner(store, caller, usage)
	run := threeCaseRun(t, store, runner)
	run = startAndWait(t, runner, run.ID, false)
	if run.Status != RunStatusStoppedBudget || !strings.Contains(run.StatusReason, "anthropic seven_day is at 45%") {
		t.Fatalf("run ended %s: %s", run.Status, run.StatusReason)
	}
	if made := len(caller.keys()); made != 4 {
		t.Fatalf("%d calls before the stop, want two batches of two", made)
	}

	usage.mutex.Lock()
	usage.readings, usage.reads = []float64{30}, 0
	usage.mutex.Unlock()
	run = startAndWait(t, runner, run.ID, false)
	if run.Status != RunStatusCompleted {
		t.Fatalf("resumed run ended %s: %s", run.Status, run.StatusReason)
	}
	for key, count := range distinct(caller.keys()) {
		if count != 1 {
			t.Errorf("%s was sent %d times", key, count)
		}
	}
	if len(caller.keys()) != 12 {
		t.Errorf("%d calls in all, want 12", len(caller.keys()))
	}
}

func TestAStaleUsageReadingStopsTheRunBeforeAnyCall(t *testing.T) {
	store := openTestStore(t)
	caller := newFakeCaller()
	runner := newTestRunner(store, caller, &fakeUsage{stale: true})
	run := startAndWait(t, runner, threeCaseRun(t, store, runner).ID, false)
	if run.Status != RunStatusStoppedBudget || !strings.Contains(run.StatusReason, "stale") || len(caller.keys()) != 0 {
		t.Errorf("run ended %s (%s) after %d calls", run.Status, run.StatusReason, len(caller.keys()))
	}
}

func TestABridgeBudgetRefusalStopsTheRunAndLeavesTheKeyFree(t *testing.T) {
	store := openTestStore(t)
	caller := newFakeCaller()
	caller.refuse = true
	runner := newTestRunner(store, caller, &fakeUsage{})
	run := startAndWait(t, runner, threeCaseRun(t, store, runner).ID, false)
	if run.Status != RunStatusStoppedBudget || !strings.Contains(run.StatusReason, "refused for budget") {
		t.Fatalf("run ended %s: %s", run.Status, run.StatusReason)
	}
	progress, _ := store.RunProgress(run.ID)
	if progress.AnswersRecorded != 3 { // only the three originals
		t.Errorf("%d answers recorded; a refused call must leave no answer behind", progress.AnswersRecorded)
	}
	refusedKeys := caller.keys()
	caller.refuse = false
	startAndWait(t, runner, run.ID, false)
	resumedKeys := caller.keys()[len(refusedKeys):]
	if !strings.Contains(strings.Join(resumedKeys, " "), refusedKeys[0]) {
		t.Errorf("the resume did not send the refused key %s again", refusedKeys[0])
	}
}

func TestARunWillNotStartWithoutAnOrganization(t *testing.T) {
	store := openTestStore(t)
	runner := NewRunner(store, newFakeCaller(), syntheticCatalog(), &fakeUsage{}, RunDefaults{Concurrency: 1, BudgetCheckEveryCalls: 1})
	run := threeCaseRun(t, store, runner)
	if _, err := runner.StartRun(run.ID, false); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "organization") {
		t.Errorf("start without an organization = %v", err)
	}
}

func TestCreatingARunRefusesUnknownModelsAndFreezesASample(t *testing.T) {
	store := openTestStore(t)
	runner := newTestRunner(store, newFakeCaller(), &fakeUsage{})
	suite := emailSuite(t, store)
	for i := range 10 {
		addEmailCase(t, store, suite, fmt.Sprint(i))
	}
	if _, err := runner.CreateRun(context.Background(), NewRun{SuiteID: suite.ID, CandidateModelIDs: []string{"model-small", "model-nobody-knows"},
		JudgeModelIDs: []string{"judge-a"}, CaseSelection: CaseSelection{Mode: CaseSelectionAll}}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "model-nobody-knows") {
		t.Errorf("unknown model = %v", err)
	}
	sample := func(seed int64) []string {
		run, err := runner.CreateRun(context.Background(), NewRun{SuiteID: suite.ID, CandidateModelIDs: []string{"model-small", "model-large"},
			JudgeModelIDs: []string{"judge-a"}, CaseSelection: CaseSelection{Mode: CaseSelectionSample, Size: 4, Seed: seed}})
		if err != nil {
			t.Fatal(err)
		}
		return run.CaseIDs
	}
	first, again, other := sample(7), sample(7), sample(8)
	if len(first) != 4 || strings.Join(first, ",") != strings.Join(again, ",") {
		t.Errorf("the same seed picked %v and %v", first, again)
	}
	if strings.Join(first, ",") == strings.Join(other, ",") {
		t.Errorf("seeds 7 and 8 picked the same sample %v", first)
	}
}
