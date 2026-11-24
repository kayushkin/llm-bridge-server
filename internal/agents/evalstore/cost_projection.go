package evalstore

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ProjectionRequest is what GET /runs/{id}/cost-projection takes.
type ProjectionRequest struct {
	// TargetUSD is the weekly spend to solve for.
	TargetUSD float64
	// CallsPerWeek is the volume for a task that does not batch; zero takes
	// the suite's weekly calls source.
	CallsPerWeek float64
	// WeeklyUnits is the volume for a task that batches: units of the
	// batched feature a week.
	WeeklyUnits float64
	// Features are per-call values; a feature not given takes its mean over
	// the run's cases.
	Features map[string]float64
}

// parseProjectionRequest reads the query: target_usd, calls_per_week,
// weekly.<batched feature> and feature.<name>.
func parseProjectionRequest(query map[string][]string, model CostModel) (ProjectionRequest, error) {
	request := ProjectionRequest{TargetUSD: 100, Features: map[string]float64{}}
	for key, values := range query {
		if len(values) == 0 {
			continue
		}
		number, err := strconv.ParseFloat(values[0], 64)
		if err != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
			return request, invalidf("%s=%q is not a non-negative number", key, values[0])
		}
		switch {
		case key == "target_usd":
			request.TargetUSD = number
		case key == "calls_per_week":
			request.CallsPerWeek = number
		case strings.HasPrefix(key, "weekly."):
			feature := strings.TrimPrefix(key, "weekly.")
			if model.Batching == nil || model.Batching.Feature != feature {
				return request, invalidf("%s: only the batched feature takes a weekly volume, and this suite batches %s", key, batchedFeatureName(model))
			}
			request.WeeklyUnits = number
		case strings.HasPrefix(key, "feature."):
			feature := strings.TrimPrefix(key, "feature.")
			if !slices.Contains(model.Features, feature) {
				return request, invalidf("%s: the suite's cost model has features %v", key, model.Features)
			}
			if model.Batching != nil && model.Batching.Feature == feature {
				return request, invalidf("%s is batched: give its weekly volume as weekly.%s", key, feature)
			}
			request.Features[feature] = number
		default:
			return request, invalidf("unknown parameter %s; known: target_usd, calls_per_week, weekly.<batched feature>, feature.<name>", key)
		}
	}
	if request.TargetUSD <= 0 {
		return request, invalidf("target_usd must be positive")
	}
	return request, nil
}

func batchedFeatureName(model CostModel) string {
	if model.Batching == nil {
		return "nothing"
	}
	return model.Batching.Feature
}

// CostProjection is the answer of GET /runs/{id}/cost-projection.
type CostProjection struct {
	RunID string `json:"run_id"`
	// FixedInputTokens and InputTokensPerFeature are the baseline's input
	// formula; Fitted names the coefficients fitted from the run rather than
	// pinned on the suite.
	FixedInputTokens      float64            `json:"fixed_input_tokens"`
	InputTokensPerFeature map[string]float64 `json:"input_tokens_per_feature"`
	Fitted                []string           `json:"fitted"`
	FitCases              int                `json:"fit_cases"`
	ReferenceModelID      string             `json:"reference_model_id"`
	Features              map[string]float64 `json:"features"`
	CallsPerWeek          float64            `json:"calls_per_week"`
	CallsPerWeekSource    string             `json:"calls_per_week_source"`
	BatchedFeature        string             `json:"batched_feature,omitempty"`
	WeeklyUnits           float64            `json:"weekly_units,omitempty"`
	UnitsPerCall          float64            `json:"units_per_call,omitempty"`
	TargetUSD             float64            `json:"target_usd"`
	Models                []ProjectedModel   `json:"models"`
	Note                  string             `json:"note"`
}

// ProjectedModel is one model's projected week.
type ProjectedModel struct {
	ModelID           string  `json:"model_id"`
	PriceKnown        bool    `json:"price_known"`
	HarnessMultiplier float64 `json:"harness_multiplier"`
	// OutputTokensPerUnit is per unit of the output feature, or per call.
	OutputTokensPerUnit  float64             `json:"output_tokens_per_unit"`
	InputTokensPerCall   float64             `json:"input_tokens_per_call"`
	OutputTokensPerCall  float64             `json:"output_tokens_per_call"`
	CostPerCall          *float64            `json:"cost_per_call"`
	CostPerWeek          *float64            `json:"cost_per_week"`
	CostPerUnit          *float64            `json:"cost_per_unit,omitempty"`
	VolumeForTarget      *float64            `json:"volume_for_target"`
	FeatureForTarget     map[string]*float64 `json:"feature_for_target"`
	CoefficientForTarget map[string]*float64 `json:"coefficient_for_target"`
}

