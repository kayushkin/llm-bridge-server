package evalstore

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Cost assumptions. model-store lists one input and one output price per
// model and no cache prices, so the cache-adjusted figure rests on these,
// and every report says so.
const (
	cacheReadShareOfInputPrice           = 0.10
	anthropicCacheWriteShareOfInputPrice = 1.25
	anthropicProvider                    = "anthropic"
)

var costAssumptions = []string{
	"Cost per call at list price charges every input token, cache reads and writes included, at the model's input price, as llm-bridge-server's budgets do.",
	"The cache-adjusted cost is an assumption, not a price anyone lists here: cache reads at 10% of the input price, cache writes at 125% for Anthropic models and 100% for others.",
	"Prices are the ones frozen on the run when it was created; a model model-store had no price for has no cost figures.",
}

// Report is everything GET /runs/{id}/report says about a run.
type Report struct {
	RunID              string                    `json:"run_id"`
	RunStatus          string                    `json:"run_status"`
	RunOrigin          string                    `json:"run_origin"`
	SuiteID            string                    `json:"suite_id"`
	SuiteName          string                    `json:"suite_name"`
	TaskKind           string                    `json:"task_kind"`
	Cases              int                       `json:"cases"`
	BaselineModelID    string                    `json:"baseline_model_id"`
	Judges             []ReportJudge             `json:"judges"`
	CallsPerWeek       *float64                  `json:"calls_per_week"`
	CallsPerWeekSource string                    `json:"calls_per_week_source"`
	PricesNote         string                    `json:"prices_note"`
	Assumptions        []string                  `json:"assumptions"`
	Models             []ReportModel             `json:"models"`
	BestAnswer         ReportBestAnswer          `json:"best_answer"`
	PairwiseVsBaseline []ReportPairwise          `json:"pairwise_vs_baseline"`
	Checks             map[string][]ReportCheck  `json:"checks"`
	BaselineAgreement  []ReportBaselineAgreement `json:"baseline_agreement"`
	Progress           RunProgress               `json:"progress"`
	PredictionID       string                    `json:"prediction_id,omitempty"`
}

// ReportJudge is one judge and how far it can be trusted on this run.
type ReportJudge struct {
	ModelID  string `json:"model_id"`
	Provider string `json:"provider"`
	// IsCandidate: the judge also answered, so it ranks its own answer.
	IsCandidate bool `json:"is_candidate"`
	// SharesProviderWith lists the candidates of the judge's own provider,
	// whose answers it may favor.
	SharesProviderWith []string `json:"shares_provider_with"`
	Valid              int      `json:"valid_verdicts"`
	Malformed          int      `json:"malformed_verdicts"`
	Failed             int      `json:"failed_verdicts"`
}

// ReportModel is one candidate's figures.
type ReportModel struct {
	ModelID  string `json:"model_id"`
	Provider string `json:"provider"`
	// Answers counts the answers recorded, AnswerErrors those that failed.
	Answers      int                `json:"answers"`
	AnswerErrors int                `json:"answer_errors"`
	PerJudge     []ReportModelJudge `json:"per_judge"`
	// AllJudgesJudged counts the cases every judge gave a valid verdict on;
	// the next two are out of that.
	AllJudgesJudged            int                 `json:"all_judges_judged"`
	AllJudgesAccept            int                 `json:"all_judges_accept"`
	JudgesAgreeOnAcceptability int                 `json:"judges_agree_on_acceptability"`
	FailureKinds               []ReportFailureKind `json:"failure_kinds"`
	Tokens                     *ReportTokens       `json:"tokens"`
	PriceKnown                 bool                `json:"price_known"`
	InputCostPerMillion        float64             `json:"input_cost_per_million"`
	OutputCostPerMillion       float64             `json:"output_cost_per_million"`
	CostPerCallListPrice       *float64            `json:"cost_per_call_list_price"`
	CostPerCallCacheAdjusted   *float64            `json:"cost_per_call_cache_adjusted"`
	CostPerWeekListPrice       *float64            `json:"cost_per_week_list_price"`
	CostPerWeekCacheAdjusted   *float64            `json:"cost_per_week_cache_adjusted"`
	LatencyMedianSeconds       *float64            `json:"latency_median_seconds"`
	LatencyP90Seconds          *float64            `json:"latency_p90_seconds"`
}

