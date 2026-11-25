package main

import (
	"testing"
	"time"
)

// The fire decision is a chain of gates, and each one is the only thing standing
// between a quiet night and a session. They are tested separately because a test
// that only asserts "it did not fire" is satisfied by ANY gate closing, and would
// stay green with every other one deleted.

// A week's worth of anchors, so a test can say "day 3 of 7" without spelling a
// fraction the curve also spells. Never derived from targetCurve itself: a
// fixture seeded with the constant under test moves with the code and pins
// nothing.
const weekSeconds = int64(7 * 24 * 60 * 60)

// resetsAtForDay returns the resets_at that puts `now` exactly `day` days into
// the seven-day window.
func resetsAtForDay(now time.Time, day float64) int64 {
	return now.Unix() + int64((7-day)*24*60*60)
}

func snapshotAtDay(t *testing.T, usedPercent float64, day float64) *providerLimits {
	t.Helper()
	resets := resetsAtForDay(time.Now(), day)
	return &providerLimits{
		Provider: "anthropic",
		Windows: map[string]*limitWindow{
			"seven_day": {UsedPercent: usedPercent, ResetsAt: &resets},
		},
		SnapshotAt: time.Now().Unix(),
	}
}

// allDayProvider is the anthropic spec's shape: the safe window is the whole
// day, so a test about some OTHER gate is never accidentally decided by the
// clock the test happens to run at.
func allDayProvider() providerSpec {
	return providerSpec{
		Name:      "anthropic",
		WindowKey: "seven_day",
		SafeStart: 0,
		SafeEnd:   24 * 60,
		Cooldown:  15 * time.Minute,
	}
}

func emptyState() *state { return &state{LastFire: map[string]int64{}} }

// ---------------------------------------------------------------- the gates

// Six days in with the budget barely touched is the case this binary exists for.
// If this does not fire, nothing does, and every "did not fire" assertion below
// would be vacuous.
func TestFiresWhenUsageIsFarBelowTheCurve(t *testing.T) {
	d := evaluate(allDayProvider(), snapshotAtDay(t, 5, 6), time.Now(), emptyState(), 15)
	if !d.fire {
		t.Fatalf("did not fire at 5%% used on day 6 of 7: %s", d.reason)
	}
	if d.target <= d.actual {
		t.Errorf("target %.0f%% is not above actual %.0f%%, so this fired for the wrong reason",
			d.target, d.actual)
	}
}

// The slack band. Usage just under target must NOT fire: the curve is a ramp, and
// firing inside the band would dispatch a session on every tick all week.
func TestUsageInsideTheSlackBandDoesNotFire(t *testing.T) {
	// Day 6 targets 85%. Sitting at 80% is 5pp under — inside a 15pp band.
	d := evaluate(allDayProvider(), snapshotAtDay(t, 80, 6), time.Now(), emptyState(), 15)
	if d.fire {
		t.Fatalf("fired at %.0f%% used against a %.0f%% target with 15pp slack: %s",
			d.actual, d.target, d.reason)
	}
}

// The band is a band, not a floor. Usage ABOVE target must not fire either — that
// is the direction where firing spends budget the curve says is already spent.
func TestUsageAboveTargetDoesNotFire(t *testing.T) {
	d := evaluate(allDayProvider(), snapshotAtDay(t, 99, 3), time.Now(), emptyState(), 15)
	if d.fire {
		t.Fatalf("fired at %.0f%% used against a %.0f%% target: %s", d.actual, d.target, d.reason)
	}
}

// Slack has to be the number the caller passed. A gate that ignores it fires at a
// hair under target, every tick.
func TestSlackWidthDecidesTheBand(t *testing.T) {
	lim := snapshotAtDay(t, 80, 6) // target 85%, so 5pp under
	if d := evaluate(allDayProvider(), lim, time.Now(), emptyState(), 15); d.fire {
		t.Errorf("15pp slack should swallow a 5pp gap, but it fired: %s", d.reason)
	}
	if d := evaluate(allDayProvider(), snapshotAtDay(t, 80, 6), time.Now(), emptyState(), 1); !d.fire {
		t.Errorf("1pp slack should not swallow a 5pp gap, but it did not fire")
	}
}

