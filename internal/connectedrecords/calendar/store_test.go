package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

var testContext = context.Background()

// Two made-up owners, each with an address book of their own.
const (
	aliceID = "principal_000101"
	bobID   = "principal_000102"
)

var (
	aliceScope = ownership.OwnedBy(aliceID)
	bobScope   = ownership.OwnedBy(bobID)
)

// fakePeople is people-store: each owner's people, by address and by id.
type fakePeople struct {
	byEmail map[string]map[string]string
	persons map[string]map[string]bool
}

func (f fakePeople) PersonByEmail(_ context.Context, owner, email string) (string, error) {
	return f.byEmail[owner][email], nil
}

func (f fakePeople) PersonExists(_ context.Context, owner, id string) (bool, error) {
	return f.persons[owner][id], nil
}

func testPeople() fakePeople {
	return fakePeople{
		byEmail: map[string]map[string]string{
			aliceID: {"carol@example.com": "person_000012", "me@example.com": "person_000001"},
			bobID:   {"carol@example.com": "person_000050"},
		},
		persons: map[string]map[string]bool{
			aliceID: {"person_000012": true, "person_000001": true, "person_000020": true},
			bobID:   {"person_000050": true, "person_000051": true},
		},
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module), testPeople())
}

func calendarFile(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-WR-TIMEZONE:Europe/Lisbon\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
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
	calendar, created, err := store.EnsureCalendar(testContext, aliceID, "Main", "google", "main")
	if err != nil || !created || calendar.ID != "calendar_000001" || calendar.OwnerPrincipalID != aliceID {
		t.Fatalf("calendar %+v %v %v", calendar, created, err)
	}
	first, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, skatingMoved, birthday)))
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 4 || first.PeopleLinked != 2 || first.ID != "calendar_import_000001" || first.OwnerPrincipalID != aliceID {
		t.Fatalf("first import = %+v", first)
	}
	again, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, skatingMoved, birthday)))
	if err != nil || again.Unchanged != 4 || again.Created != 0 {
		t.Fatalf("second import = %+v %v", again, err)
	}
	changed := strings.Replace(lunch, "Lunch with Carol", "Long lunch with Carol", 1)
	third, err := store.ImportICS(testContext, aliceScope, calendar.ID, "b.ics", []byte(calendarFile(changed)))
	if err != nil || third.Updated != 1 {
		t.Fatalf("third import = %+v %v", third, err)
	}
	event, err := store.GetEvent(testContext, aliceScope, "calendar_event_000001")
	if err != nil || event.Summary != "Long lunch with Carol" || len(event.People) != 2 || event.ImportID != third.ID || event.OwnerPrincipalID != aliceID {
		t.Fatalf("event = %+v %v", event, err)
	}
	revisions, err := store.Revisions(testContext, aliceScope, event.ID)
	if err != nil || len(revisions) != 1 || !strings.Contains(string(revisions[0].Event), `"Lunch with Carol"`) || revisions[0].OwnerPrincipalID != aliceID {
		t.Fatalf("revisions = %+v %v", revisions, err)
	}
	// The file without the birthday keeps it.
	if _, err := store.GetEvent(testContext, aliceScope, "calendar_event_000004"); err != nil {
		t.Errorf("an event the newer file left out was lost: %v", err)
	}
	imports, err := store.Imports(testContext, aliceScope)
	if err != nil || len(imports) != 3 || imports[0].ID != third.ID || imports[0].Updated != 1 || imports[2].Created != 4 {
		t.Fatalf("imports = %+v %v", imports, err)
	}
}

func TestAFileThatDoesNotParseWritesNothing(t *testing.T) {
	store := testStore(t)
	calendar, _, _ := store.EnsureCalendar(testContext, aliceID, "Main", "google", "main")
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte("not a calendar")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a broken file: %v, want invalid", err)
	}
	// An import that fails part way through rolls back its import row too.
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(birthday, birthday))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("one event twice: %v, want invalid", err)
	}
	counts, err := store.Counts(testContext, aliceScope)
	if err != nil || counts != (Counts{Calendars: 1}) {
		t.Fatalf("counts = %+v %v", counts, err)
	}
}

