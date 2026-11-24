package timeaccounting

import (
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
)

func event(id string, at time.Time, kind model.EventKind, state model.ClockState) model.CardEvent {
	return model.CardEvent{ID: id, CardID: "card", Kind: kind, ClockState: state, OccurredAt: at, RecordedAt: at}
}

// TestPausedTimeDoesNotBurnTheBudget replays the case this feature was built for:
// a priority job arrives, half an hour of work happens, it waits a day on someone
// else's reply, then an hour finishes it. Twenty-five and a half hours pass, the
// limit is two, and the work was only available for one and a half — so the card
// is inside its limit despite having been open for a day.
func TestPausedTimeDoesNotBurnTheBudget(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	budget := 2 * 3600
	summary, timeline := Compute(Input{
		Events: []model.CardEvent{
			event("1", start, model.EventEmailReceived, model.ClockRunning),
			event("2", start.Add(30*time.Minute), model.EventWaitingStarted, model.ClockPaused),
			event("3", start.Add(30*time.Minute+24*time.Hour), model.EventEmailReceived, model.ClockRunning),
			event("4", start.Add(90*time.Minute+24*time.Hour), model.EventCardCompleted, model.ClockStopped),
		},
		Level: &model.BoardPriorityLevel{PriorityValue: 5, Label: "P0", BudgetSeconds: &budget},
		Now:   start.Add(72 * time.Hour), // long after completion; must not matter
	})

	if got, want := summary.ElapsedSeconds, 25.5*3600; got != want {
		t.Errorf("elapsed = %v, want %v", got, want)
	}
	if got, want := summary.BudgetClockSeconds, 1.5*3600; got != want {
		t.Errorf("budget clock = %v, want %v", got, want)
	}
	if got, want := summary.WaitingSeconds, 24.0*3600; got != want {
		t.Errorf("waiting = %v, want %v", got, want)
	}
	if summary.OverBudget == nil || *summary.OverBudget {
		t.Errorf("over budget = %v, want false: 1.5h of work against a 2h limit", summary.OverBudget)
	}
	if got, want := *summary.BudgetRemainingSeconds, 0.5*3600; got != want {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	if summary.PriorityLabel != "P0" {
		t.Errorf("label = %q, want P0", summary.PriorityLabel)
	}
	if len(timeline) != 4 {
		t.Fatalf("timeline has %d entries, want 4", len(timeline))
	}
	if got, want := timeline[2].SecondsSincePreviousEvent, 24.0*3600; got != want {
		t.Errorf("gap before the reply = %v, want %v", got, want)
	}
	if timeline[1].CountsAgainstBudget {
		t.Error("the waiting segment must not count against the budget")
	}
}

// TestStoppedCardsStopAccruing pins that a finished card's figures freeze. Without
// it the last segment would run to now and every completed card would drift over
// its limit days later.
func TestStoppedCardsStopAccruing(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	summary, _ := Compute(Input{
		Events: []model.CardEvent{
			event("1", start, model.EventCardCreated, model.ClockRunning),
			event("2", start.Add(time.Hour), model.EventCardCompleted, model.ClockStopped),
		},
		Now: start.Add(100 * time.Hour),
	})
	if got, want := summary.ElapsedSeconds, 3600.0; got != want {
		t.Errorf("elapsed = %v, want %v", got, want)
	}
	if got, want := summary.BudgetClockSeconds, 3600.0; got != want {
		t.Errorf("budget clock = %v, want %v", got, want)
	}
}

// TestOpenSegmentRunsToNow pins the opposite: an unfinished card keeps counting.
func TestOpenSegmentRunsToNow(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	summary, timeline := Compute(Input{
		Events: []model.CardEvent{event("1", start, model.EventCardCreated, model.ClockRunning)},
		Now:    start.Add(3 * time.Hour),
	})
	if got, want := summary.BudgetClockSeconds, 3*3600.0; got != want {
		t.Errorf("budget clock = %v, want %v", got, want)
	}
	if !timeline[0].SegmentOpen {
		t.Error("the last segment of an unfinished card must be open")
	}
}

// TestNoEventsReportsNothing pins that a card nothing has happened to reports
// zeroes rather than inventing an age from a row timestamp.
func TestNoEventsReportsNothing(t *testing.T) {
	summary, timeline := Compute(Input{Now: time.Now()})
	if summary.EventCount != 0 || summary.ElapsedSeconds != 0 || summary.FirstEventAt != nil {
		t.Errorf("card with no events reported %+v", summary)
	}
	if len(timeline) != 0 {
		t.Errorf("timeline = %d entries, want 0", len(timeline))
	}
}

// TestUnrankedCardsGetNoBudget pins that a card at the default priority is not
// silently treated as the top of the ladder.
func TestUnrankedCardsGetNoBudget(t *testing.T) {
	ladder := &model.PriorityLadder{Levels: []model.BoardPriorityLevel{{PriorityValue: 5, Label: "P0"}}}
	if lv := ladder.LevelFor(model.UnsetPriorityValue); lv != nil {
		t.Errorf("unranked card matched level %+v", lv)
	}
	summary, _ := Compute(Input{Events: []model.CardEvent{
		event("1", time.Now().Add(-time.Hour), model.EventCardCreated, model.ClockRunning),
	}, Level: ladder.LevelFor(model.UnsetPriorityValue)})
	if summary.BudgetSeconds != nil || summary.OverBudget != nil {
		t.Errorf("unranked card got a budget: %+v", summary)
	}
}

// TestBusinessHoursCountOnlyTheWorkingWeek walks a Friday-evening arrival through
// to Monday: three days elapsed, but only Monday morning is business time.
func TestBusinessHoursCountOnlyTheWorkingWeek(t *testing.T) {
	hours := &model.BusinessHours{TZID: "America/Los_Angeles", Days: []string{"MO", "TU", "WE", "TH", "FR"}, Start: "09:00", End: "17:00"}
	if err := hours.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	loc, _ := time.LoadLocation(hours.TZID)
	friday := time.Date(2026, 8, 21, 18, 0, 0, 0, loc) // Friday, after hours
	monday := time.Date(2026, 8, 24, 11, 0, 0, 0, loc) // Monday, two hours in

	if got, want := BusinessSecondsBetween(friday, monday, hours), 2*3600.0; got != want {
		t.Errorf("business seconds = %v, want %v", got, want)
	}
	summary, _ := Compute(Input{
		Events: []model.CardEvent{
			event("1", friday, model.EventEmailReceived, model.ClockRunning),
			event("2", monday, model.EventCardCompleted, model.ClockStopped),
		},
		Hours: hours,
	})
	if got, want := summary.ElapsedSeconds, 65*3600.0; got != want {
		t.Errorf("elapsed = %v, want %v", got, want)
	}
	if summary.BusinessHoursElapsedSeconds == nil {
		t.Fatal("business hours elapsed missing on a board that has hours")
	}
	if got, want := *summary.BusinessHoursElapsedSeconds, 2*3600.0; got != want {
		t.Errorf("business elapsed = %v, want %v", got, want)
	}
}

// TestBusinessHoursAbsentWithoutConfiguration pins that an unconfigured board
// reports no business figures at all rather than a guessed nine-to-five.
func TestBusinessHoursAbsentWithoutConfiguration(t *testing.T) {
	summary, _ := Compute(Input{Events: []model.CardEvent{
		event("1", time.Now().Add(-time.Hour), model.EventCardCreated, model.ClockRunning),
	}})
	if summary.BusinessHoursElapsedSeconds != nil || summary.BusinessHoursBudgetClockSeconds != nil {
		t.Errorf("business figures appeared without configuration: %+v", summary)
	}
}

// TestDaylightSavingIsCountedAsItHappened pins the reason business hours carry a
// zone and not an offset: the Sunday the clocks go forward is 23 hours long, and
// the Monday after it is still a full working day.
func TestDaylightSavingIsCountedAsItHappened(t *testing.T) {
	hours := &model.BusinessHours{TZID: "America/Los_Angeles", Days: []string{"MO"}, Start: "09:00", End: "17:00"}
	loc, _ := time.LoadLocation(hours.TZID)
	before := time.Date(2027, 3, 13, 12, 0, 0, 0, loc) // Saturday before the spring change
	after := time.Date(2027, 3, 15, 17, 0, 0, 0, loc)  // Monday close
	if got, want := BusinessSecondsBetween(before, after, hours), 8*3600.0; got != want {
		t.Errorf("business seconds across the change = %v, want %v", got, want)
	}
}

// TestOutOfOrderBackdatingNeverSubtracts pins that a backdated event cannot push
// a total negative.
func TestOutOfOrderBackdatingNeverSubtracts(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	summary, timeline := Compute(Input{
		Events: []model.CardEvent{
			event("2", start.Add(time.Hour), model.EventNoteAdded, model.ClockRunning),
			event("1", start, model.EventEmailReceived, model.ClockRunning),
		},
		Now: start.Add(2 * time.Hour),
	})
	if summary.ElapsedSeconds < 0 || summary.BudgetClockSeconds != 2*3600 {
		t.Errorf("summary = %+v, want 2h on the clock", summary)
	}
	if timeline[0].Event.ID != "1" {
		t.Errorf("timeline starts with %q, want the earlier event", timeline[0].Event.ID)
	}
}

// TestLadderRejectsLevelAtZero pins the rule that keeps "unranked" and "most
// urgent" apart: 0 is reserved, so P0 is the highest value on the board and never
// the literal zero.
func TestLadderRejectsLevelAtZero(t *testing.T) {
	req := &model.SetPriorityLadderRequest{Levels: []model.BoardPriorityLevel{{PriorityValue: 0, Label: "P0"}}}
	if err := req.Validate(); err == nil {
		t.Fatal("a level at priority_value 0 was accepted; unranked cards would become P0")
	}
}

// A ticket arrives late on Wednesday before a Thursday holiday. Thursday is a
// working day of the week and counts for nothing; Friday morning does.
func TestAHolidayCountsForNothingThoughItsWeekdayIsWorked(t *testing.T) {
	hours := &model.BusinessHours{
		TZID: "America/Los_Angeles", Days: []string{"MO", "TU", "WE", "TH", "FR"}, Start: "09:00", End: "17:00",
		Holidays: []string{"2026-11-26"},
	}
	if err := hours.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	loc, _ := time.LoadLocation(hours.TZID)
	wednesdayEvening := time.Date(2026, 11, 25, 16, 0, 0, 0, loc) // one working hour left
	fridayMorning := time.Date(2026, 11, 27, 10, 0, 0, 0, loc)    // one hour into Friday

	if got, want := BusinessSecondsBetween(wednesdayEvening, fridayMorning, hours), 2*3600.0; got != want {
		t.Errorf("business seconds across the holiday = %v, want %v (Thursday's eight hours must not count)", got, want)
	}
	withoutTheHoliday := *hours
	withoutTheHoliday.Holidays = nil
	if got, want := BusinessSecondsBetween(wednesdayEvening, fridayMorning, &withoutTheHoliday), 10*3600.0; got != want {
		t.Errorf("the same range with no holiday = %v, want %v", got, want)
	}

	// The holiday is a date in the board's zone. 07:00 UTC on the 27th is still
	// 23:00 on the 26th in Los Angeles: a range read in UTC must not end it early.
	endOfHolidayInUTC := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	startOfHolidayInUTC := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC) // 00:00 in Los Angeles
	if got := BusinessSecondsBetween(startOfHolidayInUTC, endOfHolidayInUTC, hours); got != 0 {
		t.Errorf("business seconds inside the holiday = %v, want 0", got)
	}
}

func TestHolidaysThatCannotBeReadAreRefused(t *testing.T) {
	week := model.BusinessHours{TZID: "America/Los_Angeles", Days: []string{"MO"}, Start: "09:00", End: "17:00"}
	for what, holidays := range map[string][]string{
		"not a date":         {"Thanksgiving"},
		"a date with a time": {"2026-11-26T00:00:00Z"},
		"an unpadded date":   {"2026-1-5"},
		"a day that is not":  {"2026-02-30"},
		"the same day twice": {"2026-12-25", "2026-12-25"},
	} {
		hours := week
		hours.Holidays = holidays
		if err := hours.Validate(); err == nil {
			t.Errorf("%s (%v) was accepted", what, holidays)
		}
	}
}
