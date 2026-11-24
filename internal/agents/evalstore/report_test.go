package evalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func verdictLines(lines ...VerdictCandidate) []VerdictCandidate { return lines }

func line(model string, rank int, acceptable bool, kind string) VerdictCandidate {
	return VerdictCandidate{ModelID: model, Rank: rank, Acceptable: acceptable, FailureKind: kind}
}

// syntheticReportInputs: candidates base (anthropic) and other (openai),
// judges judge-a (anthropic) and judge-b (openai), three cases; judge-b's
// verdict on the third case is malformed.
func syntheticReportInputs() (*Run, *Suite, map[answerKey]*Answer, []*Verdict) {
	run := &Run{ID: "eval_run_000001", CandidateModelIDs: []string{"base", "other"}, JudgeModelIDs: []string{"judge-a", "judge-b"},
		CaseIDs: []string{"c1", "c2", "c3"}, ModelPrices: map[string]ModelPrice{
			"base":    {Provider: "anthropic", InputCostPerMillion: 3, OutputCostPerMillion: 15, Known: true},
			"other":   {Provider: "openai", InputCostPerMillion: 1, OutputCostPerMillion: 2, Known: true},
			"judge-a": {Provider: "anthropic", Known: true},
			"judge-b": {Provider: "openai", Known: true},
		}}
	suite := &Suite{ID: "eval_suite_000001", BaselineModelID: "base"}
	answers := map[answerKey]*Answer{}
	for i, caseID := range run.CaseIDs {
		duration := int64(1000 * []int{1, 3, 2}[i])
		answers[answerKey{caseID, "base"}] = &Answer{CaseID: caseID, ModelID: "base", Parsed: json.RawMessage(`{}`), InputTokens: 1000, CacheReadTokens: 4000,
			CacheWriteTokens: 1000, OutputTokens: int64([]int{100, 300, 200}[i]), UsageKnown: true, DurationMs: &duration}
		answers[answerKey{caseID, "other"}] = &Answer{CaseID: caseID, ModelID: "other", Parsed: json.RawMessage(`{}`), InputTokens: 2000, OutputTokens: 50,
			UsageKnown: true, DurationMs: &duration}
	}
	verdicts := []*Verdict{
		{CaseID: "c1", JudgeModelID: "judge-a", Valid: true, Candidates: verdictLines(line("base", 1, true, "none"), line("other", 2, true, "none"))},
		{CaseID: "c1", JudgeModelID: "judge-b", Valid: true, Candidates: verdictLines(line("base", 2, true, "none"), line("other", 1, false, "wrong_card"))},
		{CaseID: "c2", JudgeModelID: "judge-a", Valid: true, Candidates: verdictLines(line("base", 1, true, "none"), line("other", 2, true, "other"))},
		{CaseID: "c2", JudgeModelID: "judge-b", Valid: true, Candidates: verdictLines(line("base", 1, true, "none"), line("other", 2, true, "none"))},
		{CaseID: "c3", JudgeModelID: "judge-a", Valid: true, Candidates: verdictLines(line("base", 2, true, "none"), line("other", 1, true, "none"))},
		{CaseID: "c3", JudgeModelID: "judge-b", Valid: false, InvalidReason: "ranking misses a label"},
	}
	return run, suite, answers, verdicts
}

