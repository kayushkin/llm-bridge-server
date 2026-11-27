package model

import (
	"testing"
	"time"
)

func mustLoadPacific(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

// The headline case. "Every 3rd Tuesday" is the rule that motivated this field,
// and it is also the rule that is easiest to write subtly wrong — BYDAY=3TU and
// BYDAY=TU;BYSETPOS=3 read alike and diverge in months where the 1st is a
// Tuesday. Pin the actual instants.
func TestEveryThirdTuesdayExpandsToTheThirdTuesday(t *testing.T) {
	loc := mustLoadPacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc) // 3rd Tue of July 2026
	sched := &Schedule{
		DTStart: &anchor,
		TZID:    "America/Los_Angeles",
		RRule:   "FREQ=MONTHLY;BYDAY=3TU",
	}
	if err := sched.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	got, err := sched.DueOccurrences(nil,
		time.Date(2026, 7, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 11, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("DueOccurrences: %v", err)
	}

	want := []time.Time{
		time.Date(2026, 7, 21, 9, 0, 0, 0, loc),
		time.Date(2026, 8, 18, 9, 0, 0, 0, loc),
		time.Date(2026, 9, 15, 9, 0, 0, 0, loc),
		time.Date(2026, 10, 20, 9, 0, 0, 0, loc),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("occurrence %d = %s, want %s", i, got[i], want[i])
		}
		if wd := got[i].Weekday(); wd != time.Tuesday {
			t.Errorf("occurrence %d landed on %s, not Tuesday", i, wd)
		}
	}
}

