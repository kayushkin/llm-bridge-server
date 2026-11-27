package main

import (
	"testing"
	"time"
)

func TestSpanOfEachKindOfEvent(t *testing.T) {
	zone, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, zone)
	at := func(year int, month time.Month, day, hour, minute int) int64 {
		return time.Date(year, month, day, hour, minute, 0, 0, zone).Unix()
	}

	single, ok, err := spanOf(calendarEvent{ID: "a", StartsAt: at(2024, 3, 10, 12, 0), EndsAt: at(2024, 3, 10, 13, 0)}, zone, now)
	if err != nil || !ok || single.Precision != "minute" || single.EndsAt != at(2024, 3, 10, 13, 0) || single.Repeats != "" {
		t.Errorf("single = %+v %v %v", single, ok, err)
	}

	allDay, ok, err := spanOf(calendarEvent{ID: "b", AllDay: true, StartDate: "2024-07-17", EndDate: "2024-07-18"}, zone, now)
	if err != nil || !ok || allDay.Precision != "day" || allDay.OccurredAt != at(2024, 7, 17, 0, 0) || allDay.EndsAt != 0 {
		t.Errorf("all-day = %+v %v %v", allDay, ok, err)
	}
	trip, _, _ := spanOf(calendarEvent{ID: "c", AllDay: true, StartDate: "2024-07-17", EndDate: "2024-07-20"}, zone, now)
	if trip.EndsAt != at(2024, 7, 19, 0, 0) {
		t.Errorf("three-day event ends %v", time.Unix(trip.EndsAt, 0).In(zone))
	}

	weekly, ok, err := spanOf(calendarEvent{ID: "d", StartsAt: at(2024, 10, 10, 18, 0), EndsAt: at(2024, 10, 10, 19, 30),
		TimeZone: "America/Los_Angeles", RecurrenceRule: "FREQ=WEEKLY;UNTIL=20241031T020000Z"}, zone, now)
	if err != nil || !ok || weekly.EndsAt != at(2024, 10, 24, 18, 0) || weekly.Repeats != "weekly, 3 dates until 24 Oct 2024" {
		t.Errorf("weekly = %+v (ends %v) %v %v", weekly, time.Unix(weekly.EndsAt, 0).In(zone), ok, err)
	}

	yearly, ok, err := spanOf(calendarEvent{ID: "e", AllDay: true, StartDate: "2024-07-17", EndDate: "2024-07-18", RecurrenceRule: "FREQ=YEARLY"}, zone, now)
	if err != nil || !ok || yearly.EndsAt != 0 || yearly.Repeats != "yearly" {
		t.Errorf("yearly = %+v %v %v", yearly, ok, err)
	}

	fortnightly, _, err := spanOf(calendarEvent{ID: "f", StartsAt: at(2024, 1, 1, 9, 0), RecurrenceRule: "FREQ=WEEKLY;INTERVAL=2"}, zone, now)
	if err != nil || fortnightly.Repeats != "every 2 weeks" {
		t.Errorf("fortnightly = %+v %v", fortnightly, err)
	}

	if _, ok, _ := spanOf(calendarEvent{ID: "g", StartsAt: at(2027, 1, 1, 9, 0)}, zone, now); ok {
		t.Error("a future event was placed")
	}
}