// ReportModelJudge is one judge's view of one candidate.
type ReportModelJudge struct {
	JudgeModelID string   `json:"judge_model_id"`
	Acceptable   int      `json:"acceptable"`
	Judged       int      `json:"judged"`
	MeanRank     *float64 `json:"mean_rank"`
}

// ReportFailureKind counts one judge's verdicts of one kind and acceptability
// on one candidate, for every verdict that was unacceptable or named a kind.
type ReportFailureKind struct {
	JudgeModelID string `json:"judge_model_id"`
	FailureKind  string `json:"failure_kind"`
	Acceptable   bool   `json:"acceptable"`
	Count        int    `json:"count"`
}

// ReportTokens are means over the answers whose usage is known. Input
// includes cache reads and writes.
type ReportTokens struct {
	Answers        int     `json:"answers"`
	InputMean      float64 `json:"input_mean"`
	CacheReadMean  float64 `json:"cache_read_mean"`
	CacheWriteMean float64 `json:"cache_write_mean"`
	OutputMean     float64 `json:"output_mean"`
	OutputMedian   float64 `json:"output_median"`
}

// ReportBestAnswer says how often every judge put the same answer first.
type ReportBestAnswer struct {
	CasesAllJudged    int            `json:"cases_all_judged"`
	JudgesAgree       int            `json:"judges_agree"`
	WinnersWhenAgreed map[string]int `json:"winners_when_agreed"`
}

// ReportPairwise compares one candidate with the baseline case by case.
type ReportPairwise struct {
	ModelID           string `json:"model_id"`
	Cases             int    `json:"cases"`
	AllPreferModel    int    `json:"all_judges_prefer_model"`
	AllPreferBaseline int    `json:"all_judges_prefer_baseline"`
	Split             int    `json:"split"`
}

// ReportCheck sums one check over one model's answers.
type ReportCheck struct {
	CheckName string `json:"check_name"`
	Answers   int    `json:"answers"`
	Failed    int    `json:"failed"`
	Count     int    `json:"count"`
}

// ReportBaselineAgreement says how often a model chose what the baseline
// chose, unit by unit (an email's cards, for filing).
type ReportBaselineAgreement struct {
	ModelID string `json:"model_id"`
	Same    int    `json:"same"`
	Units   int    `json:"units"`
}

// ReportOptions changes how a report is computed.
type ReportOptions struct {
	// CallsPerWeek overrides the suite's weekly calls source when positive.
	CallsPerWeek float64
}

// BuildReport computes a run's report. bridge counts operations when the
// suite's weekly calls come from llm-bridge-server; it may be nil otherwise.
func (s *Store) BuildReport(ctx context.Context, runID string, options ReportOptions, operations OperationLister) (*Report, error) {
	run, err := s.GetRun(runID)
	if err != nil {
		return nil, err
	}
	suite, err := s.GetSuite(run.SuiteID)
	if err != nil {
		return nil, err
	}
	answers, err := s.answersByKey(runID)
	if err != nil {
		return nil, err
	}
	verdicts, err := s.ListVerdicts(runID, VerdictFilter{})
	if err != nil {
		return nil, err
	}
	checks, err := s.ListChecks(runID)
	if err != nil {
		return nil, err
	}
	progress, err := s.RunProgress(runID)
	if err != nil {
		return nil, err
	}
	report := computeReport(run, suite, answers, verdicts, checks)
	report.Progress = progress
	callsPerWeek, source, err := resolveCallsPerWeek(ctx, suite, options, operations, s.now())
	if err != nil {
		return nil, err
	}
	report.CallsPerWeekSource = source
	if callsPerWeek > 0 {
		report.CallsPerWeek = &callsPerWeek
		for i := range report.Models {
			m := &report.Models[i]
			if m.CostPerCallListPrice != nil {
				m.CostPerWeekListPrice = pointer(*m.CostPerCallListPrice * callsPerWeek)
				m.CostPerWeekCacheAdjusted = pointer(*m.CostPerCallCacheAdjusted * callsPerWeek)
			}
		}
	}
	if suite.CheckerName != "" && suite.BaselineModelID != "" && slices.Contains(run.CandidateModelIDs, suite.BaselineModelID) {
		report.BaselineAgreement, err = s.baselineAgreement(run, suite, answers)
		if err != nil {
			return nil, err
		}
	}
	return report, nil
}

