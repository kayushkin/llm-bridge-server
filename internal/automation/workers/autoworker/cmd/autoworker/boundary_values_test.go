package main

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

// The numbers in this binary, as opposed to the mechanisms that read them.
//
// The case list scored 68/73 with both controls behaving and printed "every real
// mechanism is pinned", truthfully. It says nothing about the VALUES those
// mechanisms compare against: every gate had a test, and most of those tests
// built their own provider spec, so the hours and cooldowns the fleet actually
// runs on could each move a unit with the suite green.
//
// Two rules the fixtures below are written to:
//
//   - Straddle, in literals. A pair one minute / one notch either side of the
//     cut, so the value reddens a row whichever way it moves. Spelled as
//     constants in the test, never as `p.Cooldown - time.Minute`: a fixture
//     written in terms of the number it would have to pin moves with that number
//     and can never fail for a wrong value of it.
//   - Read the SHIPPED spec. The gate tests in fire_test.go each build their own
//     providerSpec so that some other gate cannot decide the outcome. That is
//     right for a mechanism test and it is why nothing here can be left to them.

// shippedProvider is the spec the binary actually runs on, looked up by name.
// A test that built its own would pin the fixture rather than the fleet.
func shippedProvider(t *testing.T, name string) providerSpec {
	t.Helper()
	for _, p := range providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no shipped provider is named %q — the two providers ARE the thing this "+
		"file pins, so a rename is a finding rather than a fixture problem", name)
	return providerSpec{}
}

// codexSnapshot mirrors snapshotAtDay for codex, whose window is keyed "weekly"
// rather than "seven_day". Written out rather than derived from the spec, for
// the same reason: reading p.WindowKey here would make the fixture agree with
// whatever the code says.
func codexSnapshot(t *testing.T, usedPercent, day float64) *providerLimits {
	t.Helper()
	resets := resetsAtForDay(time.Now(), day)
	return &providerLimits{
		Provider: "codex",
		Windows: map[string]*limitWindow{
			"weekly": {UsedPercent: usedPercent, ResetsAt: &resets},
		},
		SnapshotAt: time.Now().Unix(),
	}
}

// ------------------------------------------------------ the shipped windows

// The hours each provider may start a session in. codex's window is the
// overnight one — the whole reason it exists is that codex sessions must not
// start in the middle of the working day — and anthropic's is the whole day, to
// catch up before the weekly reset.
func TestTheShippedSafeWindowsAreTheHoursTheDesignGranted(t *testing.T) {
	// A Monday, so a WeekdayOnly spec could not be what decides these.
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 8, 10, hour, minute, 0, 0, time.UTC)
	}

	codex := shippedProvider(t, "codex")
	if !inSafeWindow(codex, at(5, 59)) {
		t.Error("05:59 is inside codex's overnight window, which closes at 06:00")
	}
	if inSafeWindow(codex, at(6, 0)) {
		t.Error("06:00 is the exclusive end of codex's overnight window — a codex " +
			"session started after it runs into the working day")
	}

	anthropic := shippedProvider(t, "anthropic")
	if !inSafeWindow(anthropic, at(0, 0)) {
		t.Error("anthropic's window is the whole day, so it opens at 00:00")
	}
	// 23:59 is the last minute of the day, and the only fixture that can tell a
	// whole-day window from one that closes a minute early. There is no fixture
	// on the other side: minutesOfDay cannot exceed 23*60+59, so widening the end
	// past 24*60 is unobservable by construction rather than untested.
	if !inSafeWindow(anthropic, at(23, 59)) {
		t.Error("23:59 is inside anthropic's whole-day window; the catch-up hour " +
			"before the reset is the one this provider exists for")
	}
}

// ---------------------------------------------------- the shipped cooldowns

// The cooldown is the gap between sessions, and it is the difference between one
// under-used week and a stream of dispatches. Both providers' values are pinned
// by a straddling pair, because the existing cooldown test drives its fixture
// from p.Cooldown itself and so holds at any value of it.
//
// ⚠️ The pair is spaced by HALF a unit either side, not a whole one, and that is
// the whole of why it works. The first draft used 14m and 16m around a 15-minute
// cut and caught the cut narrowing but not it widening: at a cooldown of 16m the
// 16m fixture sits exactly ON the new boundary, and `elapsed < cooldown` sends a
// tie the permissive way, so it still fired and the assertion still held. A
// fixture has to land strictly BETWEEN the right value and the wrong one, which
// for a value counted in whole minutes means offsetting by thirty seconds.
func TestTheShippedCooldownsAreTheGapsBetweenSessions(t *testing.T) {
	anthropic := shippedProvider(t, "anthropic")
	firedAgo := func(p providerSpec, d time.Duration) *state {
		return &state{LastFire: map[string]int64{p.Name: time.Now().Add(-d).Unix()}}
	}

	if d := evaluate(anthropic, snapshotAtDay(t, 5, 6), time.Now(),
		firedAgo(anthropic, 14*time.Minute+30*time.Second), 15); d.fire {
		t.Errorf("anthropic fired 14m30s after its last session; its cooldown is "+
			"15 minutes: %s", d.reason)
	}
	if d := evaluate(anthropic, snapshotAtDay(t, 5, 6), time.Now(),
		firedAgo(anthropic, 15*time.Minute+30*time.Second), 15); !d.fire {
		t.Errorf("anthropic did not fire 15m30s after its last session, which is "+
			"past its 15-minute cooldown: %s", d.reason)
	}

	// codex is gated to the overnight window as well, so its decisions are taken
	// at 03:00 — inside the window, and not what is being tested here.
	codex := shippedProvider(t, "codex")
	overnight := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)

	if d := evaluate(codex, codexSnapshot(t, 5, 6), overnight,
		firedAgo(codex, 119*time.Minute+30*time.Second), 15); d.fire {
		t.Errorf("codex fired 1h59m30s after its last session; its cooldown is "+
			"2 hours: %s", d.reason)
	}
	if d := evaluate(codex, codexSnapshot(t, 5, 6), overnight,
		firedAgo(codex, 120*time.Minute+30*time.Second), 15); !d.fire {
		t.Errorf("codex did not fire 2h00m30s after its last session, which is past "+
			"its 2-hour cooldown: %s", d.reason)
	}
}