// The cooldown is what keeps one under-used week from becoming a stream of
// sessions. It is checked against a decision that would otherwise fire.
func TestCooldownSuppressesAnOtherwiseFiringDecision(t *testing.T) {
	p := allDayProvider()
	lim := snapshotAtDay(t, 5, 6)

	fresh := &state{LastFire: map[string]int64{p.Name: time.Now().Add(-1 * time.Minute).Unix()}}
	if d := evaluate(p, lim, time.Now(), fresh, 15); d.fire {
		t.Errorf("fired 1 minute into a %s cooldown: %s", p.Cooldown, d.reason)
	}

	cold := &state{LastFire: map[string]int64{p.Name: time.Now().Add(-2 * p.Cooldown).Unix()}}
	if d := evaluate(p, snapshotAtDay(t, 5, 6), time.Now(), cold, 15); !d.fire {
		t.Errorf("did not fire after the cooldown had passed: %s", d.reason)
	}
}

// A cooldown recorded for one provider must not silence the other. They have
// different windows and different costs, and the state file keys them separately
// for exactly this reason.
func TestCooldownIsPerProvider(t *testing.T) {
	p := allDayProvider()
	other := &state{LastFire: map[string]int64{"codex": time.Now().Unix()}}
	if d := evaluate(p, snapshotAtDay(t, 5, 6), time.Now(), other, 15); !d.fire {
		t.Errorf("codex's cooldown suppressed anthropic: %s", d.reason)
	}
}

// The safe window is why codex does not start a session in the middle of the
// working day. Asserted through evaluate, not just inSafeWindow, so deleting the
// call site is caught too.
func TestSafeWindowSuppressesAnOtherwiseFiringDecision(t *testing.T) {
	p := allDayProvider()
	p.SafeStart = 0
	p.SafeEnd = 6 * 60 // 00:00–06:00

	inside := time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC)
	if d := evaluate(p, snapshotAtDay(t, 5, 6), inside, emptyState(), 15); !d.fire {
		t.Errorf("03:00 is inside a 00:00–06:00 window but it did not fire: %s", d.reason)
	}

	outside := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	if d := evaluate(p, snapshotAtDay(t, 5, 6), outside, emptyState(), 15); d.fire {
		t.Errorf("14:00 is outside a 00:00–06:00 window but it fired: %s", d.reason)
	}
}

// The end of the window is exclusive. A window written as 00:00–06:00 that fires
// AT 06:00 has one more minute than the person who wrote it granted.
func TestSafeWindowEndIsExclusive(t *testing.T) {
	p := providerSpec{SafeStart: 0, SafeEnd: 6 * 60}
	day := func(h, m int) time.Time { return time.Date(2026, 8, 8, h, m, 0, 0, time.UTC) }

	if !inSafeWindow(p, day(5, 59)) {
		t.Error("05:59 is inside a 00:00–06:00 window")
	}
	if inSafeWindow(p, day(6, 0)) {
		t.Error("06:00 is the exclusive end of a 00:00–06:00 window, so it is outside")
	}
}

// The start of the window is inclusive — the mirror of the assertion above, and
// the one that fails if somebody "fixes" the boundary on the wrong side.
func TestSafeWindowStartIsInclusive(t *testing.T) {
	p := providerSpec{SafeStart: 9 * 60, SafeEnd: 17 * 60}
	if !inSafeWindow(p, time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)) {
		t.Error("09:00 is the inclusive start of a 09:00–17:00 window")
	}
	if inSafeWindow(p, time.Date(2026, 8, 8, 8, 59, 0, 0, time.UTC)) {
		t.Error("08:59 is before a 09:00–17:00 window")
	}
}