// OperationLister lists operations on llm-bridge-server.
type OperationLister interface {
	CountOperations(ctx context.Context, filter OperationsFilter) (int, error)
}

func resolveCallsPerWeek(ctx context.Context, suite *Suite, options ReportOptions, operations OperationLister, now time.Time) (float64, string, error) {
	if options.CallsPerWeek > 0 {
		return options.CallsPerWeek, "given in the request", nil
	}
	source := suite.WeeklyCallsSource
	switch source.Kind {
	case WeeklyCallsSourceFixed:
		return source.CallsPerWeek, "the suite's fixed figure: " + source.Note, nil
	case WeeklyCallsSourceLLMBridgeOperations:
		if operations == nil {
			return 0, "", fmt.Errorf("the suite counts weekly calls on llm-bridge-server, and no bridge is configured")
		}
		filter := *source.Operations
		filter.CreatedAfter = now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
		filter.CreatedBefore = ""
		count, err := operations.CountOperations(ctx, filter)
		if err != nil {
			return 0, "", fmt.Errorf("count the suite's weekly calls: %w", err)
		}
		return float64(count), fmt.Sprintf("succeeded llm-bridge-server operations in the seven days before %s", now.UTC().Format(time.RFC3339)), nil
	}
	return 0, "the suite names no weekly calls source and the request gave no calls_per_week", nil
}

// verdictTable is the valid verdicts of a run: judge → case → model → line.
type verdictTable map[string]map[string]map[string]VerdictCandidate

func (t verdictTable) line(judge, caseID, model string) (VerdictCandidate, bool) {
	line, ok := t[judge][caseID][model]
	return line, ok
}

func (t verdictTable) judged(judge, caseID string) bool {
	_, ok := t[judge][caseID]
	return ok
}