func TestPeopleAreLinkedByIdAndChecked(t *testing.T) {
	store := testStore(t)
	calendar, _, _ := store.EnsureCalendar(testContext, aliceID, "Main", "google", "main")
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(skating))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []PersonLink{
		{PersonID: "person_000020", LinkedBy: LinkedByAgent},
		{PersonID: "person_000020", LinkedBy: LinkedByAttendeeEmail, Reason: "x"},
		{PersonID: "Carol", LinkedBy: LinkedByOperator},
		{PersonID: "person_000099", LinkedBy: LinkedByOperator},
	} {
		if _, err := store.LinkPerson(testContext, aliceScope, "calendar_event_000001", bad); err == nil {
			t.Errorf("took %+v", bad)
		}
	}
	event, err := store.LinkPerson(testContext, aliceScope, "calendar_event_000001", PersonLink{PersonID: "person_000020", LinkedBy: LinkedByAgent, Reason: "the title names them"})
	if err != nil || len(event.People) != 1 {
		t.Fatalf("link = %+v %v", event, err)
	}
	if _, err := store.LinkPerson(testContext, aliceScope, "calendar_event_000001", PersonLink{PersonID: "person_000020", LinkedBy: LinkedByOperator}); !errors.Is(err, ErrConflict) {
		t.Errorf("the same person linked twice: %v, want conflict", err)
	}
	found, err := store.Events(testContext, aliceScope, EventQuery{PersonID: "person_000020"})
	if err != nil || len(found) != 1 {
		t.Fatalf("events by person = %d %v", len(found), err)
	}
	if err := store.UnlinkPerson(testContext, aliceScope, "calendar_event_000001", "person_000020"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnlinkPerson(testContext, aliceScope, "calendar_event_000001", "person_000020"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unlinking twice: %v, want not found", err)
	}
}

// q is a substring of summary, location or description, in any case, with
// the pattern marks % and _ matched as themselves.
func TestTextSearchIsASubstring(t *testing.T) {
	store := testStore(t)
	calendar, _, _ := store.EnsureCalendar(testContext, aliceID, "Main", "google", "main")
	described := "BEGIN:VEVENT\r\nUID:plan-1\r\nDTSTART:20240501T100000Z\r\nSUMMARY:Planning\r\nDESCRIPTION:Bring 100% of the notes_v2\r\nEND:VEVENT\r\n"
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, described))); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string][]string{
		"LUNCH":        {"lunch-1"},
		"downtown":     {"lunch-1"},
		"skat":         {"skate-1"},
		"100%":         {"plan-1"},
		"%":            {"plan-1"},
		"notes_v2":     {"plan-1"},
		"notes_":       {"plan-1"},
		"_":            {"plan-1"},
		"no such text": nil,
	} {
		events, err := store.Events(testContext, aliceScope, EventQuery{Text: query})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, event := range events {
			got = append(got, event.UID)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("q=%q found %v, want %v", query, got, want)
		}
	}
}