// projectionVolumeCeiling bounds every solve; a figure past it is reported
// as out of reach rather than invented.
const projectionVolumeCeiling = 1e7

// BuildCostProjection models a week of the task for every candidate.
func (s *Store) BuildCostProjection(ctx context.Context, runID string, query map[string][]string, operations OperationLister) (*CostProjection, error) {
	run, err := s.GetRun(runID)
	if err != nil {
		return nil, err
	}
	suite, err := s.GetSuite(run.SuiteID)
	if err != nil {
		return nil, err
	}
	// A cost model with no features is a fixed input per call, fitted to
	// the baseline when not pinned.
	model := suite.CostModel
	request, err := parseProjectionRequest(query, model)
	if err != nil {
		return nil, err
	}
	answers, err := s.answersByKey(runID)
	if err != nil {
		return nil, err
	}
	cases := map[string]*Case{}
	for _, id := range run.CaseIDs {
		c, err := s.GetCase(id)
		if err != nil {
			return nil, err
		}
		cases[id] = c
	}
	projection := &CostProjection{RunID: runID, TargetUSD: request.TargetUSD, Features: map[string]float64{},
		Note: "List prices, no cache discount. Input per call = harness multiplier × (fixed + Σ coefficient × feature); the multiplier is " +
			"a model's mean input tokens over the reference model's on the run's cases. " + model.Note}
	if err := projection.fit(run, suite, answers, cases); err != nil {
		return nil, err
	}

	for _, feature := range model.Features {
		if model.Batching != nil && feature == model.Batching.Feature {
			continue
		}
		if value, given := request.Features[feature]; given {
			projection.Features[feature] = value
			continue
		}
		var values []float64
		for _, c := range cases {
			values = append(values, c.Features[feature])
		}
		projection.Features[feature] = mean(values)
	}
	if model.Batching != nil {
		if request.WeeklyUnits <= 0 {
			return nil, invalidf("this suite batches %s: give weekly.%s, the units a week", model.Batching.Feature, model.Batching.Feature)
		}
		projection.BatchedFeature, projection.WeeklyUnits = model.Batching.Feature, request.WeeklyUnits
		projection.UnitsPerCall = model.Batching.unitsPerCall(request.WeeklyUnits)
		projection.CallsPerWeek = request.WeeklyUnits / projection.UnitsPerCall
		projection.CallsPerWeekSource = fmt.Sprintf("%g %s a week in batches of %g", request.WeeklyUnits, model.Batching.Feature, projection.UnitsPerCall)
	} else {
		callsPerWeek, source, err := resolveCallsPerWeek(ctx, suite, ReportOptions{CallsPerWeek: request.CallsPerWeek}, operations, s.now())
		if err != nil {
			return nil, err
		}
		if callsPerWeek <= 0 {
			return nil, invalidf("no calls per week: %s", source)
		}
		projection.CallsPerWeek, projection.CallsPerWeekSource = callsPerWeek, source
	}

	for _, candidate := range run.CandidateModelIDs {
		projected, err := projection.project(run, model, candidate, answers, cases)
		if err != nil {
			return nil, err
		}
		projection.Models = append(projection.Models, projected)
	}
	return projection, nil
}

func (b *Batching) unitsPerCall(weeklyUnits float64) float64 {
	return math.Min(b.MaximumPerCall, math.Max(b.MinimumPerCall, weeklyUnits/b.TicksPerWeek))
}

