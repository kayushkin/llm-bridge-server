package predictionstore

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Bucket is one decile of the calibration curve: of the predictions you stated
// at roughly this confidence, how many came true.
type Bucket struct {
	Lo              float64 `json:"lo"`
	Hi              float64 `json:"hi"`
	N               int     `json:"n"`
	MeanProbability float64 `json:"mean_probability"`
	ObservedRate    float64 `json:"observed_rate"`
	Gap             float64 `json:"gap"` // stated minus observed; positive = too confident here
}

// Score is the calibration of one set of resolved predictions.
type Score struct {
	Group string `json:"group,omitempty"`
	N     int    `json:"n"` // resolved and scoreable; excludes open and void
	Open  int    `json:"open"`
	Void  int    `json:"void"`

	// Brier is the mean squared error of the stated probability. 0 is perfect,
	// 0.25 is what you get by always saying 50%. Lower is better.
	Brier float64 `json:"brier"`
	// LogScore is the mean log likelihood of what happened. Closer to 0 is
	// better; it punishes confident misses far harder than Brier does.
	LogScore float64 `json:"log_score"`

	MeanProbability float64 `json:"mean_probability"`
	ObservedRate    float64 `json:"observed_rate"`

	// The overconfidence view. A prediction stated at 20% that came out false
	// was a CORRECT call made with 80% confidence, and the raw mean-probability
	// comparison above scores it as if you had been wrong. MeanConfidence is
	// max(p, 1-p) and Accuracy is how often the side you leaned on won, so
	// Overconfidence is the gap that actually needs correcting.
	MeanConfidence float64 `json:"mean_confidence"`
	Accuracy       float64 `json:"accuracy"`
	Overconfidence float64 `json:"overconfidence"`

	// Murphy's decomposition: Brier = Reliability - Resolution + Uncertainty.
	// It separates the two failures that need opposite corrections. Bad
	// Reliability means the numbers are shifted and you should shade them. Bad
	// Resolution means you are not discriminating at all — your 90%s and your
	// 60%s come true equally often — and no amount of shading fixes that; stop
	// dressing guesses as estimates. Computed by grouping on the exact stated
	// probability, which the ladder makes possible, so the identity is exact
	// rather than an artifact of the bucket edges.
	Reliability float64 `json:"reliability"`
	Resolution  float64 `json:"resolution"`
	Uncertainty float64 `json:"uncertainty"`

	// SkillScore is 1 - Brier/Uncertainty: how much better than always guessing
	// the base rate. Positive is skill, 0 is no better than the base rate,
	// negative is worse. Null when every outcome was the same, which makes the
	// comparison undefined rather than zero.
	SkillScore *float64 `json:"skill_score"`

	// Whether revising the odds helped. BrierInitial scores the opening number,
	// Brier scores the final one, and UpdateGain is the difference: positive
	// means updating on evidence moved you toward the truth.
	BrierInitial float64 `json:"brier_initial"`
	UpdateGain   float64 `json:"update_gain"`
	RevisedCount int     `json:"revised_count"`

	Buckets []Bucket `json:"buckets"`
}

// CalibrationReport is the answer to "what kind of prediction am I most often
// wrong about".
type CalibrationReport struct {
	GroupBy   string   `json:"group_by,omitempty"`
	Overall   Score    `json:"overall"`
	Groups    []*Score `json:"groups,omitempty"`
	Generated int64    `json:"generated_at"`
}

// BaseRate is how often claims of a kind turned out true. This is the number
// the "start from the base rate" rule needs and could not previously get.
type BaseRate struct {
	Group string  `json:"group,omitempty"`
	N     int     `json:"n"`
	True  int     `json:"true"`
	False int     `json:"false"`
	Rate  float64 `json:"rate"`
}

// BaseRateReport carries the overall rate and the requested slice.
type BaseRateReport struct {
	GroupBy   string      `json:"group_by,omitempty"`
	Overall   BaseRate    `json:"overall"`
	Groups    []*BaseRate `json:"groups,omitempty"`
	Generated int64       `json:"generated_at"`
}

// GroupByFields are the slices a caller may ask for. Served, not inferred.
var GroupByFields = []string{"category", "provenance", "author", "tag", "entity_type", "lead_time", "probability"}

// validateGroupBy rejects a slice this store cannot cut. The empty string means
// "do not slice" and is always allowed.
func validateGroupBy(groupBy string) error {
	if groupBy == "" || inList(GroupByFields, groupBy) {
		return nil
	}
	return fmt.Errorf("%w: unknown group_by %q: use one of %s",
		ErrInvalidPrediction, groupBy, strings.Join(GroupByFields, ", "))
}