func TestReportArithmeticOnASyntheticRun(t *testing.T) {
	run, suite, answers, verdicts := syntheticReportInputs()
	report := computeReport(run, suite, answers, verdicts, nil)

	if report.Judges[1].Malformed != 1 || report.Judges[1].Valid != 2 || report.Judges[0].Valid != 3 {
		t.Errorf("judge counts %+v", report.Judges)
	}
	if !strings.Contains(strings.Join(report.Judges[0].SharesProviderWith, ","), "base") || report.Judges[0].IsCandidate {
		t.Errorf("judge-a should be flagged as sharing base's provider: %+v", report.Judges[0])
	}
	base, other := report.Models[0], report.Models[1]
	expectJudge := func(m ReportModel, judge int, acceptable, judged int, meanRank float64) {
		t.Helper()
		got := m.PerJudge[judge]
		if got.Acceptable != acceptable || got.Judged != judged || !near(*got.MeanRank, meanRank) {
			t.Errorf("%s by %s: %d/%d rank %v, want %d/%d rank %v", m.ModelID, got.JudgeModelID, got.Acceptable, got.Judged, *got.MeanRank, acceptable, judged, meanRank)
		}
	}
	expectJudge(base, 0, 3, 3, 4.0/3)
	expectJudge(base, 1, 2, 2, 1.5)
	expectJudge(other, 0, 3, 3, 5.0/3)
	expectJudge(other, 1, 1, 2, 1.5)
	if base.AllJudgesJudged != 2 || base.AllJudgesAccept != 2 || base.JudgesAgreeOnAcceptability != 2 {
		t.Errorf("base both-judge figures %+v", base)
	}
	if other.AllJudgesAccept != 1 || other.JudgesAgreeOnAcceptability != 1 {
		t.Errorf("other both-judge figures: accept %d agree %d, want 1 and 1", other.AllJudgesAccept, other.JudgesAgreeOnAcceptability)
	}
	if fmt.Sprint(other.FailureKinds) != "[{judge-a other true 1} {judge-b wrong_card false 1}]" {
		t.Errorf("failure kinds %v", other.FailureKinds)
	}
	if report.BestAnswer.CasesAllJudged != 2 || report.BestAnswer.JudgesAgree != 1 || report.BestAnswer.WinnersWhenAgreed["base"] != 1 {
		t.Errorf("best answer %+v", report.BestAnswer)
	}
	if p := report.PairwiseVsBaseline[0]; p.Cases != 2 || p.AllPreferModel != 0 || p.AllPreferBaseline != 1 || p.Split != 1 {
		t.Errorf("pairwise %+v", p)
	}

	tokens := base.Tokens
	if !near(tokens.InputMean, 6000) || !near(tokens.CacheReadMean, 4000) || !near(tokens.OutputMean, 200) || !near(tokens.OutputMedian, 200) {
		t.Errorf("tokens %+v", tokens)
	}
	// 6000 input at $3/M and 200 output at $15/M.
	if !near(*base.CostPerCallListPrice, 0.021) {
		t.Errorf("list price per call %v", *base.CostPerCallListPrice)
	}
	// (1000 fresh + 10% of 4000 read + 125% of 1000 written) at $3/M, plus output.
	if !near(*base.CostPerCallCacheAdjusted, 2650*3/1e6+0.003) {
		t.Errorf("cache-adjusted per call %v", *base.CostPerCallCacheAdjusted)
	}
	// Not Anthropic: cache writes at 100%; no cache here, so both agree.
	if !near(*other.CostPerCallListPrice, 0.0021) || !near(*other.CostPerCallCacheAdjusted, 0.0021) {
		t.Errorf("other per call %v / %v", *other.CostPerCallListPrice, *other.CostPerCallCacheAdjusted)
	}
	if !near(*base.LatencyMedianSeconds, 2) || !near(*base.LatencyP90Seconds, 3) {
		t.Errorf("latency median %v p90 %v", *base.LatencyMedianSeconds, *base.LatencyP90Seconds)
	}
	if markdown := report.Markdown(); !strings.Contains(markdown, "| other | 3/3 | 1.67 | 1/2 | 1.50 | 1/2 | 1/2 |") ||
		!strings.Contains(markdown, "assumption") {
		t.Errorf("markdown:\n%s", markdown)
	}
}

func TestAJudgeThatIsAlsoACandidateIsFlagged(t *testing.T) {
	run, suite, answers, verdicts := syntheticReportInputs()
	run.JudgeModelIDs = []string{"base"}
	report := computeReport(run, suite, answers, verdicts, nil)
	if !report.Judges[0].IsCandidate {
		t.Errorf("judge %+v", report.Judges[0])
	}
}

func TestQuantilesFollowThePythonBench(t *testing.T) {
	// round(0.9 × 5) = round(4.5) = 4 in Python, which rounds half to even.
	if got := nearestRankQuantile([]float64{6, 1, 5, 2, 4, 3}, 0.9); got != 5 {
		t.Errorf("p90 of 1..6 = %v, want 5", got)
	}
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("median = %v", got)
	}
}