// computeReport is the arithmetic, apart from the store, so tests can feed it.
func computeReport(run *Run, suite *Suite, answers map[answerKey]*Answer, verdicts []*Verdict, checks []CheckResult) *Report {
	report := &Report{RunID: run.ID, RunStatus: run.Status, RunOrigin: run.Origin, SuiteID: suite.ID, SuiteName: suite.Name,
		TaskKind: suite.TaskKind, Cases: len(run.CaseIDs), BaselineModelID: suite.BaselineModelID, PricesNote: run.PricesNote,
		Assumptions: costAssumptions, PredictionID: run.PredictionID, Checks: map[string][]ReportCheck{},
		BestAnswer: ReportBestAnswer{WinnersWhenAgreed: map[string]int{}}}

	table := verdictTable{}
	judgeCounts := map[string]*ReportJudge{}
	for _, judge := range run.JudgeModelIDs {
		table[judge] = map[string]map[string]VerdictCandidate{}
		provider := run.ModelPrices[judge].Provider
		reportJudge := &ReportJudge{ModelID: judge, Provider: provider, IsCandidate: slices.Contains(run.CandidateModelIDs, judge),
			SharesProviderWith: []string{}}
		for _, candidate := range run.CandidateModelIDs {
			if provider != "" && run.ModelPrices[candidate].Provider == provider {
				reportJudge.SharesProviderWith = append(reportJudge.SharesProviderWith, candidate)
			}
		}
		judgeCounts[judge] = reportJudge
	}
	for _, v := range verdicts {
		counts := judgeCounts[v.JudgeModelID]
		if counts == nil {
			continue
		}
		switch {
		case v.Error != "":
			counts.Failed++
		case !v.Valid:
			counts.Malformed++
		default:
			counts.Valid++
			lines := map[string]VerdictCandidate{}
			for _, line := range v.Candidates {
				lines[line.ModelID] = line
			}
			table[v.JudgeModelID][v.CaseID] = lines
		}
	}
	for _, judge := range run.JudgeModelIDs {
		report.Judges = append(report.Judges, *judgeCounts[judge])
	}

	allJudged := func(caseID string) bool {
		for _, judge := range run.JudgeModelIDs {
			if !table.judged(judge, caseID) {
				return false
			}
		}
		return len(run.JudgeModelIDs) > 0
	}

	for _, model := range run.CandidateModelIDs {
		report.Models = append(report.Models, modelFigures(run, model, answers, table, allJudged))
	}

	for _, caseID := range run.CaseIDs {
		if !allJudged(caseID) {
			continue
		}
		report.BestAnswer.CasesAllJudged++
		var first string
		agree := true
		for i, judge := range run.JudgeModelIDs {
			top := ""
			for _, model := range run.CandidateModelIDs {
				if line, ok := table.line(judge, caseID, model); ok && line.Rank == 1 {
					top = model
				}
			}
			if i == 0 {
				first = top
			} else if top != first {
				agree = false
			}
		}
		if agree && first != "" {
			report.BestAnswer.JudgesAgree++
			report.BestAnswer.WinnersWhenAgreed[first]++
		}
	}

	if suite.BaselineModelID != "" && slices.Contains(run.CandidateModelIDs, suite.BaselineModelID) {
		for _, model := range run.CandidateModelIDs {
			if model == suite.BaselineModelID {
				continue
			}
			pairwise := ReportPairwise{ModelID: model}
			for _, caseID := range run.CaseIDs {
				if !allJudged(caseID) {
					continue
				}
				pairwise.Cases++
				prefer := 0
				for _, judge := range run.JudgeModelIDs {
					mine, _ := table.line(judge, caseID, model)
					baseline, _ := table.line(judge, caseID, suite.BaselineModelID)
					if mine.Rank < baseline.Rank {
						prefer++
					}
				}
				switch prefer {
				case len(run.JudgeModelIDs):
					pairwise.AllPreferModel++
				case 0:
					pairwise.AllPreferBaseline++
				default:
					pairwise.Split++
				}
			}
			report.PairwiseVsBaseline = append(report.PairwiseVsBaseline, pairwise)
		}
	}

	summed := map[string]map[string]*ReportCheck{}
	for _, check := range checks {
		if summed[check.ModelID] == nil {
			summed[check.ModelID] = map[string]*ReportCheck{}
		}
		sum := summed[check.ModelID][check.CheckName]
		if sum == nil {
			sum = &ReportCheck{CheckName: check.CheckName}
			summed[check.ModelID][check.CheckName] = sum
		}
		sum.Answers++
		sum.Count += check.Count
		if !check.Passed {
			sum.Failed++
		}
	}
	for model, byName := range summed {
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			report.Checks[model] = append(report.Checks[model], *byName[name])
		}
	}
	return report
}