func TestWeekdayOnlyWindowIsClosedAtTheWeekend(t *testing.T) {
	p := providerSpec{SafeStart: 0, SafeEnd: 24 * 60, WeekdayOnly: true}
	// 2026-08-08 is a Saturday, 2026-08-09 a Sunday, 2026-08-10 a Monday.
	for _, tc := range []struct {
		day  time.Time
		want bool
	}{
		{time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), true},
	} {
		if got := inSafeWindow(p, tc.day); got != tc.want {
			t.Errorf("%s: inSafeWindow = %v, want %v", tc.day.Weekday(), got, tc.want)
		}
	}
}

// ------------------------------------------------- snapshots we must not trust

// A stale snapshot is worse than no snapshot: it is a real number from a week
// that has already reset, and budgeting against it dispatches into a spent week.
func TestStaleSnapshotDoesNotFire(t *testing.T) {
	lim := snapshotAtDay(t, 5, 6)
	stale := time.Now().Add(-1 * time.Hour).Unix()
	lim.StaleAfter = &stale

	if d := evaluate(allDayProvider(), lim, time.Now(), emptyState(), 15); d.fire {
		t.Fatalf("fired on a snapshot that went stale an hour ago: %s", d.reason)
	}
}

// The mirror: a snapshot still inside its freshness window must not be discarded,
// or the codex provider (whose staleness is the normal case) never fires at all.
func TestFreshSnapshotIsNotTreatedAsStale(t *testing.T) {
	lim := snapshotAtDay(t, 5, 6)
	fresh := time.Now().Add(1 * time.Hour).Unix()
	lim.StaleAfter = &fresh

	if d := evaluate(allDayProvider(), lim, time.Now(), emptyState(), 15); !d.fire {
		t.Fatalf("a snapshot fresh for another hour was rejected: %s", d.reason)
	}
}

func TestMissingSnapshotDoesNotFire(t *testing.T) {
	if d := evaluate(allDayProvider(), nil, time.Now(), emptyState(), 15); d.fire {
		t.Fatalf("fired with no provider snapshot at all: %s", d.reason)
	}
}

func TestMissingWeeklyWindowDoesNotFire(t *testing.T) {
	lim := &providerLimits{Windows: map[string]*limitWindow{"five_hour": {UsedPercent: 5}}}
	if d := evaluate(allDayProvider(), lim, time.Now(), emptyState(), 15); d.fire {
		t.Fatalf("fired with no seven_day window in the snapshot: %s", d.reason)
	}
}

// Without resets_at there is no window start, so there is no elapsed fraction and
// no target. Scoring it anyway would measure the week from the epoch.
func TestWindowWithoutResetsAtDoesNotFire(t *testing.T) {
	lim := &providerLimits{Windows: map[string]*limitWindow{"seven_day": {UsedPercent: 5}}}
	if d := evaluate(allDayProvider(), lim, time.Now(), emptyState(), 15); d.fire {
		t.Fatalf("fired on a window with no resets_at: %s", d.reason)
	}
}

// ------------------------------------------------------------- the curve itself

// elapsedFraction is measured BACKWARDS from resets_at, because that is the only
// timestamp usage-store publishes. Getting the direction wrong reads a fresh week
// as a spent one and suppresses every fire for seven days.
func TestElapsedFractionCountsBackFromTheReset(t *testing.T) {
	now := int64(1_800_000_000)
	for _, tc := range []struct {
		name    string
		resets  int64
		want    float64
		epsilon float64
	}{
		{"the instant the window opened", now + weekSeconds, 0, 0.0001},
		{"halfway through", now + weekSeconds/2, 0.5, 0.0001},
		{"six of seven days in", now + weekSeconds/7, 6.0 / 7.0, 0.0001},
	} {
		got := elapsedFraction(tc.resets, now)
		if diff := got - tc.want; diff > tc.epsilon || diff < -tc.epsilon {
			t.Errorf("%s: elapsedFraction = %.4f, want %.4f", tc.name, got, tc.want)
		}
	}
}

