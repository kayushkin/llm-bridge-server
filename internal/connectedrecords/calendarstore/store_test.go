package calendarstore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakePeople struct {
	byEmail map[string]string
	persons map[string]bool
}

func (f fakePeople) PersonByEmail(email string) (string, error) { return f.byEmail[email], nil }
func (f fakePeople) PersonExists(id string) (bool, error)       { return f.persons[id], nil }

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), fakePeople{
		byEmail: map[string]string{"carol@example.com": "person_000012", "me@example.com": "person_000001"},
		persons: map[string]bool{"person_000012": true, "person_000001": true, "person_000020": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func calendarFile(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-WR-TIMEZONE:America/Los_Angeles\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
}

const lunch = "BEGIN:VEVENT\r\nUID:lunch-1\r\nDTSTART:20240310T200000Z\r\nDTEND:20240310T210000Z\r\nSUMMARY:Lunch with Carol\r\n" +
	"LOCATION:Cafe\\, downtown\r\nORGANIZER;CN=Me:mailto:me@example.com\r\n" +
	"ATTENDEE;CN=Carol;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:Carol@Example.com\r\n" +
	"ATTENDEE;CN=me@example.com:mailto:me@example.com\r\nEND:VEVENT\r\n"

const skating = "BEGIN:VEVENT\r\nUID:skate-1\r\nDTSTART;TZID=America/Los_Angeles:20241010T180000\r\n" +
	"DTEND;TZID=America/Los_Angeles:20241010T193000\r\nRRULE:FREQ=WEEKLY;UNTIL=20241212T020000Z\r\nSUMMARY:Ice Skating\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT30M\r\nEND:VALARM\r\nEND:VEVENT\r\n"

const skatingMoved = "BEGIN:VEVENT\r\nUID:skate-1\r\nRECURRENCE-ID;TZID=America/Los_Angeles:20241024T180000\r\n" +
	"DTSTART;TZID=America/Los_Angeles:20241024T190000\r\nDTEND;TZID=America/Los_Angeles:20241024T200000\r\nSUMMARY:Ice Skating\r\nEND:VEVENT\r\n"

const birthday = "BEGIN:VEVENT\r\nUID:bday-1\r\nDTSTART;VALUE=DATE:20240717\r\nDTEND;VALUE=DATE:20240718\r\nSUMMARY:A birthday\r\nEND:VEVENT\r\n"

func TestParseReadsEveryKindOfTime(t *testing.T) {
	events, err := ParseICS([]byte(calendarFile(lunch, skating, skatingMoved, birthday)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("read %d events", len(events))
	}
	if events[0].StartsAt != time.Date(2024, 3, 10, 20, 0, 0, 0, time.UTC).Unix() || events[0].TimeZone != "UTC" || events[0].Location != "Cafe, downtown" {
		t.Errorf("lunch = %+v", events[0])
	}
	if len(events[0].Attendees) != 2 || events[0].Attendees[0].Email != "carol@example.com" || events[0].Attendees[0].ParticipationStatus != "ACCEPTED" {
		t.Errorf("lunch attendees = %+v", events[0].Attendees)
	}
	pacific, _ := time.LoadLocation("America/Los_Angeles")
	if events[1].StartsAt != time.Date(2024, 10, 10, 18, 0, 0, 0, pacific).Unix() || events[1].RecurrenceRule == "" || events[1].TimeZone != "America/Los_Angeles" {
		t.Errorf("skating = %+v", events[1])
	}
	if !strings.Contains(events[1].Raw, "BEGIN:VALARM") {
		t.Errorf("the raw text lost the alarm: %q", events[1].Raw)
	}
	if events[2].RecurrenceID != "America/Los_Angeles:20241024T180000" {
		t.Errorf("moved date recurrence id = %q", events[2].RecurrenceID)
	}
	if !events[3].AllDay || events[3].StartDate != "2024-07-17" || events[3].EndDate != "2024-07-18" {
		t.Errorf("birthday = %+v", events[3])
	}
}

func TestFloatingTimeWithNoCalendarZoneIsRefused(t *testing.T) {
	file := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20240101T100000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := ParseICS([]byte(file)); err == nil {
		t.Fatal("a time with no zone was guessed at")
	}
}

func TestImportCreatesSkipsAndUpdatesWithARevision(t *testing.T) {
	store := testStore(t)
	calendar, created, err := store.EnsureCalendar("Main", "google", "main")
	if err != nil || !created || calendar.ID != "calendar_000001" {
		t.Fatalf("calendar %+v %v %v", calendar, created, err)
	}
	first, err := store.ImportICS(calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, skatingMoved, birthday)))
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 4 || first.PeopleLinked != 2 {
		t.Fatalf("first import = %+v", first)
	}
	again, err := store.ImportICS(calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, skatingMoved, birthday)))
	if err != nil || again.Unchanged != 4 || again.Created != 0 {
		t.Fatalf("second import = %+v %v", again, err)
	}
	changed := strings.Replace(lunch, "Lunch with Carol", "Long lunch with Carol", 1)
	third, err := store.ImportICS(calendar.ID, "b.ics", []byte(calendarFile(changed)))
	if err != nil || third.Updated != 1 {
		t.Fatalf("third import = %+v %v", third, err)
	}
	event, err := store.GetEvent("calendar_event_000001")
	if err != nil || event.Summary != "Long lunch with Carol" || len(event.People) != 2 {
		t.Fatalf("event = %+v %v", event, err)
	}
	revisions, err := store.Revisions(event.ID)
	if err != nil || len(revisions) != 1 || !strings.Contains(string(revisions[0].Event), `"Lunch with Carol"`) {
		t.Fatalf("revisions = %+v %v", revisions, err)
	}
	// The file without the birthday keeps it.
	if _, err := store.GetEvent("calendar_event_000004"); err != nil {
		t.Errorf("an event the newer file left out was lost: %v", err)
	}
}

