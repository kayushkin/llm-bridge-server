package predictionstore

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The vocabularies are served (GET /categories) rather than inferred from the
// rows, so no caller ever builds a filter out of whatever values happen to
// exist. Same move as job-store's role families.

// Categories are the KINDS of claim, chosen so that a calibration slice by
// category answers "what am I systematically wrong about". Keep this list
// short: a category with three predictions in it scores nothing.
var Categories = []string{
	"root-cause",   // this is what is actually wrong
	"will-fix",     // this change will fix it
	"behavior",     // this system behaves this way / this call returns that
	"will-ship-by", // this lands by this date
	"effort",       // this is roughly this much work
	"will-recur",   // this will happen again; this is flaky
	"external",     // anything about the world beyond this host
}

// categorySynonyms maps a word somebody actually typed onto a canonical
// category. Unknown input is a 400 that names the vocabulary — never a silent
// coercion to some default bucket.
var categorySynonyms = map[string]string{
	"cause": "root-cause", "diagnosis": "root-cause", "rootcause": "root-cause",
	"root_cause": "root-cause", "why": "root-cause",
	"fix": "will-fix", "repair": "will-fix", "willfix": "will-fix",
	"will_fix": "will-fix", "patch": "will-fix",
	"behaviour": "behavior", "api": "behavior", "contract": "behavior",
	"returns": "behavior", "works": "behavior",
	"deadline": "will-ship-by", "ship": "will-ship-by", "eta": "will-ship-by",
	"schedule": "will-ship-by", "will_ship_by": "will-ship-by", "willshipby": "will-ship-by",
	"size": "effort", "estimate": "effort", "cost": "effort", "duration": "effort",
	"flaky": "will-recur", "recur": "will-recur", "again": "will-recur",
	"regression": "will-recur", "will_recur": "will-recur", "willrecur": "will-recur",
	"world": "external", "outside": "external", "market": "external",
}

// Provenance is where the belief came from, taken verbatim from the AGENTS.md
// rule that provenance and probability are different facts and the reader needs
// both. 90% from a live call and 90% from a plausible-looking README should not
// be spent the same way — slicing calibration by this column is what proves it.
var Provenances = []string{"measured", "read", "inferred", "guessed"}

// Statuses. `void` means the claim stopped being resolvable — the work was
// canceled, the question changed — and it is excluded from every score rather
// than counted as a miss.
var Statuses = []string{"open", "resolved", "void"}

// Outcomes accepted by POST /predictions/{id}/resolve.
var Outcomes = []string{"true", "false", "void"}

// ProbabilityLadder is the set of odds you may state. The house rule asks for
// steps of 5 or 10 below 90, then 95 / 99 / 99.9; a flat step of 5 from 0.05 to
// 0.95 satisfies it and is easier to remember, with 0.99 and 0.999 above and
// 0.01 and 0.001 mirrored below. The mirror matters: a claim you think is
// unlikely deserves the same resolution as one you think is likely.
//
// Off-ladder values are a 400. "73%" claims a precision nobody has and reads as
// arithmetic that was never done; refusing it also keeps the calibration
// buckets clean enough to mean something.
var ProbabilityLadder = buildLadder()

func buildLadder() []float64 {
	ladder := []float64{0.001, 0.01}
	for step := 5; step <= 95; step += 5 {
		ladder = append(ladder, float64(step)/100)
	}
	ladder = append(ladder, 0.99, 0.999)
	sort.Float64s(ladder)
	return ladder
}

// ladderTolerance absorbs float64 round-tripping through JSON. It is far
// tighter than the smallest gap between two rungs (0.009, from 0.001 to 0.01),
// so it can never let an off-ladder value through.
const ladderTolerance = 1e-9

// NormalizeCategory resolves a caller's word to a canonical category.
func NormalizeCategory(raw string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	key = strings.ReplaceAll(key, " ", "-")
	for _, c := range Categories {
		if key == c {
			return c, true
		}
	}
	if c, ok := categorySynonyms[key]; ok {
		return c, true
	}
	if c, ok := categorySynonyms[strings.ReplaceAll(key, "-", "_")]; ok {
		return c, true
	}
	return "", false
}

// ErrUnknownCategory names the whole vocabulary, because a caller who guessed
// wrong cannot guess right from a bare rejection.
func ErrUnknownCategory(raw string) error {
	return fmt.Errorf("%w: unknown category %q: use one of %s",
		ErrInvalidPrediction, raw, strings.Join(Categories, ", "))
}

// ValidProvenance reports whether raw is one of the four provenances.
func ValidProvenance(raw string) bool { return inList(Provenances, raw) }

// ValidOutcome reports whether raw is an outcome resolve accepts.
func ValidOutcome(raw string) bool { return inList(Outcomes, raw) }

func inList(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// SnapToLadder returns p unchanged if it sits on the ladder, and an error
// naming the two nearest rungs if it does not.
func SnapToLadder(p float64) (float64, error) {
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return 0, fmt.Errorf("%w: probability must be a number, got %v", ErrInvalidPrediction, p)
	}
	if p <= 0 || p >= 1 {
		return 0, fmt.Errorf(
			"%w: probability must be between 0 and 1 exclusive, got %v: 0 and 1 are not odds you can be wrong about, and they make the log score infinite",
			ErrInvalidPrediction, p)
	}
	for _, rung := range ProbabilityLadder {
		if math.Abs(p-rung) < ladderTolerance {
			return rung, nil
		}
	}
	below, above := nearestRungs(p)
	return 0, fmt.Errorf(
		"%w: probability %v is off the ladder: use %v or %v. The ladder is steps of 5 below 95, plus 0.001, 0.01, 0.99 and 0.999 — a value like 0.73 claims a precision nobody has",
		ErrInvalidPrediction, p, below, above)
}

func nearestRungs(p float64) (below, above float64) {
	below, above = ProbabilityLadder[0], ProbabilityLadder[len(ProbabilityLadder)-1]
	for _, rung := range ProbabilityLadder {
		if rung < p && rung > below {
			below = rung
		}
	}
	for i := len(ProbabilityLadder) - 1; i >= 0; i-- {
		if ProbabilityLadder[i] > p && ProbabilityLadder[i] < above {
			above = ProbabilityLadder[i]
		}
	}
	return below, above
}