func TestEventsAreFilteredAndPaged(t *testing.T) {
	store := testStore(t)
	calendar, _, _ := store.EnsureCalendar(testContext, aliceID, "Main", "google", "main")
	other, _, _ := store.EnsureCalendar(testContext, aliceID, "Other", "outlook", "other")
	if _, err := store.ImportICS(testContext, aliceScope, calendar.ID, "a.ics", []byte(calendarFile(lunch, skating, birthday))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportICS(testContext, aliceScope, other.ID, "b.ics", []byte(calendarFile(lunch))); err != nil {
		t.Fatal(err)
	}
	uids := func(query EventQuery) string {
		t.Helper()
		events, err := store.Events(testContext, aliceScope, query)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, event := range events {
			if event.Raw != "" {
				t.Errorf("a listed event carries its raw text")
			}
			got = append(got, event.CalendarID+":"+event.UID)
		}
		return strings.Join(got, ",")
	}
	if got := uids(EventQuery{CalendarID: calendar.ID}); got != "calendar_000001:lunch-1,calendar_000001:bday-1,calendar_000001:skate-1" {
		t.Errorf("one calendar, oldest first = %s", got)
	}
	if got := uids(EventQuery{CalendarID: "calendar_x"}); got != "" {
		t.Errorf("a malformed calendar id = %s", got)
	}
	july := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC).Unix()
	if got := uids(EventQuery{From: july, To: july + 60*86400}); got != "calendar_000001:bday-1" {
		t.Errorf("from july for sixty days = %s", got)
	}
	if got := uids(EventQuery{Limit: 1, Offset: 1}); got != "calendar_000002:lunch-1" {
		t.Errorf("the second event = %s", got)
	}
	if _, err := store.Events(testContext, aliceScope, EventQuery{Limit: MaxEventLimit + 1}); !errors.Is(err, ErrInvalid) {
		t.Errorf("past the most: %v", err)
	}
}

// request is a call to the routes as caller, or with no caller when nil.
func request(t *testing.T, mux *http.ServeMux, caller *ownership.Caller, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if caller != nil {
		r = r.WithContext(ownership.ContextWithCaller(r.Context(), *caller))
	}
	mux.ServeHTTP(recorder, r)
	return recorder
}

var (
	alice         = &ownership.Caller{PrincipalID: aliceID}
	bob           = &ownership.Caller{PrincipalID: bobID}
	administrator = &ownership.Caller{PrincipalID: "principal_000001", SeesEveryOwner: true}
	thisServer    = &ownership.Caller{SeesEveryOwner: true}
)

func routes(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHandlers(mux, testStore(t))
	return mux
}