// Both ends are clamped into the curve's domain. A reset already in the past
// would otherwise produce a fraction above 1 and read off the end of the curve.
func TestElapsedFractionStaysInsideTheCurvesDomain(t *testing.T) {
	now := int64(1_800_000_000)
	if got := elapsedFraction(now+2*weekSeconds, now); got != 0 {
		t.Errorf("a reset two weeks out is before the window opened: got %v, want 0", got)
	}
	if got := elapsedFraction(now-weekSeconds, now); got < 0 || got >= 1 {
		t.Errorf("a reset already past must clamp into [0,1): got %v", got)
	}
	// And the clamp's VALUE, not only that it landed in range. 0.999 is the last
	// fraction inside the curve's domain; anything lower quietly budgets the end
	// of the week against an earlier day than it has reached.
	if got := elapsedFraction(now-weekSeconds, now); got != 0.999 {
		t.Errorf("a reset already past clamps to 0.999, got %v", got)
	}
	// A reset exactly now is the instant the window ends: elapsed is exactly one
	// week. It is the only input that separates a clamp written >= from one
	// written >, and the latter reads 1.0 — off the end of the anchor table.
	if got := elapsedFraction(now, now); got != 0.999 {
		t.Errorf("a reset exactly now is the end of the window and clamps to 0.999, got %v", got)
	}
}

// The curve's opening is deliberately flat: the user asked for the first couple
// of days to stay blank, so an early week is not spent on speculative work.
func TestTheCurveStaysBlankForTheFirstTwoDays(t *testing.T) {
	for _, day := range []float64{0, 1, 2} {
		frac := day / 7.0
		if got := interpolate(frac); got != 0 {
			t.Errorf("day %.0f of 7 (frac %.3f): target %.0f%%, want 0%% — the curve's "+
				"opening is flat on purpose", day, frac, got)
		}
	}

	// Where the flat opening ENDS. Days 0–2 above all sit just under that anchor
	// (2/7 is 0.2857), so they redden it if it slides down and cannot see it slide
	// up — the flat stretch could swallow another day with every assertion above
	// still green. This pair straddles it.
	if got := interpolate(0.2855); got != 0 {
		t.Errorf("frac 0.2855 is still inside the flat opening: target %.4f%%, want 0%%", got)
	}
	if got := interpolate(0.2865); got <= 0 {
		t.Errorf("frac 0.2865 is past the end of the flat opening, so the curve has "+
			"begun to ramp: target %.4f%%, want more than 0%%", got)
	}
}

// And it ramps after that. A curve that stayed flat would never dispatch anything.
func TestTheCurveRampsToFullByTheReset(t *testing.T) {
	previous := -1.0
	for _, day := range []float64{3, 4, 5, 6, 7} {
		got := interpolate(day / 7.0)
		if got <= previous {
			t.Fatalf("day %.0f of 7: target %.0f%% did not rise above the previous %.0f%%",
				day, got, previous)
		}
		previous = got
	}
	if got := interpolate(1.0); got != 100 {
		t.Errorf("at the reset the target is 100%%, got %.0f%%", got)
	}
}

// Between two anchors the target is interpolated, not snapped to either end.
// Snapping to the lower anchor lags a whole day; snapping to the upper runs a day
// ahead and spends the week early.
func TestTheCurveInterpolatesBetweenAnchors(t *testing.T) {
	// The anchors either side of 0.50 are (0.429, 25) and (0.571, 50).
	got := interpolate(0.50)
	if got <= 25 || got >= 50 {
		t.Fatalf("target at frac 0.50 is %.2f%%; it must lie strictly between the "+
			"25%% and 50%% anchors, not snap to one", got)
	}
}

// Off the end of the curve in either direction the target is the nearest anchor,
// never a wrapped or zero value.
func TestTheCurveClampsOutsideItsAnchors(t *testing.T) {
	if got := interpolate(-1); got != 0 {
		t.Errorf("below the first anchor the target is the first anchor: got %.0f%%", got)
	}
	if got := interpolate(2); got != 100 {
		t.Errorf("above the last anchor the target is the last anchor: got %.0f%%", got)
	}
}
