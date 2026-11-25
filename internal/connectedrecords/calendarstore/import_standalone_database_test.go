package calendarstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows and gaps in every numbering.
func newStandaloneDatabase(t *testing.T) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "calendar-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO calendars (seq, id, name, source, source_ref, created_at) VALUES (2, 'calendar_000002', 'Main', 'google', 'main', 1700000000)`,
		`INSERT INTO calendar_imports (seq, id, calendar_id, file_name, format, file_sha256, created, updated, unchanged, people_linked, skipped, imported_at)
			VALUES (3, 'calendar_import_000003', 'calendar_000002', 'a.ics', 'ics', 'aa', 2, 0, 0, 1, 0, 1700000100)`,
		`INSERT INTO calendar_imports (seq, id, calendar_id, file_name, format, file_sha256, created, updated, unchanged, people_linked, skipped, imported_at)
			VALUES (5, 'calendar_import_000005', 'calendar_000002', 'dash', 'google-event-json', 'bb', 0, 1, 1, 0, 0, 1700000200)`,
		`INSERT INTO calendar_events (seq, id, calendar_id, uid, recurrence_id, source_event_id, summary, description, location,
			starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email,
			organizer_name, source_created_at, source_modified_at, sequence, raw, raw_hash, import_id, created_at, updated_at)
			VALUES (4, 'calendar_event_000004', 'calendar_000002', 'lunch-1', '', 'lunch1', 'Lunch with Carol', '', 'Cafe', 1710100800, 1710104400, 0,
			'', '', 'UTC', '', 'CONFIRMED', '', 'me@example.com', 'Me', 0, 0, 0, '{"id":"lunch1"}', 'h1', 'calendar_import_000005', 1700000100, 1700000200)`,
		`INSERT INTO calendar_events (seq, id, calendar_id, uid, recurrence_id, source_event_id, summary, description, location,
			starts_at, ends_at, all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email,
			organizer_name, source_created_at, source_modified_at, sequence, raw, raw_hash, import_id, created_at, updated_at)
			VALUES (7, 'calendar_event_000007', 'calendar_000002', 'bday-1', '', '', 'A birthday', '', '', 1721174400, 1721260800, 1,
			'2024-07-17', '2024-07-18', '', '', '', '', '', '', 0, 0, 0, 'BEGIN:VEVENT', 'h2', 'calendar_import_000003', 1700000100, 1700000100)`,
		`INSERT INTO calendar_event_attendees VALUES ('calendar_event_000004', 0, 'carol@example.com', 'Carol', 'REQ-PARTICIPANT', 'ACCEPTED')`,
		`INSERT INTO calendar_event_attendees VALUES ('calendar_event_000004', 1, 'me@example.com', '', '', '')`,
		`INSERT INTO calendar_event_people VALUES ('calendar_event_000004', 'person_000012', 'attendee-email', 'people-store holds the attendee''s address', 1700000100)`,
		`INSERT INTO calendar_event_revisions VALUES (6, 'calendar_event_000004', 'import', 1700000200, '{"id":"calendar_event_000004","summary":"Lunch with Carol"}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	store := testStore(t)
	counts, err := ImportStandaloneDatabase(testContext, store.db, newStandaloneDatabase(t), aliceID)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Calendars: 1, Imports: 2, Events: 2, Attendees: 2, PeopleLinks: 1, Revisions: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if got := counts.Summary(); got != "calendars=1 imports=2 events=2 attendees=2 people_links=1 revisions=1" {
		t.Errorf("summary = %q", got)
	}

	lunchEvent, err := store.GetEvent(testContext, aliceScope, "calendar_event_000004")
	if err != nil {
		t.Fatal(err)
	}
	if lunchEvent.OwnerPrincipalID != aliceID || lunchEvent.CalendarID != "calendar_000002" || lunchEvent.ImportID != "calendar_import_000005" ||
		lunchEvent.SourceEventID != "lunch1" || lunchEvent.Raw != `{"id":"lunch1"}` || lunchEvent.UpdatedAt != 1700000200 ||
		len(lunchEvent.Attendees) != 2 || lunchEvent.Attendees[0].Name != "Carol" || len(lunchEvent.People) != 1 || lunchEvent.People[0].PersonID != "person_000012" {
		t.Fatalf("calendar_event_000004 = %+v", lunchEvent)
	}
	birthdayEvent, err := store.GetEvent(testContext, aliceScope, "calendar_event_000007")
	if err != nil || !birthdayEvent.AllDay || birthdayEvent.StartDate != "2024-07-17" {
		t.Fatalf("calendar_event_000007 = %+v %v", birthdayEvent, err)
	}
	revisions, err := store.Revisions(testContext, aliceScope, "calendar_event_000004")
	if err != nil || len(revisions) != 1 || revisions[0].RevisionID != 6 || string(revisions[0].Event) != `{"id":"calendar_event_000004","summary":"Lunch with Carol"}` {
		t.Fatalf("revisions = %+v %v", revisions, err)
	}
	if _, err := store.GetEvent(testContext, bobScope, "calendar_event_000004"); err == nil {
		t.Error("another owner reads an imported event")
	}
	found, err := store.Events(testContext, aliceScope, EventQuery{Text: "carol"})
	if err != nil || len(found) != 1 || found[0].ID != "calendar_event_000004" {
		t.Fatalf("search after the import = %+v %v", found, err)
	}
	imports, err := store.Imports(testContext, aliceScope)
	if err != nil || len(imports) != 2 || imports[0].ID != "calendar_import_000005" || imports[0].Unchanged != 1 {
		t.Fatalf("imports = %+v %v", imports, err)
	}

	// The next records take the numbers after the highest copied.
	calendar, created, err := store.EnsureCalendar(testContext, aliceID, "Work", "google", "work")
	if err != nil || !created || calendar.ID != "calendar_000003" {
		t.Fatalf("next calendar = %+v %v %v", calendar, created, err)
	}
	record, err := store.ImportICS(testContext, aliceScope, "calendar_000002", "b.ics", []byte(calendarFile(strings.Replace(lunch, "UID:lunch-1", "UID:lunch-2", 1))))
	if err != nil || record.ID != "calendar_import_000006" {
		t.Fatalf("next import = %+v %v", record, err)
	}
	events, err := store.Events(testContext, aliceScope, EventQuery{Text: "Lunch"})
	if err != nil || len(events) != 2 || events[1].ID != "calendar_event_000008" {
		t.Fatalf("next event = %+v %v", events, err)
	}
	changed := strings.Replace(lunch, "UID:lunch-1", "UID:lunch-2", 1)
	changed = strings.Replace(changed, "Lunch with Carol", "Long lunch", 1)
	if _, err := store.ImportICS(testContext, aliceScope, "calendar_000002", "c.ics", []byte(calendarFile(changed))); err != nil {
		t.Fatal(err)
	}
	revisions, err = store.Revisions(testContext, aliceScope, "calendar_event_000008")
	if err != nil || len(revisions) != 1 || revisions[0].RevisionID != 7 {
		t.Fatalf("next revision = %+v %v", revisions, err)
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	store := testStore(t)
	path := newStandaloneDatabase(t)
	for _, owner := range []string{"", "alice", "principal_1"} {
		if _, err := ImportStandaloneDatabase(testContext, store.db, path, owner); err == nil {
			t.Errorf("owner %q was taken", owner)
		}
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, filepath.Join(t.TempDir(), "missing.db"), aliceID); err == nil {
		t.Error("a missing file was imported as an empty one")
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, path, aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, path, aliceID); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("a second import: %v, want a refusal", err)
	}

	other := testStore(t)
	extraColumn := newStandaloneDatabase(t)
	db, err := sql.Open("sqlite", extraColumn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE calendar_events ADD COLUMN color TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, other.db, extraColumn, aliceID); err == nil || !strings.Contains(err.Error(), "color") {
		t.Errorf("a column the import does not know: %v", err)
	}

	wrongID := newStandaloneDatabase(t)
	db, err = sql.Open("sqlite", wrongID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE calendars SET id = 'calendar_000009' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, other.db, wrongID, aliceID); err == nil {
		t.Error("an id its number does not make was copied")
	}
	if counts, err := other.Counts(testContext, aliceScope); err != nil || counts != (Counts{}) {
		t.Errorf("a refused import left rows: %+v %v", counts, err)
	}
}