// fit pins or fits the baseline's input formula.
func (p *CostProjection) fit(run *Run, suite *Suite, answers map[answerKey]*Answer, cases map[string]*Case) error {
	model := suite.CostModel
	p.InputTokensPerFeature = map[string]float64{}
	var unknowns []string
	if model.FixedInputTokens != nil {
		p.FixedInputTokens = *model.FixedInputTokens
	} else {
		unknowns = append(unknowns, "fixed")
	}
	for _, feature := range model.Features {
		if value, pinned := model.InputTokensPerFeature[feature]; pinned {
			p.InputTokensPerFeature[feature] = value
		} else {
			unknowns = append(unknowns, feature)
		}
	}
	p.ReferenceModelID = suite.BaselineModelID
	if p.ReferenceModelID != "" && !slices.Contains(run.CandidateModelIDs, p.ReferenceModelID) {
		p.ReferenceModelID = ""
	}
	if len(unknowns) == 0 {
		return nil
	}
	if p.ReferenceModelID == "" {
		return invalidf("cost model leaves %v to fit, and the suite's baseline is not a candidate of this run to fit them to", unknowns)
	}
	var rows [][]float64
	var targets []float64
	for _, caseID := range run.CaseIDs {
		a := answers[answerKey{caseID, p.ReferenceModelID}]
		if a == nil || !a.UsageKnown {
			continue
		}
		features := cases[caseID].Features
		target := float64(a.TotalInputTokens()) - p.FixedInputTokens
		row := make([]float64, 0, len(unknowns))
		for _, unknown := range unknowns {
			if unknown == "fixed" {
				row = append(row, 1)
			} else {
				row = append(row, features[unknown])
			}
		}
		for feature, value := range p.InputTokensPerFeature {
			target -= value * features[feature]
		}
		rows, targets = append(rows, row), append(targets, target)
	}
	coefficients, err := leastSquares(rows, targets)
	if err != nil {
		return invalidf("fit %v to %d answers of %s: %v; pin them on the suite's cost_model", unknowns, len(rows), p.ReferenceModelID, err)
	}
	for i, unknown := range unknowns {
		if unknown == "fixed" {
			p.FixedInputTokens = coefficients[i]
		} else {
			p.InputTokensPerFeature[unknown] = coefficients[i]
		}
	}
	p.Fitted, p.FitCases = unknowns, len(rows)
	return nil
}

func (p *CostProjection) formulaInput(features map[string]float64, coefficients map[string]float64) float64 {
	total := p.FixedInputTokens
	for feature, coefficient := range coefficients {
		total += coefficient * features[feature]
	}
	return total
}