// The reason TZID is mandatory. The cron nudges this replaces are pinned in UTC,
// so they drift an hour when the clocks move. A rule expanded in a named zone
// must hold the WALL CLOCK time across a DST boundary — 9am in October and 9am
// in November, not 9am then 8am.
func TestRecurrenceHoldsWallClockTimeAcrossDST(t *testing.T) {
	loc := mustLoadPacific(t)
	anchor := time.Date(2026, 10, 20, 9, 0, 0, 0, loc) // PDT
	sched := &Schedule{
		DTStart: &anchor,
		TZID:    "America/Los_Angeles",
		RRule:   "FREQ=MONTHLY;BYDAY=3TU",
	}

	got, err := sched.DueOccurrences(nil,
		time.Date(2026, 10, 1, 0, 0, 0, 0, loc),
		time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("DueOccurrences: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("want at least Oct + Nov occurrences, got %v", got)
	}

	// US DST ended 2026-11-01, so the November occurrence is PST while October's
	// is PDT. Both must still read as 9am local.
	for _, occurrence := range got {
		if h, m := occurrence.Hour(), occurrence.Minute(); h != 9 || m != 0 {
			t.Errorf("%s is %02d:%02d local, want 09:00 — DST drift", occurrence, h, m)
		}
	}

	octOffset := got[0].Format("-07:00")
	novOffset := got[1].Format("-07:00")
	if octOffset == novOffset {
		t.Fatalf("Oct and Nov have the same UTC offset (%s) — the test is not actually crossing a DST boundary", octOffset)
	}
}

// An item with only RDATE is "these specific dates and no others" — the user
// asked for this explicitly alongside cadences.
func TestSpecificDatesWithoutARule(t *testing.T) {
	loc := mustLoadPacific(t)
	d1 := time.Date(2026, 8, 14, 9, 0, 0, 0, loc)
	d2 := time.Date(2026, 11, 2, 9, 0, 0, 0, loc)
	anchor := d1
	sched := &Schedule{DTStart: &anchor, TZID: "America/Los_Angeles", RDate: []time.Time{d1, d2}}

	if err := sched.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !sched.HasDueRecurrence() {
		t.Fatal("RDate-only schedule should count as having a due recurrence")
	}

	got, err := sched.DueOccurrences(nil,
		time.Date(2026, 1, 1, 0, 0, 0, 0, loc),
		time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("DueOccurrences: %v", err)
	}
	if len(got) != 2 || !got[0].Equal(d1) || !got[1].Equal(d2) {
		t.Fatalf("got %v, want exactly [%s %s]", got, d1, d2)
	}
}

func TestExDateRemovesAnOccurrence(t *testing.T) {
	loc := mustLoadPacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	skipped := time.Date(2026, 8, 18, 9, 0, 0, 0, loc)
	sched := &Schedule{
		DTStart: &anchor,
		TZID:    "America/Los_Angeles",
		RRule:   "FREQ=MONTHLY;BYDAY=3TU",
		ExDate:  []time.Time{skipped},
	}

	got, err := sched.DueOccurrences(nil,
		time.Date(2026, 7, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("DueOccurrences: %v", err)
	}
	for _, occurrence := range got {
		if occurrence.Equal(skipped) {
			t.Fatalf("ExDate %s was still produced", skipped)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d occurrences %v, want 2 (July + September, August excluded)", len(got), got)
	}
}

// The car-title shape: a one-shot task with a due date and a weekday nag. The nag
// anchors on the item's due_at, because restating the date as a dtstart would be
// a second copy of the same fact.
func TestNagAnchorsOnItemDueDate(t *testing.T) {
	loc := mustLoadPacific(t)
	due := time.Date(2026, 6, 30, 9, 0, 0, 0, loc)
	sched := &Schedule{
		TZID:   "America/Los_Angeles",
		Remind: &Remind{Nag: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10;BYMINUTE=0;BYSECOND=0"},
	}
	if err := sched.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sched.HasDueRecurrence() {
		t.Fatal("a nag-only schedule must not claim a due recurrence — it would make the coordinator regenerate the task")
	}

	got, err := sched.NagOccurrences(&due,
		time.Date(2026, 7, 6, 0, 0, 0, 0, loc), // a Monday
		time.Date(2026, 7, 13, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("NagOccurrences: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d nag firings %v, want 5 (Mon-Fri)", len(got), got)
	}
	for _, occurrence := range got {
		if wd := occurrence.Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Errorf("nag fired on %s — weekend", wd)
		}
		if occurrence.Hour() != 10 {
			t.Errorf("nag fired at %02d:00, want 10:00", occurrence.Hour())
		}
	}
}

// RDate and ExDate describe the DUE series. Expanding the nag rule through them
// too meant one extra due date bought one extra nag, and a skipped due week
// silently skipped that week's nagging — neither of which the user wrote. This
// reaches real reminders through the coordinator, not just the preview, so it is
// pinned on both rules at once.
func TestDueDateOverridesDoNotMoveTheNag(t *testing.T) {
	loc := mustLoadPacific(t)
	anchor := time.Date(2026, 8, 10, 9, 0, 0, 0, loc)
	extra := time.Date(2026, 12, 25, 9, 0, 0, 0, loc)
	sched := &Schedule{
		DTStart: &anchor,
		TZID:    "America/Los_Angeles",
		RRule:   "FREQ=DAILY;COUNT=2",
		RDate:   []time.Time{extra},
		Remind:  &Remind{Nag: "FREQ=DAILY;COUNT=2"},
	}
	if err := sched.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)

	// The due series keeps its extra date: that is what RDate is for.
	due, err := sched.DueOccurrences(nil, from, to)
	if err != nil {
		t.Fatalf("DueOccurrences: %v", err)
	}
	if len(due) != 3 {
		t.Fatalf("got %d due occurrences %v, want 3 (COUNT=2 plus the RDate)", len(due), due)
	}
	if !due[2].Equal(extra) {
		t.Errorf("last due occurrence = %v, want the RDate %v", due[2], extra)
	}

	// The nag rule says COUNT=2 and must fire exactly twice.
	nag, err := sched.NagOccurrences(nil, from, to)
	if err != nil {
		t.Fatalf("NagOccurrences: %v", err)
	}
	if len(nag) != 2 {
		t.Fatalf("got %d nag firings %v, want 2 — the due RDate leaked into the nag rule", len(nag), nag)
	}
	for _, occurrence := range nag {
		if occurrence.Equal(extra) {
			t.Errorf("nag fired on the due-only RDate %v", extra)
		}
	}

	// Same edge from the other side: an ExDate skips a due date, not a nag.
	sched.RDate = nil
	sched.ExDate = []time.Time{anchor}
	nag, err = sched.NagOccurrences(nil, from, to)
	if err != nil {
		t.Fatalf("NagOccurrences: %v", err)
	}
	if len(nag) != 2 {
		t.Fatalf("got %d nag firings %v after an ExDate, want 2 — the due ExDate suppressed a nag", len(nag), nag)
	}
}

// A finite series has to actually end, otherwise a rolling item never retires.
func TestSeriesEndsAndNextOccurrenceGoesNil(t *testing.T) {
	loc := mustLoadPacific(t)
	anchor := time.Date(2026, 7, 21, 9, 0, 0, 0, loc)
	sched := &Schedule{
		DTStart: &anchor,
		TZID:    "America/Los_Angeles",
		RRule:   "FREQ=MONTHLY;BYDAY=3TU;COUNT=2",
	}

	next, err := sched.NextDueOccurrence(nil, time.Date(2026, 7, 22, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("NextDueOccurrence: %v", err)
	}
	if next == nil || !next.Equal(time.Date(2026, 8, 18, 9, 0, 0, 0, loc)) {
		t.Fatalf("next = %v, want 2026-08-18 09:00 (the 2nd and final occurrence)", next)
	}

	// Past the end of a COUNT=2 series there is nothing left.
	exhausted, err := sched.NextDueOccurrence(nil, time.Date(2026, 8, 19, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("NextDueOccurrence: %v", err)
	}
	if exhausted != nil {
		t.Fatalf("series with COUNT=2 produced a 3rd occurrence %s", exhausted)
	}
}

// Every one of these is a rule that would otherwise expand to nothing and
// silently remind nobody. They must be rejected at write time, loudly.
func TestValidateRejectsRulesThatWouldSilentlyDoNothing(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		sched *Schedule
	}{
		{"rrule with no tzid", &Schedule{DTStart: &now, RRule: "FREQ=MONTHLY;BYDAY=3TU"}},
		{"rrule with no dtstart", &Schedule{TZID: "America/Los_Angeles", RRule: "FREQ=MONTHLY;BYDAY=3TU"}},
		{"unparseable rrule", &Schedule{DTStart: &now, TZID: "America/Los_Angeles", RRule: "EVERY THIRD TUESDAY"}},
		{"unparseable nag", &Schedule{TZID: "America/Los_Angeles", Remind: &Remind{Nag: "WEEKDAYS PLZ"}}},
		{"unknown tzid", &Schedule{DTStart: &now, TZID: "Mars/Olympus_Mons", RRule: "FREQ=DAILY"}},
		{"bogus mode", &Schedule{DTStart: &now, TZID: "America/Los_Angeles", RRule: "FREQ=DAILY", Mode: "sometimes"}},
		{"bogus lead", &Schedule{DTStart: &now, TZID: "America/Los_Angeles", RRule: "FREQ=DAILY", Remind: &Remind{Lead: []string{"1 day"}}}},
		{"bogus channel", &Schedule{DTStart: &now, TZID: "America/Los_Angeles", RRule: "FREQ=DAILY", Remind: &Remind{Channels: []string{"carrier-pigeon"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.sched.Validate(); err == nil {
				t.Fatalf("Validate accepted %s — it would expand to nothing and remind no one", tc.name)
			}
		})
	}
}

// A rule with nothing to count from is the same silent failure, but it can only
// be caught once the item is in hand.
func TestAnchorRefusesToInventOne(t *testing.T) {
	sched := &Schedule{TZID: "America/Los_Angeles", Remind: &Remind{Nag: "FREQ=DAILY"}}
	if _, err := sched.Anchor(nil); err == nil {
		t.Fatal("Anchor invented an anchor from nothing; a rule that re-anchors on every run fires on the wrong day")
	}

	due := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	got, err := sched.Anchor(&due)
	if err != nil {
		t.Fatalf("Anchor: %v", err)
	}
	if !got.Equal(due) {
		t.Fatalf("Anchor = %s, want the item's due date %s", got, due)
	}
}

func TestInstanceIsTheDefaultMode(t *testing.T) {
	sched := &Schedule{}
	if got := sched.EffectiveMode(); got != ScheduleModeInstance {
		t.Fatalf("EffectiveMode = %q, want %q", got, ScheduleModeInstance)
	}
}

func TestDigestIsTheDefaultChannel(t *testing.T) {
	var remind *Remind
	if !remind.DeliversOn(ChannelDigest) {
		t.Error("a nil Remind should still reach the digest — the digest is the primary surface")
	}
	if remind.DeliversOn(ChannelCalendar) {
		t.Error("calendar must be opt-in, not implied")
	}

	explicit := &Remind{Channels: []string{ChannelCalendar}}
	if explicit.DeliversOn(ChannelDigest) {
		t.Error("an explicit channel list must not silently also mean digest")
	}
	if !explicit.DeliversOn(ChannelCalendar) {
		t.Error("explicit calendar channel not honored")
	}
}

func TestParseISO8601Duration(t *testing.T) {
	valid := map[string]ISO8601Duration{
		"-P1D":     {Negative: true, Days: 1},
		"-PT2H":    {Negative: true, Hours: 2},
		"-P1W":     {Negative: true, Weeks: 1},
		"P3D":      {Days: 3},
		"-P1DT12H": {Negative: true, Days: 1, Hours: 12},
		"-PT30M":   {Negative: true, Minutes: 30},
		"PT45S":    {Seconds: 45},
	}
	for in, want := range valid {
		got, err := ParseISO8601Duration(in)
		if err != nil {
			t.Errorf("ParseISO8601Duration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseISO8601Duration(%q) = %+v, want %+v", in, got, want)
		}
	}

	invalid := []string{"", "1D", "P", "PT", "-P", "P1M", "P1Y", "-P0D", "P1X", "PD", "-PT1H30"}
	for _, in := range invalid {
		if _, err := ParseISO8601Duration(in); err == nil {
			t.Errorf("ParseISO8601Duration(%q) was accepted; want an error", in)
		}
	}
}

// "One day before" and "24 hours before" are the same thing 363 days a year. This
// asserts the two days they are not — a lead offset must preserve the wall-clock
// time of day across a DST transition, or a "remind me the morning before" lands
// an hour off.
func TestLeadOffsetIsCalendarAwareNotElapsedTime(t *testing.T) {
	loc := mustLoadPacific(t)
	oneDayBefore, err := ParseISO8601Duration("-P1D")
	if err != nil {
		t.Fatalf("ParseISO8601Duration: %v", err)
	}

	// US DST falls back at 02:00 on 2026-11-01, so that calendar day is 25 hours
	// long. A due date ON Nov 1 (PST), minus "one day", lands on Oct 31 (PDT) —
	// the one span that actually crosses the transition.
	due := time.Date(2026, 11, 1, 9, 0, 0, 0, loc)
	got := oneDayBefore.ApplyTo(due)

	want := time.Date(2026, 10, 31, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("-P1D before %s = %s, want %s (same wall clock, one calendar day earlier)", due, got, want)
	}
	if got.Hour() != 9 {
		t.Fatalf("lead landed at %02d:00 local, want 09:00 — this is the DST bug", got.Hour())
	}

	// And it is genuinely NOT 24 elapsed hours. A naive `t.Add(-24 * time.Hour)`
	// would have landed at 10:00 on Oct 31, an hour off, because that Tuesday is
	// 25 hours long. This is exactly the drift the UTC-pinned cron nudges have.
	elapsed := due.Sub(got)
	if elapsed != 25*time.Hour {
		t.Fatalf("-P1D across the fall-back spanned %s; want 25h (the day is 25 hours long)", elapsed)
	}
}

// An RRULE copied out of an .ics file or a Google Calendar payload arrives with
// its line prefix attached. Rejecting that would be a papercut with no upside.
func TestRRulePrefixIsTolerated(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	sched := &Schedule{DTStart: &now, TZID: "America/Los_Angeles", RRule: "RRULE:FREQ=MONTHLY;BYDAY=3TU"}
	if err := sched.Validate(); err != nil {
		t.Fatalf("Validate rejected a prefixed rule: %v", err)
	}
}