func modelFigures(run *Run, model string, answers map[answerKey]*Answer, table verdictTable, allJudged func(string) bool) ReportModel {
	price := run.ModelPrices[model]
	figures := ReportModel{ModelID: model, Provider: price.Provider, PriceKnown: price.Known, InputCostPerMillion: price.InputCostPerMillion,
		OutputCostPerMillion: price.OutputCostPerMillion, FailureKinds: []ReportFailureKind{}}

	for _, judge := range run.JudgeModelIDs {
		perJudge := ReportModelJudge{JudgeModelID: judge}
		var ranks []float64
		for _, caseID := range run.CaseIDs {
			line, ok := table.line(judge, caseID, model)
			if !ok {
				continue
			}
			perJudge.Judged++
			if line.Acceptable {
				perJudge.Acceptable++
			}
			ranks = append(ranks, float64(line.Rank))
		}
		if len(ranks) > 0 {
			perJudge.MeanRank = pointer(mean(ranks))
		}
		figures.PerJudge = append(figures.PerJudge, perJudge)
	}

	for _, caseID := range run.CaseIDs {
		if !allJudged(caseID) {
			continue
		}
		figures.AllJudgesJudged++
		allAccept, accepted := true, 0
		for _, judge := range run.JudgeModelIDs {
			line, _ := table.line(judge, caseID, model)
			if line.Acceptable {
				accepted++
			} else {
				allAccept = false
			}
		}
		if allAccept {
			figures.AllJudgesAccept++
		}
		if accepted == 0 || accepted == len(run.JudgeModelIDs) {
			figures.JudgesAgreeOnAcceptability++
		}
	}

	kinds := map[ReportFailureKind]int{}
	var order []ReportFailureKind
	for _, judge := range run.JudgeModelIDs {
		for _, caseID := range run.CaseIDs {
			line, ok := table.line(judge, caseID, model)
			if !ok || (line.Acceptable && line.FailureKind == FailureKindNone) {
				continue
			}
			key := ReportFailureKind{JudgeModelID: judge, FailureKind: line.FailureKind, Acceptable: line.Acceptable}
			if kinds[key] == 0 {
				order = append(order, key)
			}
			kinds[key]++
		}
	}
	for _, key := range order {
		key.Count = kinds[key]
		figures.FailureKinds = append(figures.FailureKinds, key)
	}

	var inputs, cacheReads, cacheWrites, outputs, latencies []float64
	for _, caseID := range run.CaseIDs {
		a := answers[answerKey{caseID, model}]
		if a == nil {
			continue
		}
		figures.Answers++
		if a.Error != "" {
			figures.AnswerErrors++
		}
		if a.UsageKnown {
			inputs = append(inputs, float64(a.TotalInputTokens()))
			cacheReads = append(cacheReads, float64(a.CacheReadTokens))
			cacheWrites = append(cacheWrites, float64(a.CacheWriteTokens))
			outputs = append(outputs, float64(a.OutputTokens))
		}
		if a.DurationMs != nil {
			latencies = append(latencies, float64(*a.DurationMs)/1000)
		}
	}
	if len(inputs) > 0 {
		tokens := &ReportTokens{Answers: len(inputs), InputMean: mean(inputs), CacheReadMean: mean(cacheReads), CacheWriteMean: mean(cacheWrites),
			OutputMean: mean(outputs), OutputMedian: median(outputs)}
		figures.Tokens = tokens
		if price.Known {
			inputPrice, outputPrice := price.InputCostPerMillion/1e6, price.OutputCostPerMillion/1e6
			listPrice := tokens.InputMean*inputPrice + tokens.OutputMean*outputPrice
			writeShare := 1.0
			if price.Provider == anthropicProvider {
				writeShare = anthropicCacheWriteShareOfInputPrice
			}
			adjustedInput := tokens.InputMean - tokens.CacheReadMean - tokens.CacheWriteMean +
				cacheReadShareOfInputPrice*tokens.CacheReadMean + writeShare*tokens.CacheWriteMean
			figures.CostPerCallListPrice = pointer(listPrice)
			figures.CostPerCallCacheAdjusted = pointer(adjustedInput*inputPrice + tokens.OutputMean*outputPrice)
		}
	}
	if len(latencies) > 0 {
		figures.LatencyMedianSeconds = pointer(median(latencies))
		figures.LatencyP90Seconds = pointer(nearestRankQuantile(latencies, 0.9))
	}
	return figures
}