// scored is one resolved prediction reduced to what the maths needs.
type scored struct {
	probability float64
	initial     float64
	outcome     float64 // 1 or 0
	revised     bool
	groups      []string
}

func brierOf(p *Prediction) (float64, bool) {
	if p.Status != "resolved" || (p.Outcome != "true" && p.Outcome != "false") {
		return 0, false
	}
	o := 0.0
	if p.Outcome == "true" {
		o = 1
	}
	d := p.Probability - o
	return d * d, true
}

func leadTimeGroup(created, resolved int64) string {
	switch d := resolved - created; {
	case d < 3600:
		return "under-1h"
	case d < 86400:
		return "under-1d"
	case d < 604800:
		return "under-1w"
	case d < 2592000:
		return "under-1mo"
	default:
		return "over-1mo"
	}
}

// gather loads every prediction matching the filter and reduces the resolved
// ones to scoreable rows, tagged with their group keys.
func (s *Store) gather(f Filter, groupBy string) ([]scored, int, int, error) {
	// Validated before the rows are read, not inside the loop over them: an
	// empty result set would otherwise accept any group_by at all and answer
	// with an ungrouped report, which reads as "no data" rather than "you asked
	// for a slice that does not exist".
	if err := validateGroupBy(groupBy); err != nil {
		return nil, 0, 0, err
	}
	f.Limit, f.Offset, f.Expand = 0, 0, false
	predictions, err := s.List(f)
	if err != nil {
		return nil, 0, 0, err
	}
	revisions, err := s.revisionCounts()
	if err != nil {
		return nil, 0, 0, err
	}
	var entityTypes map[string][]string
	if groupBy == "entity_type" {
		if entityTypes, err = s.entityTypesByPrediction(); err != nil {
			return nil, 0, 0, err
		}
	}

	var rows []scored
	var open, void int
	for _, p := range predictions {
		switch p.Status {
		case "open":
			open++
			continue
		case "void":
			void++
			continue
		}
		if p.Outcome != "true" && p.Outcome != "false" {
			continue
		}
		o := 0.0
		if p.Outcome == "true" {
			o = 1
		}
		row := scored{
			probability: p.Probability,
			initial:     p.InitialProbability,
			outcome:     o,
			revised:     revisions[p.ID] > 1,
		}
		switch groupBy {
		case "":
		case "category":
			row.groups = []string{p.Category}
		case "provenance":
			row.groups = []string{p.Provenance}
		case "author":
			author := p.Author
			if author == "" {
				author = "(unattributed)"
			}
			row.groups = []string{author}
		case "tag":
			row.groups = append([]string{}, p.Tags...)
		case "entity_type":
			row.groups = entityTypes[p.ID]
		case "lead_time":
			row.groups = []string{leadTimeGroup(p.CreatedAt, p.ResolvedAt)}
		case "probability":
			row.groups = []string{fmt.Sprintf("%g", p.Probability)}
		default:
			// Unreachable: validateGroupBy above rejects anything not listed.
			// Kept so adding a field to GroupByFields without adding its case
			// here fails loudly instead of silently grouping nothing.
			return nil, 0, 0, fmt.Errorf("%w: group_by %q is listed but not implemented",
				ErrInvalidPrediction, groupBy)
		}
		rows = append(rows, row)
	}
	return rows, open, void, nil
}