func (p *CostProjection) project(run *Run, model CostModel, candidate string, answers map[answerKey]*Answer, cases map[string]*Case) (ProjectedModel, error) {
	price := run.ModelPrices[candidate]
	projected := ProjectedModel{ModelID: candidate, PriceKnown: price.Known, FeatureForTarget: map[string]*float64{}, CoefficientForTarget: map[string]*float64{}}
	var inputs, outputs, formula []float64
	outputUnits := 0.0
	for _, caseID := range run.CaseIDs {
		a := answers[answerKey{caseID, candidate}]
		if a == nil || !a.UsageKnown {
			continue
		}
		inputs = append(inputs, float64(a.TotalInputTokens()))
		outputs = append(outputs, float64(a.OutputTokens))
		formula = append(formula, p.formulaInput(cases[caseID].Features, p.InputTokensPerFeature))
		if model.OutputPerFeature != "" {
			outputUnits += cases[caseID].Features[model.OutputPerFeature]
		}
	}
	if len(inputs) == 0 {
		return projected, nil
	}
	reference := mean(formula)
	if p.ReferenceModelID != "" {
		var referenceInputs []float64
		for _, caseID := range run.CaseIDs {
			if a := answers[answerKey{caseID, p.ReferenceModelID}]; a != nil && a.UsageKnown {
				referenceInputs = append(referenceInputs, float64(a.TotalInputTokens()))
			}
		}
		if len(referenceInputs) > 0 {
			reference = mean(referenceInputs)
		}
	}
	projected.HarnessMultiplier = mean(inputs) / reference
	if model.OutputPerFeature != "" {
		if outputUnits == 0 {
			return projected, invalidf("no %s in the cases %s answered, so output per %s cannot be measured", model.OutputPerFeature, candidate, model.OutputPerFeature)
		}
		projected.OutputTokensPerUnit = sum(outputs) / outputUnits
	} else {
		projected.OutputTokensPerUnit = mean(outputs)
	}

	week := func(volume float64, features map[string]float64, coefficients map[string]float64) (cost, perCallInput, perCallOutput float64) {
		callFeatures := map[string]float64{}
		for feature, value := range features {
			callFeatures[feature] = value
		}
		calls := volume
		if model.Batching != nil {
			unitsPerCall := model.Batching.unitsPerCall(volume)
			callFeatures[model.Batching.Feature] = unitsPerCall
			calls = volume / unitsPerCall
		}
		perCallInput = projected.HarnessMultiplier * p.formulaInput(callFeatures, coefficients)
		perCallOutput = projected.OutputTokensPerUnit
		if model.OutputPerFeature != "" {
			perCallOutput *= callFeatures[model.OutputPerFeature]
		}
		cost = calls * (perCallInput*price.InputCostPerMillion/1e6 + perCallOutput*price.OutputCostPerMillion/1e6)
		return cost, perCallInput, perCallOutput
	}
	volume := p.CallsPerWeek
	if model.Batching != nil {
		volume = p.WeeklyUnits
	}
	cost, perCallInput, perCallOutput := week(volume, p.Features, p.InputTokensPerFeature)
	projected.InputTokensPerCall, projected.OutputTokensPerCall = perCallInput, perCallOutput
	if !price.Known {
		return projected, nil
	}
	projected.CostPerWeek = pointer(cost)
	projected.CostPerCall = pointer(cost / p.CallsPerWeek)
	if model.Batching != nil {
		projected.CostPerUnit = pointer(cost / p.WeeklyUnits)
	}
	projected.VolumeForTarget = solveIncreasing(func(v float64) float64 {
		c, _, _ := week(v, p.Features, p.InputTokensPerFeature)
		return c
	}, 1, p.TargetUSD)
	for feature := range p.Features {
		projected.FeatureForTarget[feature] = solveIncreasing(func(v float64) float64 {
			features := cloneMap(p.Features)
			features[feature] = v
			c, _, _ := week(volume, features, p.InputTokensPerFeature)
			return c
		}, 0, p.TargetUSD)
	}
	for feature := range p.InputTokensPerFeature {
		projected.CoefficientForTarget[feature] = solveIncreasing(func(v float64) float64 {
			coefficients := cloneMap(p.InputTokensPerFeature)
			coefficients[feature] = v
			c, _, _ := week(volume, p.Features, coefficients)
			return c
		}, 0, p.TargetUSD)
	}
	return projected, nil
}

// solveIncreasing finds where an increasing f reaches target between low and
// the ceiling by bisection, as the Python bench did. Nil when the target is
// out of reach in that range.
func solveIncreasing(f func(float64) float64, low, target float64) *float64 {
	high := projectionVolumeCeiling
	if f(high) < target || f(low) > target {
		return nil
	}
	for range 100 {
		middle := (low + high) / 2
		if f(middle) < target {
			low = middle
		} else {
			high = middle
		}
	}
	return pointer(low)
}

func cloneMap(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sum(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total
}

// leastSquares solves the normal equations XᵀX b = Xᵀy by Gaussian
// elimination with partial pivoting.
func leastSquares(rows [][]float64, targets []float64) ([]float64, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("no answers with usage")
	}
	n := len(rows[0])
	if len(rows) < n {
		return nil, fmt.Errorf("%d answers for %d unknowns", len(rows), n)
	}
	matrix := make([][]float64, n)
	for i := range matrix {
		matrix[i] = make([]float64, n+1)
	}
	for r, row := range rows {
		for i := range n {
			for j := range n {
				matrix[i][j] += row[i] * row[j]
			}
			matrix[i][n] += row[i] * targets[r]
		}
	}
	for column := range n {
		pivot := column
		for r := column + 1; r < n; r++ {
			if math.Abs(matrix[r][column]) > math.Abs(matrix[pivot][column]) {
				pivot = r
			}
		}
		if math.Abs(matrix[pivot][column]) < 1e-9 {
			return nil, fmt.Errorf("the features do not vary enough to tell the coefficients apart")
		}
		matrix[column], matrix[pivot] = matrix[pivot], matrix[column]
		for r := range n {
			if r == column {
				continue
			}
			factor := matrix[r][column] / matrix[column][column]
			for c := column; c <= n; c++ {
				matrix[r][c] -= factor * matrix[column][c]
			}
		}
	}
	solution := make([]float64, n)
	for i := range n {
		solution[i] = matrix[i][n] / matrix[i][i]
	}
	return solution, nil
}
