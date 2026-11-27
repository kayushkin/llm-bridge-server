package occurrences

import (
	"testing"
	"time"
)

var pacific, _ = time.LoadLocation("America/Los_Angeles")

func at(year int, month time.Month, day, hour, minute int) int64 {
	return time.Date(year, month, day, hour, minute, 0, 0, pacific).Unix()
}

func TestASeriesLosesItsExcludedAndMovedDates(t *testing.T) {
	series := Event{ID: "calendar_event_000001", UID: "u1", Summary: "Ballet", StartsAt: at(2025, 4, 15, 18, 45), EndsAt: at(2025, 4, 15, 20, 15),
		TimeZone: "America/Los_Angeles", RecurrenceRule: "FREQ=WEEKLY;UNTIL=20250520T065959Z", Status: "CONFIRMED",
		Raw: `{"recurrence":["RRULE:FREQ=WEEKLY;UNTIL=20250520T065959Z","EXDATE;TZID=America/Los_Angeles:20250422T184500"]}`}
	moved := Event{ID: "calendar_event_000002", UID: "u1", RecurrenceID: "America/Los_Angeles:20250429T184500", Summary: "Ballet",
		StartsAt: at(2025, 4, 30, 19, 0), EndsAt: at(2025, 4, 30, 20, 0), TimeZone: "America/Los_Angeles", Status: "CONFIRMED"}
	cancelled := Event{ID: "calendar_event_000003", UID: "u1", RecurrenceID: "America/Los_Angeles:20250506T184500", Summary: "Ballet",
		StartsAt: at(2025, 5, 6, 18, 45), EndsAt: at(2025, 5, 6, 20, 15), TimeZone: "America/Los_Angeles", Status: "CANCELLED"}
	got, err := Expand([]Event{series, moved, cancelled}, time.Unix(at(2025, 4, 1, 0, 0), 0), time.Unix(at(2025, 6, 1, 0, 0), 0), "America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Apr 15, (Apr 22 excluded), Apr 29 moved to the 30th, (May 6 cancelled), May 13.
	want := []struct {
		event    string
		key      int64
		startsAt int64
	}{
		{"calendar_event_000001", at(2025, 4, 15, 18, 45), at(2025, 4, 15, 18, 45)},
		{"calendar_event_000001", at(2025, 5, 13, 18, 45), at(2025, 5, 13, 18, 45)},
		{"calendar_event_000002", at(2025, 4, 29, 18, 45), at(2025, 4, 30, 19, 0)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences: %+v", len(got), got)
	}
	for i, occurrence := range got {
		if occurrence.EventID != want[i].event || occurrence.Key != want[i].key || occurrence.StartsAt != want[i].startsAt || !occurrence.Repeats {
			t.Errorf("occurrence %d = %+v, want %+v", i, occurrence, want[i])
		}
	}
	if got[0].EndsAt-got[0].StartsAt != 90*60 {
		t.Errorf("duration = %d", got[0].EndsAt-got[0].StartsAt)
	}
}

func TestAllDayAndSingleEvents(t *testing.T) {
	birthday := Event{ID: "calendar_event_000010", UID: "b", Summary: "A birthday", AllDay: true, StartDate: "2025-07-17", EndDate: "2025-07-18",
		StartsAt: time.Date(2025, 7, 17, 0, 0, 0, 0, time.UTC).Unix(), EndsAt: time.Date(2025, 7, 18, 0, 0, 0, 0, time.UTC).Unix(),
		TimeZone: "America/Los_Angeles", RecurrenceRule: "FREQ=YEARLY", Status: "CONFIRMED"}
	haircut := Event{ID: "calendar_event_000011", UID: "h", Summary: "Haircut", StartsAt: at(2025, 7, 20, 10, 0), EndsAt: at(2025, 7, 20, 10, 30),
		TimeZone: "America/Los_Angeles", Status: "CONFIRMED"}
	single := birthday
	single.ID, single.RecurrenceRule, single.TimeZone = "calendar_event_000012", "", ""
	only, err := Expand([]Event{single}, time.Unix(at(2025, 1, 1, 0, 0), 0), time.Unix(at(2025, 12, 31, 0, 0), 0), "America/Los_Angeles")
	if err != nil || len(only) != 1 || only[0].StartsAt != at(2025, 7, 17, 0, 0) || only[0].EndsAt != at(2025, 7, 18, 0, 0) {
		t.Fatalf("a single all-day event runs midnight to midnight where it is: %v %+v", err, only)
	}
	got, err := Expand([]Event{birthday, haircut}, time.Unix(at(2026, 1, 1, 0, 0), 0), time.Unix(at(2026, 12, 31, 0, 0), 0), "America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartDate != "2026-07-17" || got[0].EndDate != "2026-07-18" || got[0].StartsAt != at(2026, 7, 17, 0, 0) {
		t.Fatalf("got %+v", got)
	}
}
