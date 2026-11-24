package predictionstore

import (
	"math"
	"strings"
	"testing"
)

func closeTo(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %.12f, want %.12f", label, got, want)
	}
}

// seedScored builds the worked example the assertions below are hand-computed
// from: four predictions stated at 0.9 of which three came true, and two stated
// at 0.5 of which one did.
//
//	Brier       = (3*0.01 + 0.81 + 0.25 + 0.25) / 6 = 0.223333…
//	base rate   = 4/6 = 0.666667
//	reliability = (4*(0.9-0.75)^2 + 2*0)      / 6   = 0.015
//	resolution  = (4*(0.75-⅔)^2 + 2*(0.5-⅔)^2) / 6  = 0.013888…
//	uncertainty = ⅔ * ⅓                             = 0.222222…
func seedScored() []scored {
	return []scored{
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 0},
		{probability: 0.5, initial: 0.5, outcome: 1},
		{probability: 0.5, initial: 0.5, outcome: 0},
	}
}

func TestBrierAndLogScoreMatchTheHandComputedNumbers(t *testing.T) {
	sc := score(seedScored(), 0, 0)
	if sc.N != 6 {
		t.Fatalf("n = %d, want 6", sc.N)
	}
	closeTo(t, "brier", sc.Brier, 1.34/6)
	closeTo(t, "observed_rate", sc.ObservedRate, 4.0/6)
	closeTo(t, "mean_probability", sc.MeanProbability, (0.9*4+0.5*2)/6)
	wantLog := (3*math.Log(0.9) + math.Log(0.1) + 2*math.Log(0.5)) / 6
	closeTo(t, "log_score", sc.LogScore, wantLog)
}

// The identity is the whole reason the decomposition is worth reporting: if it
// does not hold, the three terms are not a decomposition of anything.
func TestMurphyDecompositionAddsBackUpToBrier(t *testing.T) {
	sc := score(seedScored(), 0, 0)
	closeTo(t, "reliability", sc.Reliability, 0.015)
	closeTo(t, "resolution", sc.Resolution, (4*math.Pow(0.75-4.0/6, 2)+2*math.Pow(0.5-4.0/6, 2))/6)
	closeTo(t, "uncertainty", sc.Uncertainty, (4.0/6)*(2.0/6))
	closeTo(t, "reliability - resolution + uncertainty",
		sc.Reliability-sc.Resolution+sc.Uncertainty, sc.Brier)
}

// A prediction stated at 20% that came out false was a correct call made with
// 80% confidence. Comparing mean probability to the observed rate scores it as
// a failure; the confidence view is the one that tells you whether to shade
// your numbers.
func TestOverconfidenceReadsBothSidesOfFiftyPercent(t *testing.T) {
	sc := score(seedScored(), 0, 0)
	closeTo(t, "mean_confidence", sc.MeanConfidence, (0.9*4+0.5*2)/6)
	closeTo(t, "accuracy", sc.Accuracy, 4.0/6)
	closeTo(t, "overconfidence", sc.Overconfidence, 0.1)

	// The same set read on the low side: four claims at 0.1, three of which
	// came out false, is exactly as well calibrated as four at 0.9 of which
	// three came true. Brier agrees; the raw mean-vs-observed comparison does
	// not, which is why both are reported.
	mirrored := []scored{
		{probability: 0.1, initial: 0.1, outcome: 0},
		{probability: 0.1, initial: 0.1, outcome: 0},
		{probability: 0.1, initial: 0.1, outcome: 0},
		{probability: 0.1, initial: 0.1, outcome: 1},
	}
	high := score([]scored{
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.9, initial: 0.9, outcome: 0},
	}, 0, 0)
	low := score(mirrored, 0, 0)
	closeTo(t, "mirrored brier", low.Brier, high.Brier)
	closeTo(t, "mirrored accuracy", low.Accuracy, high.Accuracy)
	closeTo(t, "mirrored overconfidence", low.Overconfidence, high.Overconfidence)
}

// A predictor who says 60% to everything can be perfectly calibrated and still
// tell you nothing. Resolution is the term that catches it, and it is the one
// that no amount of shading the numbers will fix.
func TestNoDiscriminationShowsUpAsZeroResolution(t *testing.T) {
	rows := []scored{
		{probability: 0.5, initial: 0.5, outcome: 1},
		{probability: 0.5, initial: 0.5, outcome: 0},
		{probability: 0.5, initial: 0.5, outcome: 1},
		{probability: 0.5, initial: 0.5, outcome: 0},
	}
	sc := score(rows, 0, 0)
	closeTo(t, "reliability", sc.Reliability, 0)
	closeTo(t, "resolution", sc.Resolution, 0)
	if sc.SkillScore == nil {
		t.Fatal("skill score should be defined when outcomes differ")
	}
	closeTo(t, "skill_score", *sc.SkillScore, 0) // no better than the base rate
}

