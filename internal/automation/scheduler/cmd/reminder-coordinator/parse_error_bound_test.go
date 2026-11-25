package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ListEvents parses four timestamps out of the calendar payload with
// time.Parse and wraps the parser's error with %w beside the raw value and the
// event summary. All three of those strings come from Google via the dash
// proxy, and nothing in this process bounds any of them.
//
// The error is not swallowed — it reaches log.Printf in the tick, whose output
// scheduler run history keeps. So one malformed dateTime on one event buries the
// whole tick's log.
//
// These are ceiling assertions on the rendered error, not greps for a truncate.
const (
	oversizedFieldLength = 5000
	// One wrapped error's ceiling: wide enough for the bounded summary, the
	// bounded value and a real parser message, far below a 5 KB echo.
	calendarErrorCeiling = 400
)

func oversizedField() string { return strings.Repeat("x", oversizedFieldLength) }

// calendarServing stands up a dash proxy that answers the events read with the
// given raw JSON, and returns a mirror pointed at it.
func calendarServing(t *testing.T, eventsJSON string) *GoogleCalendarMirror {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, eventsJSON)
	}))
	t.Cleanup(server.Close)
	return &GoogleCalendarMirror{DashURL: server.URL}
}

func listEventsError(t *testing.T, eventsJSON string) string {
	t.Helper()
	mirror := calendarServing(t, eventsJSON)
	_, err := mirror.ListEvents("tok", time.Unix(0, 0), time.Unix(0, 0).Add(24*time.Hour))
	if err == nil {
		t.Fatal("an unparseable timestamp produced no error, so the tick would read the day as empty")
	}
	return err.Error()
}

func TestAnUnreadableEventStartIsBoundedInTheError(t *testing.T) {
	rendered := listEventsError(t, `{"events":[{"summary":"standup","start":{"dateTime":"`+oversizedField()+`"}}]}`)
	if len(rendered) > calendarErrorCeiling {
		t.Errorf("a %d-byte start dateTime produced a %d-byte error (ceiling %d)",
			oversizedFieldLength, len(rendered), calendarErrorCeiling)
	}
	if !strings.Contains(rendered, "start") {
		t.Errorf("the bounded error %q no longer says which field failed", rendered)
	}
}

func TestAnUnreadableEventEndIsBoundedInTheError(t *testing.T) {
	// A valid start so the handler reaches the end branch; without it the start
	// site would answer for the end site and this case would test nothing.
	rendered := listEventsError(t, `{"events":[{"summary":"standup","start":{"dateTime":"2026-01-01T09:00:00Z"},"end":{"dateTime":"`+oversizedField()+`"}}]}`)
	if len(rendered) > calendarErrorCeiling {
		t.Errorf("a %d-byte end dateTime produced a %d-byte error (ceiling %d)",
			oversizedFieldLength, len(rendered), calendarErrorCeiling)
	}
	if !strings.Contains(rendered, "end") {
		t.Errorf("the bounded error %q no longer says which field failed", rendered)
	}
}

func TestAnUnreadableAllDayDateIsBoundedInTheError(t *testing.T) {
	rendered := listEventsError(t, `{"events":[{"summary":"holiday","start":{"date":"`+oversizedField()+`"}}]}`)
	if len(rendered) > calendarErrorCeiling {
		t.Errorf("a %d-byte all-day date produced a %d-byte error (ceiling %d)",
			oversizedFieldLength, len(rendered), calendarErrorCeiling)
	}
	if !strings.Contains(rendered, "date") {
		t.Errorf("the bounded error %q no longer says which field failed", rendered)
	}
}

func TestAnUnreadableAllDayEndDateIsBoundedInTheError(t *testing.T) {
	// A valid start.date so the handler reaches the end.date branch.
	rendered := listEventsError(t, `{"events":[{"summary":"holiday","start":{"date":"2026-01-01"},"end":{"date":"`+oversizedField()+`"}}]}`)
	if len(rendered) > calendarErrorCeiling {
		t.Errorf("a %d-byte all-day end date produced a %d-byte error (ceiling %d)",
			oversizedFieldLength, len(rendered), calendarErrorCeiling)
	}
	if !strings.Contains(rendered, "end date") {
		t.Errorf("the bounded error %q no longer says which field failed", rendered)
	}
}

// TestAnOversizedSummaryIsBoundedInTheError covers the half a reader is least
// likely to look at. The summary is quoted into the same error and is just as
// unbounded as the timestamp; bounding only the timestamp leaves the line long.
func TestAnOversizedSummaryIsBoundedInTheError(t *testing.T) {
	rendered := listEventsError(t, `{"events":[{"summary":"`+oversizedField()+`","start":{"dateTime":"not-a-time"}}]}`)
	if len(rendered) > calendarErrorCeiling {
		t.Errorf("a %d-byte summary produced a %d-byte error (ceiling %d)",
			oversizedFieldLength, len(rendered), calendarErrorCeiling)
	}
}

// TestAnOrdinaryBadEventTimeStillSaysWhy is the discriminating negative. A
// repair that drops the parser's error passes every ceiling above and leaves the
// operator with no way to tell a bad month from a bad format.
func TestAnOrdinaryBadEventTimeStillSaysWhy(t *testing.T) {
	rendered := listEventsError(t, `{"events":[{"summary":"standup","start":{"dateTime":"2026-13-01T09:00:00Z"}}]}`)
	if !strings.Contains(rendered, "month out of range") {
		t.Errorf("the error %q dropped the parser's diagnosis", rendered)
	}
	if !strings.Contains(rendered, "standup") {
		t.Errorf("the error %q dropped the event summary, which is how the operator finds the event", rendered)
	}
}