// baselineAgreement compares each model's per-unit choices with the
// baseline's, using the suite's checker.
func (s *Store) baselineAgreement(run *Run, suite *Suite, answers map[answerKey]*Answer) ([]ReportBaselineAgreement, error) {
	checker := checkers[suite.CheckerName]
	var agreements []ReportBaselineAgreement
	cases := map[string]*Case{}
	for _, model := range run.CandidateModelIDs {
		if model == suite.BaselineModelID {
			continue
		}
		agreement := ReportBaselineAgreement{ModelID: model}
		comparable := false
		for _, caseID := range run.CaseIDs {
			baseline, mine := answers[answerKey{caseID, suite.BaselineModelID}], answers[answerKey{caseID, model}]
			if baseline == nil || !baseline.HasAnswer() {
				continue
			}
			c := cases[caseID]
			if c == nil {
				var err error
				if c, err = s.GetCase(caseID); err != nil {
					return nil, err
				}
				cases[caseID] = c
			}
			baselineChoices := checker.Choices(c, baseline.Parsed)
			if baselineChoices == nil {
				continue
			}
			comparable = true
			var myChoices map[string]string
			if mine != nil && mine.HasAnswer() {
				myChoices = checker.Choices(c, mine.Parsed)
			}
			for unit, choice := range baselineChoices {
				agreement.Units++
				if mineChoice, ok := myChoices[unit]; ok && mineChoice == choice {
					agreement.Same++
				}
			}
		}
		if comparable {
			agreements = append(agreements, agreement)
		}
	}
	return agreements, nil
}

func pointer(value float64) *float64 { return &value }