func (s *Store) revisionCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT prediction_id, COUNT(*) FROM estimates GROUP BY prediction_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) entityTypesByPrediction() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT prediction_id, entity_type FROM links`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, entityType string
		if err := rows.Scan(&id, &entityType); err != nil {
			return nil, err
		}
		out[id] = append(out[id], entityType)
	}
	return out, rows.Err()
}

// score turns scoreable rows into a Score. It is pure arithmetic on its input
// so it can be tested without a database.
func score(rows []scored, open, void int) Score {
	sc := Score{N: len(rows), Open: open, Void: void, Buckets: []Bucket{}}
	if len(rows) == 0 {
		return sc
	}
	n := float64(len(rows))
	var sumBrier, sumBrierInitial, sumLog, sumProb, sumOutcome, sumConfidence, sumCorrect float64
	byProbability := map[float64][]scored{}

	for _, r := range rows {
		d := r.probability - r.outcome
		sumBrier += d * d
		di := r.initial - r.outcome
		sumBrierInitial += di * di
		if r.outcome == 1 {
			sumLog += math.Log(r.probability)
		} else {
			sumLog += math.Log(1 - r.probability)
		}
		sumProb += r.probability
		sumOutcome += r.outcome
		confidence := math.Max(r.probability, 1-r.probability)
		sumConfidence += confidence
		leanedTrue := r.probability >= 0.5
		if leanedTrue == (r.outcome == 1) {
			sumCorrect++
		}
		if r.revised {
			sc.RevisedCount++
		}
		byProbability[r.probability] = append(byProbability[r.probability], r)
	}

	sc.Brier = sumBrier / n
	sc.BrierInitial = sumBrierInitial / n
	sc.UpdateGain = sc.BrierInitial - sc.Brier
	sc.LogScore = sumLog / n
	sc.MeanProbability = sumProb / n
	sc.ObservedRate = sumOutcome / n
	sc.MeanConfidence = sumConfidence / n
	sc.Accuracy = sumCorrect / n
	sc.Overconfidence = sc.MeanConfidence - sc.Accuracy

	// Murphy decomposition over exact stated probabilities.
	baseRate := sc.ObservedRate
	for probability, group := range byProbability {
		nk := float64(len(group))
		var trues float64
		for _, r := range group {
			trues += r.outcome
		}
		observed := trues / nk
		sc.Reliability += nk * (probability - observed) * (probability - observed)
		sc.Resolution += nk * (observed - baseRate) * (observed - baseRate)
	}
	sc.Reliability /= n
	sc.Resolution /= n
	sc.Uncertainty = baseRate * (1 - baseRate)
	if sc.Uncertainty > 0 {
		skill := 1 - sc.Brier/sc.Uncertainty
		sc.SkillScore = &skill
	}

	sc.Buckets = decileBuckets(rows)
	return sc
}

func decileBuckets(rows []scored) []Bucket {
	type acc struct {
		n                  int
		sumProb, sumOutcom float64
	}
	buckets := make([]acc, 10)
	for _, r := range rows {
		i := int(r.probability * 10)
		if i > 9 {
			i = 9
		}
		buckets[i].n++
		buckets[i].sumProb += r.probability
		buckets[i].sumOutcom += r.outcome
	}
	out := []Bucket{}
	for i, b := range buckets {
		if b.n == 0 {
			continue
		}
		nk := float64(b.n)
		mean := b.sumProb / nk
		observed := b.sumOutcom / nk
		out = append(out, Bucket{
			Lo:              float64(i) / 10,
			Hi:              float64(i+1) / 10,
			N:               b.n,
			MeanProbability: mean,
			ObservedRate:    observed,
			Gap:             mean - observed,
		})
	}
	return out
}

// Calibration scores the predictions matching f, optionally sliced by groupBy.
func (s *Store) Calibration(f Filter, groupBy string) (*CalibrationReport, error) {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	rows, open, void, err := s.gather(f, groupBy)
	if err != nil {
		return nil, err
	}
	report := &CalibrationReport{GroupBy: groupBy, Overall: score(rows, open, void), Generated: now()}
	if groupBy == "" {
		return report, nil
	}
	grouped := map[string][]scored{}
	for _, r := range rows {
		for _, g := range r.groups {
			grouped[g] = append(grouped[g], r)
		}
	}
	for name, group := range grouped {
		sc := score(group, 0, 0)
		sc.Group = name
		report.Groups = append(report.Groups, &sc)
	}
	// Worst calibration first: the point of the slice is to surface what needs
	// correcting, and a caller that wants alphabetical can sort at the edge.
	sort.Slice(report.Groups, func(i, j int) bool {
		if report.Groups[i].Brier != report.Groups[j].Brier {
			return report.Groups[i].Brier > report.Groups[j].Brier
		}
		return report.Groups[i].Group < report.Groups[j].Group
	})
	return report, nil
}

// BaseRates answers how often claims of a kind came true.
func (s *Store) BaseRates(f Filter, groupBy string) (*BaseRateReport, error) {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy == "" {
		groupBy = "category"
	}
	rows, _, _, err := s.gather(f, groupBy)
	if err != nil {
		return nil, err
	}
	report := &BaseRateReport{GroupBy: groupBy, Overall: baseRateOf("", rows), Generated: now()}
	grouped := map[string][]scored{}
	for _, r := range rows {
		for _, g := range r.groups {
			grouped[g] = append(grouped[g], r)
		}
	}
	for name, group := range grouped {
		br := baseRateOf(name, group)
		report.Groups = append(report.Groups, &br)
	}
	sort.Slice(report.Groups, func(i, j int) bool {
		if report.Groups[i].N != report.Groups[j].N {
			return report.Groups[i].N > report.Groups[j].N
		}
		return report.Groups[i].Group < report.Groups[j].Group
	})
	return report, nil
}

func baseRateOf(name string, rows []scored) BaseRate {
	br := BaseRate{Group: name, N: len(rows)}
	for _, r := range rows {
		if r.outcome == 1 {
			br.True++
		} else {
			br.False++
		}
	}
	if br.N > 0 {
		br.Rate = float64(br.True) / float64(br.N)
	}
	return br
}