func TestPeopleAreLinkedByIdAndChecked(t *testing.T) {
	store := testStore(t)
	calendar, _, _ := store.EnsureCalendar("Main", "google", "main")
	if _, err := store.ImportICS(calendar.ID, "a.ics", []byte(calendarFile(skating))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []PersonLink{
		{PersonID: "person_000020", LinkedBy: LinkedByAgent},
		{PersonID: "person_000020", LinkedBy: LinkedByAttendeeEmail, Reason: "x"},
		{PersonID: "Carol", LinkedBy: LinkedByOperator},
		{PersonID: "person_000099", LinkedBy: LinkedByOperator},
	} {
		if _, err := store.LinkPerson("calendar_event_000001", bad); err == nil {
			t.Errorf("took %+v", bad)
		}
	}
	event, err := store.LinkPerson("calendar_event_000001", PersonLink{PersonID: "person_000020", LinkedBy: LinkedByAgent, Reason: "the title names them"})
	if err != nil || len(event.People) != 1 {
		t.Fatalf("link = %+v %v", event, err)
	}
	if _, err := store.LinkPerson("calendar_event_000001", PersonLink{PersonID: "person_000020", LinkedBy: LinkedByOperator}); err == nil {
		t.Error("the same person was linked twice")
	}
	found, err := store.Events(EventQuery{PersonID: "person_000020"})
	if err != nil || len(found) != 1 {
		t.Fatalf("events by person = %d %v", len(found), err)
	}
	if err := store.UnlinkPerson("calendar_event_000001", "person_000020"); err != nil {
		t.Fatal(err)
	}
}

func TestRoutesAnswer(t *testing.T) {
	store := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	if code := call("POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main"}`).Code; code != http.StatusCreated {
		t.Fatalf("create calendar = %d", code)
	}
	if code := call("POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main","color":"red"}`).Code; code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", code)
	}
	if code := call("POST", "/calendars/calendar_000001/imports?file_name=a.ics", calendarFile(lunch)).Code; code != http.StatusCreated {
		t.Fatalf("import = %d", code)
	}
	if code := call("POST", "/calendars/calendar_000009/imports", calendarFile(lunch)).Code; code != http.StatusNotFound {
		t.Errorf("import to no calendar = %d", code)
	}
	if recorder := call("GET", "/events/calendar_event_000001", ""); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"person_000012"`) {
		t.Errorf("get event = %d %s", recorder.Code, recorder.Body.String())
	}
	if code := call("GET", "/events/calendar_event_000009", "").Code; code != http.StatusNotFound {
		t.Errorf("missing event = %d", code)
	}
	if code := call("GET", "/events?limit=5000", "").Code; code != http.StatusBadRequest {
		t.Errorf("limit past the most = %d", code)
	}
}