func TestRoutesAnswer(t *testing.T) {
	mux := routes(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		return request(t, mux, alice, method, path, body)
	}
	if code := call("POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main"}`).Code; code != http.StatusCreated {
		t.Fatalf("create calendar = %d", code)
	}
	if code := call("POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main"}`).Code; code != http.StatusOK {
		t.Errorf("the same calendar again = %d", code)
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
	if code := call("POST", "/calendars/calendar_event_000001/imports", calendarFile(lunch)).Code; code != http.StatusNotFound {
		t.Errorf("import to a malformed calendar id = %d", code)
	}
	if recorder := call("GET", "/events/calendar_event_000001", ""); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"person_000012"`) ||
		!strings.Contains(recorder.Body.String(), `"owner_principal_id":"`+aliceID+`"`) {
		t.Errorf("get event = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, path := range []string{"/events/calendar_event_000009", "/events/nonsense", "/events/calendar_event_1"} {
		if code := call("GET", path, "").Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, code)
		}
	}
	if code := call("GET", "/events?limit=5000", "").Code; code != http.StatusBadRequest {
		t.Errorf("limit past the most = %d", code)
	}
	if code := call("GET", "/vocabularies", "").Code; code != http.StatusOK {
		t.Errorf("vocabularies = %d", code)
	}
	if recorder := call("GET", "/health", ""); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"events":1`) {
		t.Errorf("health = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestARequestWithNoCallerIsRefused(t *testing.T) {
	mux := routes(t)
	for _, route := range [][2]string{{"GET", "/health"}, {"GET", "/vocabularies"}, {"GET", "/events"}, {"GET", "/calendars"},
		{"POST", "/calendars"}, {"GET", "/imports"}, {"GET", "/events/calendar_event_000001"}} {
		if code := request(t, mux, nil, route[0], route[1], `{}`).Code; code != http.StatusUnauthorized {
			t.Errorf("%s %s with no caller = %d", route[0], route[1], code)
		}
	}
	// This server's own service names nobody, so it cannot own a calendar.
	if code := request(t, mux, thisServer, "POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main"}`).Code; code != http.StatusForbidden {
		t.Errorf("a calendar made for nobody = %d", code)
	}
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("answer %d does not decode: %v", recorder.Code, err)
	}
	return value
}

// Alice and Bob each keep a calendar with the same source and source_ref,
// and the same attendee; neither reaches the other's records.
func TestOwnersReachOnlyTheirOwnRecords(t *testing.T) {
	mux := routes(t)
	body := `{"name":"Main","source":"google","source_ref":"main"}`
	if recorder := request(t, mux, alice, "POST", "/calendars", body); recorder.Code != http.StatusCreated || decode[Calendar](t, recorder).ID != "calendar_000001" {
		t.Fatalf("alice's calendar = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder := request(t, mux, bob, "POST", "/calendars", body)
	bobCalendar := decode[Calendar](t, recorder)
	if recorder.Code != http.StatusCreated || bobCalendar.ID != "calendar_000002" || bobCalendar.OwnerPrincipalID != bobID {
		t.Fatalf("bob's calendar with alice's source_ref = %d %+v; source_ref is unique per owner", recorder.Code, bobCalendar)
	}
	if code := request(t, mux, alice, "POST", "/calendars/calendar_000001/imports", calendarFile(lunch)).Code; code != http.StatusCreated {
		t.Fatalf("alice's import = %d", code)
	}
	if code := request(t, mux, bob, "POST", "/calendars/calendar_000001/imports", calendarFile(lunch)).Code; code != http.StatusNotFound {
		t.Errorf("bob importing into alice's calendar = %d", code)
	}
	if code := request(t, mux, bob, "POST", "/calendars/calendar_000001/google-event-imports", "[]").Code; code != http.StatusNotFound {
		t.Errorf("bob's Google import into alice's calendar = %d", code)
	}
	if code := request(t, mux, bob, "POST", "/calendars/calendar_000002/imports", calendarFile(lunch)).Code; code != http.StatusCreated {
		t.Fatalf("bob's import = %d", code)
	}

	// Each attendee address is looked up in the event owner's own people.
	aliceEvent := decode[Event](t, request(t, mux, alice, "GET", "/events/calendar_event_000001", ""))
	bobEvent := decode[Event](t, request(t, mux, bob, "GET", "/events/calendar_event_000002", ""))
	if len(aliceEvent.People) != 2 || aliceEvent.People[0].PersonID != "person_000001" || aliceEvent.People[1].PersonID != "person_000012" {
		t.Errorf("alice's event people = %+v", aliceEvent.People)
	}
	if len(bobEvent.People) != 1 || bobEvent.People[0].PersonID != "person_000050" || bobEvent.OwnerPrincipalID != bobID {
		t.Errorf("bob's event people = %+v", bobEvent.People)
	}

	// Bob cannot read or change alice's event: it answers as a missing one.
	for _, route := range [][3]string{
		{"GET", "/events/calendar_event_000001", ""},
		{"GET", "/events/calendar_event_000001/revisions", ""},
		{"POST", "/events/calendar_event_000001/people", `{"person_id":"person_000050","linked_by":"operator"}`},
		{"DELETE", "/events/calendar_event_000001/people/person_000012", ""},
	} {
		if recorder := request(t, mux, bob, route[0], route[1], route[2]); recorder.Code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d", route[0], route[1], recorder.Code)
		}
	}
	if got := decode[Event](t, request(t, mux, alice, "GET", "/events/calendar_event_000001", "")); len(got.People) != 2 {
		t.Errorf("bob's refused calls changed alice's event: %+v", got.People)
	}

	// Lists hold only the caller's own.
	for _, path := range []string{"/events", "/calendars", "/imports", "/events?person_id=person_000012", "/events?q=lunch"} {
		want := 1
		if path == "/events?person_id=person_000012" {
			want = 0
		}
		if got := decode[[]json.RawMessage](t, request(t, mux, bob, "GET", path, "")); len(got) != want {
			t.Errorf("bob's GET %s holds %d, want %d", path, len(got), want)
		}
	}
	if recorder := request(t, mux, bob, "GET", "/health", ""); !strings.Contains(recorder.Body.String(), `{"calendars":1,"events":1,"people_links":1,"imports":1}`) {
		t.Errorf("bob's health = %s", recorder.Body.String())
	}
	if code := request(t, mux, bob, "GET", "/events?owner_principal_id="+aliceID, "").Code; code != http.StatusForbidden {
		t.Errorf("bob naming alice as owner = %d", code)
	}

	// An administrator sees both, and narrows to one owner when asked.
	if got := decode[[]Event](t, request(t, mux, administrator, "GET", "/events", "")); len(got) != 2 {
		t.Errorf("the administrator's events = %d, want both owners'", len(got))
	}
	narrowed := decode[[]Event](t, request(t, mux, administrator, "GET", "/events?owner_principal_id="+bobID, ""))
	if len(narrowed) != 1 || narrowed[0].OwnerPrincipalID != bobID {
		t.Errorf("the administrator narrowed to bob = %+v", narrowed)
	}
	if got := decode[[]Calendar](t, request(t, mux, administrator, "GET", "/calendars?owner_principal_id="+aliceID, "")); len(got) != 1 || got[0].ID != "calendar_000001" {
		t.Errorf("the administrator's calendars narrowed to alice = %+v", got)
	}
	if code := request(t, mux, administrator, "GET", "/events/calendar_event_000001", "").Code; code != http.StatusOK {
		t.Errorf("the administrator reading alice's event = %d", code)
	}
	if recorder := request(t, mux, administrator, "GET", "/health", ""); !strings.Contains(recorder.Body.String(), `{"calendars":2,"events":2,"people_links":3,"imports":2}`) {
		t.Errorf("the administrator's health = %s", recorder.Body.String())
	}
	// An administrator's POST /calendars makes the administrator's own.
	if recorder := request(t, mux, administrator, "POST", "/calendars", body); recorder.Code != http.StatusCreated || decode[Calendar](t, recorder).OwnerPrincipalID != administrator.PrincipalID {
		t.Errorf("the administrator's calendar = %d %s", recorder.Code, recorder.Body.String())
	}
}

// A person only another owner keeps is refused as people-store would refuse
// an unknown one, whoever asks.
func TestAPersonOfAnotherOwnerCannotBeLinked(t *testing.T) {
	mux := routes(t)
	request(t, mux, alice, "POST", "/calendars", `{"name":"Main","source":"google","source_ref":"main"}`)
	if code := request(t, mux, alice, "POST", "/calendars/calendar_000001/imports", calendarFile(skating)).Code; code != http.StatusCreated {
		t.Fatalf("import = %d", code)
	}
	link := `{"person_id":"person_000051","linked_by":"operator"}`
	if code := request(t, mux, alice, "POST", "/events/calendar_event_000001/people", link).Code; code != http.StatusBadRequest {
		t.Errorf("alice linking bob's person = %d", code)
	}
	if code := request(t, mux, administrator, "POST", "/events/calendar_event_000001/people", link).Code; code != http.StatusBadRequest {
		t.Errorf("an administrator linking bob's person to alice's event = %d", code)
	}
	if recorder := request(t, mux, administrator, "POST", "/events/calendar_event_000001/people", `{"person_id":"person_000020","linked_by":"operator"}`); recorder.Code != http.StatusCreated {
		t.Errorf("an administrator linking alice's person to alice's event = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAZeroScopeReachesNothing(t *testing.T) {
	store := testStore(t)
	if _, err := store.Events(testContext, ownership.Scope{}, EventQuery{}); err == nil {
		t.Error("a scope that was never set listed events")
	}
	if _, _, err := store.EnsureCalendar(testContext, "", "Main", "google", "main"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a calendar with no owner: %v", err)
	}
	if _, err := store.Calendars(testContext, bobScope); err != nil {
		t.Fatal(err)
	}
}