func mean(values []float64) float64 {
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

// median is the middle value, or the mean of the two middle values.
func median(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// nearestRankQuantile is the Python bench's quantile: the sorted value at
// round(p × (n − 1)), rounding half to even as Python's round does.
func nearestRankQuantile(values []float64, p float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	index := int(math.RoundToEven(p * float64(len(sorted)-1)))
	return sorted[min(len(sorted)-1, index)]
}

// Markdown renders the report for a person.
func (report *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s (%s)\n\n", report.RunID, report.SuiteName, report.TaskKind)
	fmt.Fprintf(&b, "Run status **%s**, origin %s, %d cases. Baseline: %s.\n\n", report.RunStatus, report.RunOrigin, report.Cases, orDash(report.BaselineModelID))
	if report.PredictionID != "" {
		fmt.Fprintf(&b, "Bears on %s.\n\n", report.PredictionID)
	}
	b.WriteString("## Judges\n\n| judge | provider | valid | malformed | failed | judges itself | same provider as |\n|---|---|---|---|---|---|---|\n")
	for _, j := range report.Judges {
		self := "no"
		if j.IsCandidate {
			self = "**yes**"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %s | %s |\n", j.ModelID, orDash(j.Provider), j.Valid, j.Malformed, j.Failed, self, orDash(strings.Join(j.SharesProviderWith, ", ")))
	}
	b.WriteString("\n## Acceptance and rank\n\n| model |")
	for _, j := range report.Judges {
		fmt.Fprintf(&b, " %s acceptable | %s mean rank |", j.ModelID, j.ModelID)
	}
	b.WriteString(" all accept | judges agree |\n|---|")
	for range report.Judges {
		b.WriteString("---|---|")
	}
	b.WriteString("---|---|\n")
	for _, m := range report.Models {
		fmt.Fprintf(&b, "| %s |", m.ModelID)
		for _, j := range m.PerJudge {
			fmt.Fprintf(&b, " %d/%d | %s |", j.Acceptable, j.Judged, formatOptional(j.MeanRank, "%.2f"))
		}
		fmt.Fprintf(&b, " %d/%d | %d/%d |\n", m.AllJudgesAccept, m.AllJudgesJudged, m.JudgesAgreeOnAcceptability, m.AllJudgesJudged)
	}
	fmt.Fprintf(&b, "\nEvery judge put the same answer first on %d of %d cases; winners then: %s.\n",
		report.BestAnswer.JudgesAgree, report.BestAnswer.CasesAllJudged, formatCounts(report.BestAnswer.WinnersWhenAgreed))
	if len(report.PairwiseVsBaseline) > 0 {
		fmt.Fprintf(&b, "\n## Against the baseline, %s\n\n| model | all judges prefer model | all prefer baseline | split |\n|---|---|---|---|\n", report.BaselineModelID)
		for _, p := range report.PairwiseVsBaseline {
			fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", p.ModelID, p.AllPreferModel, p.AllPreferBaseline, p.Split)
		}
	}
	b.WriteString("\n## Failure kinds\n\n")
	for _, m := range report.Models {
		if len(m.FailureKinds) == 0 {
			continue
		}
		var parts []string
		for _, f := range m.FailureKinds {
			acceptable := "unacceptable"
			if f.Acceptable {
				acceptable = "acceptable"
			}
			parts = append(parts, fmt.Sprintf("%s said %s (%s) ×%d", f.JudgeModelID, f.FailureKind, acceptable, f.Count))
		}
		fmt.Fprintf(&b, "- %s: %s\n", m.ModelID, strings.Join(parts, "; "))
	}
	fmt.Fprintf(&b, "\n## Tokens, cost and latency\n\nCalls per week: %s (%s).\n\n", formatOptional(report.CallsPerWeek, "%.0f"), report.CallsPerWeekSource)
	b.WriteString("| model | input mean | cache read | cache write | output mean | output median | $/call list | $/call cache-adjusted* | $/week list | $/week cache-adjusted* | latency median | latency p90 |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range report.Models {
		if m.Tokens == nil {
			fmt.Fprintf(&b, "| %s | no usage recorded | | | | | | | | | | |\n", m.ModelID)
			continue
		}
		t := m.Tokens
		fmt.Fprintf(&b, "| %s | %.0f | %.0f | %.0f | %.0f | %.0f | %s | %s | %s | %s | %s | %s |\n", m.ModelID, t.InputMean, t.CacheReadMean, t.CacheWriteMean,
			t.OutputMean, t.OutputMedian, formatOptional(m.CostPerCallListPrice, "%.4f"), formatOptional(m.CostPerCallCacheAdjusted, "%.4f"),
			formatOptional(m.CostPerWeekListPrice, "%.2f"), formatOptional(m.CostPerWeekCacheAdjusted, "%.2f"),
			formatOptional(m.LatencyMedianSeconds, "%.1fs"), formatOptional(m.LatencyP90Seconds, "%.1fs"))
	}
	b.WriteString("\n")
	for _, assumption := range report.Assumptions {
		fmt.Fprintf(&b, "\\* %s\n", assumption)
	}
	fmt.Fprintf(&b, "\n%s.\n", report.PricesNote)
	if len(report.Checks) > 0 {
		b.WriteString("\n## Checks\n\n| model | check | answers | failed | count |\n|---|---|---|---|---|\n")
		models := make([]string, 0, len(report.Checks))
		for model := range report.Checks {
			models = append(models, model)
		}
		sort.Strings(models)
		for _, model := range models {
			for _, check := range report.Checks[model] {
				fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", model, check.CheckName, check.Answers, check.Failed, check.Count)
			}
		}
	}
	if len(report.BaselineAgreement) > 0 {
		fmt.Fprintf(&b, "\n## Same choice as %s, unit by unit\n\n", report.BaselineModelID)
		for _, a := range report.BaselineAgreement {
			fmt.Fprintf(&b, "- %s: %d/%d\n", a.ModelID, a.Same, a.Units)
		}
	}
	return b.String()
}

func orDash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}

func formatOptional(value *float64, format string) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf(format, *value)
}

func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s %d", key, counts[key])
	}
	return strings.Join(parts, ", ")
}