func TestSkillScoreIsUndefinedWhenEveryOutcomeMatches(t *testing.T) {
	sc := score([]scored{
		{probability: 0.9, initial: 0.9, outcome: 1},
		{probability: 0.8, initial: 0.8, outcome: 1},
	}, 0, 0)
	if sc.SkillScore != nil {
		t.Fatalf("skill_score = %v, want null — the comparison is undefined, not zero", *sc.SkillScore)
	}
}

func TestUpdateGainRewardsMovingTowardTheTruth(t *testing.T) {
	rows := []scored{
		{probability: 0.95, initial: 0.5, outcome: 1, revised: true},
		{probability: 0.05, initial: 0.5, outcome: 0, revised: true},
	}
	sc := score(rows, 0, 0)
	if sc.UpdateGain <= 0 {
		t.Fatalf("update_gain = %v, want positive — both revisions moved toward what happened", sc.UpdateGain)
	}
	closeTo(t, "brier_initial", sc.BrierInitial, 0.25)
	closeTo(t, "update_gain", sc.UpdateGain, 0.25-sc.Brier)
	if sc.RevisedCount != 2 {
		t.Fatalf("revised_count = %d, want 2", sc.RevisedCount)
	}
}

func TestEmptySetScoresNothingRatherThanZero(t *testing.T) {
	sc := score(nil, 3, 1)
	if sc.N != 0 || sc.Open != 3 || sc.Void != 1 {
		t.Fatalf("counts = %d/%d/%d, want 0/3/1", sc.N, sc.Open, sc.Void)
	}
	if len(sc.Buckets) != 0 {
		t.Fatalf("buckets = %d, want none", len(sc.Buckets))
	}
	if sc.SkillScore != nil {
		t.Fatalf("skill score should be undefined with no data")
	}
}

func TestCalibrationSlicesByCategoryWorstFirst(t *testing.T) {
	s := newTestStore(t)
	// Confident and wrong about causes; confident and right about behavior.
	for i := 0; i < 3; i++ {
		p := samplePrediction()
		p.Category = "root-cause"
		p.Probability = 0.9
		created := mustCreate(t, s, p)
		if _, err := s.Resolve(created.ID, "false", ""); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		p := samplePrediction()
		p.Category = "behavior"
		p.Probability = 0.9
		created := mustCreate(t, s, p)
		if _, err := s.Resolve(created.ID, "true", ""); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	report, err := s.Calibration(Filter{}, "category")
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	if len(report.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(report.Groups))
	}
	if report.Groups[0].Group != "root-cause" {
		t.Fatalf("worst group = %q, want root-cause first", report.Groups[0].Group)
	}
	closeTo(t, "root-cause brier", report.Groups[0].Brier, 0.81)
	closeTo(t, "behavior brier", report.Groups[1].Brier, 0.01)
}

func TestUnknownGroupByNamesTheFields(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Calibration(Filter{}, "astrology")
	if err == nil {
		t.Fatal("expected an error for an unknown group_by")
	}
	for _, f := range GroupByFields {
		if !strings.Contains(err.Error(), f) {
			t.Fatalf("error should list %q, got: %v", f, err)
		}
	}
}

func TestBaseRatesAnswerHowOftenAKindOfClaimIsTrue(t *testing.T) {
	s := newTestStore(t)
	outcomes := []string{"true", "true", "false", "true"}
	for _, o := range outcomes {
		p := samplePrediction()
		p.Category = "will-fix"
		created := mustCreate(t, s, p)
		if _, err := s.Resolve(created.ID, o, ""); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	report, err := s.BaseRates(Filter{}, "category")
	if err != nil {
		t.Fatalf("base rates: %v", err)
	}
	if len(report.Groups) != 1 || report.Groups[0].Group != "will-fix" {
		t.Fatalf("groups = %+v", report.Groups)
	}
	closeTo(t, "will-fix rate", report.Groups[0].Rate, 0.75)
	if report.Groups[0].N != 4 || report.Groups[0].True != 3 {
		t.Fatalf("n/true = %d/%d, want 4/3", report.Groups[0].N, report.Groups[0].True)
	}
	closeTo(t, "overall rate", report.Overall.Rate, 0.75)
}

func TestOpenPredictionsAreCountedButNotScored(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	resolved := mustCreate(t, s, samplePrediction())
	if _, err := s.Resolve(resolved.ID, "true", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	report, err := s.Calibration(Filter{}, "")
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	if report.Overall.N != 1 || report.Overall.Open != 1 {
		t.Fatalf("n/open = %d/%d, want 1/1", report.Overall.N, report.Overall.Open)
	}
}