// --------------------------------------------------------- the target curve

// Every anchor's target percentage, quoted in full. The existing curve tests
// assert that it starts flat, rises, interpolates between anchors and clamps at
// both ends — all four hold with every percentage on the curve moved a point,
// because none of them names a number the curve is supposed to produce.
func TestEachCurveAnchorCarriesTheTargetItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		frac float64
		want float64
	}{
		{0.00, 0},   // day 0 of 7 — blank
		{0.286, 0},  // ~day 2 — still blank
		{0.429, 25}, // ~day 3 — start budgeting
		{0.571, 50}, // ~day 4 — half
		{0.714, 70}, // ~day 5
		{0.857, 85}, // ~day 6
		{1.00, 100}, // reset — full
	} {
		if got := interpolate(tc.frac); got != tc.want {
			t.Errorf("interpolate(%.3f) = %.4f%%, want %.0f%% — this is the budget the "+
				"fleet is told to have spent by that point in the week", tc.frac, got, tc.want)
		}
	}
}

// ------------------------------------------------------------ the fire gate

// The slack band's edge is inclusive: usage LEVEL with the target-minus-slack
// line is "within slack" and does not fire. One notch stricter and the quietest
// possible week — a blank target, a blank reading and no slack — dispatches a
// session.
//
// Day 1 of 7 is inside the curve's flat opening, so the target is 0%. With 0pp
// of slack the edge is 0% too, which is the only arrangement that puts a reading
// exactly ON it without the fixture computing the curve for itself.
func TestUsageExactlyOnTheSlackEdgeDoesNotFire(t *testing.T) {
	if d := evaluate(allDayProvider(), snapshotAtDay(t, 0, 1), time.Now(), emptyState(), 0); d.fire {
		t.Errorf("usage sitting exactly on the band edge fired: %s", d.reason)
	}
}

// ------------------------------------------------------- the spend ceiling

// A card whose tree has spent EXACTLY its ceiling, to the cent.
//
// This is the boundary the whole spend mechanism is built around and the one
// input no fixture reached: the existing spent-out test overshoots by a dollar
// ($6.00 against a $5.00 ceiling), so it holds just as firmly if the guard stops
// treating "exactly spent" as spent.
//
// Being one notch out here is not a rounding error. budgetForCard would return
// (0, dispatchable) — and a cap of 0 is the one number that means UNLIMITED by
// the time it reaches the harness, because llm-bridge-claudecode omits
// --max-budget-usd when the value is zero. The tightest imaginable ceiling and
// no ceiling at all are the same wire value, so a task that had just reached its
// limit would be dispatched with no limit, and the log line would read like a
// session that simply ran long.
func TestACardWhoseSpendExactlyEqualsItsCeilingIsHeld(t *testing.T) {
	f := &fakeKanban{
		queued: []issuesclient.CardView{
			card("exact", map[string]any{"title": "spent to the cent", "auto_hold_at_usd": 5.0}),
		},
		links: map[string][]issuesclient.CardLink{
			"exact": {{CardID: "exact", EntityType: "session", EntityRef: "sess-1"}},
		},
	}
	qc := f.start(t)
	nb, _ := serveNoteboardItems(t, []map[string]any{
		{"id": "exact", "auto_hold_at_usd": 5.0},
	})
	cfg := pointAtFakes(t, serveBridgeAggregates(t, map[string]float64{"sess-1": 5.00}), nb)

	got, err := selectQueuedCard(qc, cfg, nil)
	if err != nil {
		t.Fatalf("selectQueuedCard: %v", err)
	}
	if got != nil {
		t.Fatalf("dispatched %q at a cap of $%.2f; it has spent exactly its $5.00 "+
			"ceiling, and a cap of $0.00 reaches the harness as no cap at all",
			got.cardID, got.maxBudgetUSD)
	}
	if held := f.heldCards(); len(held) != 1 || held[0] != "exact" {
		t.Errorf("the exactly-spent card was not parked (held=%v)", held)
	}
}
