package calendar

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The same skating series and moved date as skating and skatingMoved, as
// Google lists them with singleEvents=false.
const googleSkating = `{"kind":"calendar#event","id":"skate1","iCalUID":"skate-1","status":"confirmed","summary":"Ice Skating",
"start":{"dateTime":"2024-10-10T18:00:00-07:00","timeZone":"America/Los_Angeles"},
"end":{"dateTime":"2024-10-10T19:30:00-07:00","timeZone":"America/Los_Angeles"},
"recurrence":["RRULE:FREQ=WEEKLY;UNTIL=20241212T020000Z","EXDATE;TZID=America/Los_Angeles:20241031T180000"],
"created":"2024-09-01T10:00:00.000Z","updated":"2024-09-02T10:00:00.000Z","sequence":1,
"organizer":{"email":"Me@Example.com","displayName":"Me"},
"attendees":[{"email":"Carol@Example.com","displayName":"Carol","responseStatus":"accepted"},
{"email":"dave@example.com","responseStatus":"needsAction","optional":true}]}`

const googleSkatingMoved = `{"kind":"calendar#event","id":"skate1_20241025T010000Z","iCalUID":"skate-1","status":"confirmed",
"summary":"Ice Skating","recurringEventId":"skate1",
"originalStartTime":{"dateTime":"2024-10-24T18:00:00-07:00","timeZone":"America/Los_Angeles"},
"start":{"dateTime":"2024-10-24T19:00:00-07:00","timeZone":"America/Los_Angeles"},
"end":{"dateTime":"2024-10-24T20:00:00-07:00","timeZone":"America/Los_Angeles"}}`

// A date of the series cancelled: Google sends only the start it replaces.
const googleSkatingCancelled = `{"kind":"calendar#event","id":"skate1_20241108T020000Z","status":"cancelled","iCalUID":"skate-1",
"recurringEventId":"skate1","originalStartTime":{"dateTime":"2024-11-07T18:00:00-08:00","timeZone":"America/Los_Angeles"}}`

const googleBirthday = `{"kind":"calendar#event","id":"bday1","iCalUID":"bday-1","status":"confirmed","summary":"A birthday",
"transparency":"transparent","start":{"date":"2024-07-17"},"end":{"date":"2024-07-18"}}`