// projectionRun stores a run of base and other on cases with 1 to 4
// emails, where base reads exactly 1000 + 300 per email and writes 50 per
// email, and other reads 1.5 times as much.
func projectionRun(t *testing.T, costModel CostModel) (*Store, *Run) {
	t.Helper()
	store := openTestStore(t)
	suite := emailSuite(t, store)
	if _, err := store.PatchSuite(suite.ID, SuitePatch{CostModel: &costModel, BaselineModelID: pointerTo("base")}); err != nil {
		t.Fatal(err)
	}
	var caseIDs []string
	for emails := 1; emails <= 4; emails++ {
		var refs []string
		for i := range emails {
			refs = append(refs, fmt.Sprintf("e%d-%d", emails, i))
		}
		c, err := store.CreateCase(suite.ID, &Case{SourceKind: SourceKindLLMBridgeOperation, SourceRef: fmt.Sprint("operation_", emails),
			UserPrompt: syntheticFilingPrompt(refs...)})
		if err != nil {
			t.Fatal(err)
		}
		caseIDs = append(caseIDs, c.ID)
	}
	run := &Run{SuiteID: suite.ID, CandidateModelIDs: []string{"base", "other"}, JudgeModelIDs: []string{"judge-a"},
		CaseSelection: CaseSelection{Mode: CaseSelectionAll}, CaseIDs: caseIDs, Status: RunStatusCompleted, BudgetLimits: []BudgetLimit{},
		ModelPrices: map[string]ModelPrice{"base": {Provider: "anthropic", InputCostPerMillion: 3, OutputCostPerMillion: 15, Known: true},
			"other": {Provider: "openai", InputCostPerMillion: 1, OutputCostPerMillion: 2, Known: true}}, Origin: RunOriginLive}
	err := store.inTransaction(func(tx *sql.Tx) error {
		id, err := store.insertRun(tx, run)
		run.ID = id
		if err != nil {
			return err
		}
		for i, caseID := range caseIDs {
			emails := int64(i + 1)
			for _, a := range []*Answer{
				{RunID: id, CaseID: caseID, ModelID: "base", Origin: AnswerOriginReplayed, Attempt: 1, Parsed: json.RawMessage(`{}`),
					InputTokens: 1000 + 300*emails, OutputTokens: 50 * emails, UsageKnown: true},
				{RunID: id, CaseID: caseID, ModelID: "other", Origin: AnswerOriginReplayed, Attempt: 1, Parsed: json.RawMessage(`{}`),
					InputTokens: (1000 + 300*emails) * 3 / 2, OutputTokens: 20 * emails, UsageKnown: true},
			} {
				if err := store.saveAnswerIn(tx, a); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, run
}

func pointerTo[T any](value T) *T { return &value }

func TestCostProjectionFitsTheBaselineAndSolvesForTheTarget(t *testing.T) {
	store, run := projectionRun(t, CostModel{Features: []string{"emails"}, OutputPerFeature: "emails",
		Batching: &Batching{Feature: "emails", TicksPerWeek: 10, MinimumPerCall: 1, MaximumPerCall: 3}})
	projection, err := store.BuildCostProjection(context.Background(), run.ID, url.Values{"weekly.emails": {"20"}, "target_usd": {"1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !near(math.Round(projection.FixedInputTokens), 1000) || !near(math.Round(projection.InputTokensPerFeature["emails"]), 300) {
		t.Fatalf("fit %v + %v per email, want 1000 + 300", projection.FixedInputTokens, projection.InputTokensPerFeature)
	}
	if projection.UnitsPerCall != 2 || projection.CallsPerWeek != 10 {
		t.Errorf("20 emails over 10 ticks: %v per call, %v calls", projection.UnitsPerCall, projection.CallsPerWeek)
	}
	base, other := projection.Models[0], projection.Models[1]
	if !near(base.HarnessMultiplier, 1) || math.Abs(other.HarnessMultiplier-1.5) > 1e-3 || !near(base.OutputTokensPerUnit, 50) {
		t.Errorf("multipliers %v %v, base output per email %v", base.HarnessMultiplier, other.HarnessMultiplier, base.OutputTokensPerUnit)
	}
	// Base: 10 calls of (1000 + 600) input at $3/M and 100 output at $15/M.
	if want := 10 * (1600*3/1e6 + 100*15/1e6); math.Abs(*base.CostPerWeek-want) > 1e-9 {
		t.Errorf("base week $%v, want $%v", *base.CostPerWeek, want)
	}
	solved, err := store.BuildCostProjection(context.Background(), run.ID, url.Values{"weekly.emails": {fmt.Sprint(*base.VolumeForTarget)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(*solved.Models[0].CostPerWeek-1) > 1e-6 {
		t.Errorf("at the solved volume %v base spends $%v a week, want $1", *base.VolumeForTarget, *solved.Models[0].CostPerWeek)
	}
	if _, err := store.BuildCostProjection(context.Background(), run.ID, url.Values{"feature.emails": {"2"}}, nil); err == nil {
		t.Error("a per-call value for the batched feature was accepted")
	}
}

func TestCostProjectionWithPinnedCoefficientsAndNoBatching(t *testing.T) {
	fixed := 500.0
	store, run := projectionRun(t, CostModel{Features: []string{"emails"}, FixedInputTokens: &fixed, InputTokensPerFeature: map[string]float64{"emails": 100}})
	projection, err := store.BuildCostProjection(context.Background(), run.ID, url.Values{"feature.emails": {"3"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Fitted != nil || projection.CallsPerWeek != 10 || !strings.Contains(projection.CallsPerWeekSource, "a synthetic figure") {
		t.Errorf("projection %+v", projection)
	}
	// Base's mean input is 1750 against itself: multiplier 1, input 500 + 300.
	if !near(projection.Models[0].InputTokensPerCall, 800) || !near(projection.Models[0].OutputTokensPerCall, 50*2.5) {
		t.Errorf("base per call %v in, %v out", projection.Models[0].InputTokensPerCall, projection.Models[0].OutputTokensPerCall)
	}
}

func TestLeastSquaresRefusesFeaturesThatDoNotVary(t *testing.T) {
	if _, err := leastSquares([][]float64{{1, 2}, {1, 2}, {1, 2}}, []float64{1, 1, 1}); err == nil {
		t.Error("a constant feature beside the intercept was fitted")
	}
}