func googleCalendar(t *testing.T, store *Store) Calendar {
	t.Helper()
	calendar, _, err := store.EnsureCalendar(testContext, aliceID, "Main", "google", "primary")
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func eventsOf(t *testing.T, store *Store, calendarID string) map[string]Event {
	t.Helper()
	events, err := store.Events(testContext, aliceScope, EventQuery{CalendarID: calendarID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Event{}
	for _, event := range events {
		byKey[event.UID+" "+event.RecurrenceID] = event
	}
	return byKey
}

func TestGoogleEventsFindTheEventsAnICSExportBrought(t *testing.T) {
	store := testStore(t)
	calendar := googleCalendar(t, store)
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "export.ics", []byte(calendarFile(skating, skatingMoved, birthday))); err != nil {
		t.Fatal(err)
	}
	before := eventsOf(t, store, calendar.ID)

	record, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+googleSkating+","+googleSkatingMoved+","+googleBirthday+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Created != 0 || record.Updated != 3 || record.Format != ImportFormatGoogleEventJSON {
		t.Fatalf("import = %+v, want the three events found again, none made", record)
	}
	after := eventsOf(t, store, calendar.ID)
	if len(after) != 3 {
		t.Fatalf("%d events after the Google import, want 3", len(after))
	}
	for key, old := range before {
		event := after[key]
		if event.ID != old.ID {
			t.Errorf("%s: id %s became %s", key, old.ID, event.ID)
		}
		if event.StartsAt != old.StartsAt || event.EndsAt != old.EndsAt || event.TimeZone != old.TimeZone ||
			event.AllDay != old.AllDay || event.StartDate != old.StartDate || event.RecurrenceRule != old.RecurrenceRule {
			t.Errorf("%s: times read differently from Google\n ics    %+v\n google %+v", key, old, event)
		}
		revisions, err := store.Revisions(testContext, aliceScope, event.ID)
		if err != nil || len(revisions) != 1 || !strings.Contains(string(revisions[0].Event), "BEGIN:VEVENT") {
			t.Errorf("%s: revisions %v %v, want the .ics row kept", key, revisions, err)
		}
	}
	series := after["skate-1 "]
	if series.SourceEventID != "skate1" || series.OrganizerEmail != "me@example.com" || series.Sequence != 1 ||
		series.Status != "CONFIRMED" || series.Transparency != "OPAQUE" || series.SourceModifiedAt != time.Date(2024, 9, 2, 10, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("series = %+v", series)
	}
	if len(series.Attendees) != 2 || series.Attendees[0] != (Attendee{Email: "carol@example.com", Name: "Carol", Role: "REQ-PARTICIPANT", ParticipationStatus: "ACCEPTED"}) ||
		series.Attendees[1].Role != "OPT-PARTICIPANT" || series.Attendees[1].ParticipationStatus != "NEEDS-ACTION" {
		t.Errorf("attendees = %+v", series.Attendees)
	}
	if len(series.People) != 1 || series.People[0].PersonID != "person_000012" {
		t.Errorf("people = %+v", series.People)
	}
	if after["bday-1 "].Transparency != "TRANSPARENT" || after["skate-1 America/Los_Angeles:20241024T180000"].SourceEventID != "skate1_20241025T010000Z" {
		t.Errorf("birthday or moved date read wrong: %+v", after)
	}

	again, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+googleSkating+","+googleSkatingMoved+","+googleBirthday+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Unchanged != 3 || again.Updated != 0 || again.Created != 0 {
		t.Errorf("the same events again = %+v, want all unchanged", again)
	}

	// An .ics import afterwards keeps the Google id the row was given.
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "export.ics", []byte(calendarFile(birthday))); err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(t, store, calendar.ID)["bday-1 "].SourceEventID; got != "bday1" {
		t.Errorf("after an .ics import the birthday's Google id is %q", got)
	}
}

func TestGoogleCancellationsAreKept(t *testing.T) {
	store := testStore(t)
	calendar := googleCalendar(t, store)
	if _, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+googleSkating+","+googleBirthday+"]")); err != nil {
		t.Fatal(err)
	}
	deleted := `{"kind":"calendar#event","id":"bday1","status":"cancelled"}`
	neverHeld := `{"kind":"calendar#event","id":"gone","status":"cancelled"}`
	record, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+googleSkatingCancelled+","+deleted+","+neverHeld+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Created != 1 || record.Updated != 1 || record.Skipped != 1 {
		t.Fatalf("import = %+v, want the cancelled date made, the birthday cancelled, one skipped", record)
	}
	events := eventsOf(t, store, calendar.ID)
	birthday := events["bday-1 "]
	if birthday.Status != "CANCELLED" || birthday.Summary != "A birthday" || birthday.StartDate != "2024-07-17" {
		t.Errorf("deleted birthday = %+v, want cancelled with the rest kept", birthday)
	}
	cancelledDate := events["skate-1 America/Los_Angeles:20241107T180000"]
	if cancelledDate.Status != "CANCELLED" || cancelledDate.StartsAt != time.Date(2024, 11, 8, 2, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("cancelled date = %+v", cancelledDate)
	}
}

func TestGoogleImportsRefuseWhatTheyCannotPlace(t *testing.T) {
	store := testStore(t)
	calendar := googleCalendar(t, store)
	other, _, _ := store.EnsureCalendar(testContext, aliceID, "Work", "outlook", "work")
	if _, err := store.ImportGoogleEvents(testContext, aliceScope, other.ID, "dash", []byte("["+googleBirthday+"]")); !errors.Is(err, ErrInvalid) {
		t.Errorf("into a calendar Google does not keep: %v, want invalid", err)
	}
	for name, body := range map[string]string{
		"not an array":      googleBirthday,
		"no iCalUID":        `[{"id":"x","status":"confirmed","start":{"date":"2024-01-01"}}]`,
		"no start":          `[{"id":"x","iCalUID":"x","status":"confirmed"}]`,
		"unknown response":  `[{"id":"x","iCalUID":"x","start":{"date":"2024-01-01"},"attendees":[{"email":"a@b","responseStatus":"maybe"}]}]`,
		"the same id twice": "[" + googleBirthday + "," + googleBirthday + "]",
	} {
		if _, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte(body)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	if _, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+googleBirthday+"]")); err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(googleBirthday, `"iCalUID":"bday-1"`, `"iCalUID":"bday-2"`, 1)
	if _, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+renamed+"]")); !errors.Is(err, ErrConflict) {
		t.Errorf("a Google id that now names another event: %v, want conflict", err)
	}
	otherID := strings.Replace(googleBirthday, `"id":"bday1"`, `"id":"bday9"`, 1)
	if _, err := store.ImportGoogleEvents(testContext, aliceScope, calendar.ID, "dash", []byte("["+otherID+"]")); !errors.Is(err, ErrConflict) {
		t.Errorf("an event held under another Google id: %v, want conflict", err)
	}
}
