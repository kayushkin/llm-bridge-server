package timeledgerstore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clock := time.Unix(1_800_000_000, 0)
	store.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return store
}

var (
	importer = Actor{By: "time-ledger-import", Kind: ActorProgram}
	reviewer = Actor{By: "time-ledger-review", Kind: ActorModel}
	operator = Actor{By: "operator on dash (principal_000001)", Kind: ActorOperator}
)

func pointer(value int64) *int64 { return &value }

func entryInput(title string, startsAt, endsAt int64, actor Actor, links ...LinkInput) EntryInput {
	return EntryInput{Title: title, StartsAt: startsAt, EndsAt: endsAt, TimeZone: "America/Los_Angeles", Links: links,
		By: actor.By, ByKind: actor.Kind, Reason: "test"}
}

func calendarSource(ref string, occurrence int64) LinkInput {
	return LinkInput{LinkType: "calendar_events", Ref: ref, OccurrenceStartsAt: pointer(occurrence), BearsOn: BearsOnSource}
}

func mustCreate(t *testing.T, store *Store, input EntryInput) TimeEntry {
	t.Helper()
	entry, err := store.CreateEntry(input)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestOneSourceDateBelongsToOneEntry(t *testing.T) {
	store := testStore(t)
	first := mustCreate(t, store, entryInput("Ballet", 1000, 2000, importer, calendarSource("calendar_event_000193", 1000)))
	if first.ID != "time_entry_000001" || first.Outcome != OutcomeUnknown {
		t.Fatalf("first = %+v", first)
	}
	_, err := store.CreateEntry(entryInput("Ballet again", 1000, 2000, importer, calendarSource("calendar_event_000193", 1000)))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("a second entry for the same date: %v", err)
	}
	if _, err := store.Entry("time_entry_000002", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused entry was written: %v", err)
	}
	// The next week of the series is a different date and a different entry.
	mustCreate(t, store, entryInput("Ballet", 605800, 606800, importer, calendarSource("calendar_event_000193", 605800)))
	links, err := store.LinksTo("calendar_events", "calendar_event_000193", nil, false)
	if err != nil || len(links) != 2 {
		t.Fatalf("links to the series: %v %+v", err, links)
	}
}

func TestChildrenLieInsideTheirParent(t *testing.T) {
	store := testStore(t)
	journey := mustCreate(t, store, entryInput("A week away", 0+1, 700000, importer))
	day := entryInput("A day at a building", 100000, 140000, importer)
	day.ParentTimeEntryID = journey.ID
	dayEntry := mustCreate(t, store, day)
	hour := entryInput("An hour of work", 120000, 123600, operator)
	hour.ParentTimeEntryID = dayEntry.ID
	hourEntry := mustCreate(t, store, hour)

	outside := entryInput("Too long", 100000, 800000, importer)
	outside.ParentTimeEntryID = journey.ID
	if _, err := store.CreateEntry(outside); !errors.Is(err, ErrConflict) {
		t.Fatalf("a child longer than its parent: %v", err)
	}
	if _, err := store.UpdateEntry(dayEntry.ID, EntryChange{EndsAt: pointer(121000), By: importer.By, ByKind: importer.Kind, Reason: "shrink"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("shrinking a parent past its child: %v", err)
	}
	if _, err := store.UpdateEntry(journey.ID, EntryChange{ParentTimeEntryID: &hourEntry.ID, By: operator.By, ByKind: operator.Kind, Reason: "loop"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an entry inside its own grandchild: %v", err)
	}
	full, err := store.Entry(journey.ID, true)
	if err != nil || len(full.ChildIDs) != 1 || full.ChildIDs[0] != dayEntry.ID {
		t.Fatalf("children = %v %v", full.ChildIDs, err)
	}
	if err := store.DeleteEntry(dayEntry.ID, operator); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting an entry with a live child: %v", err)
	}
}

func TestSiblingsMayOverlap(t *testing.T) {
	store := testStore(t)
	festival := mustCreate(t, store, entryInput("Festival", 1000, 9000, importer))
	for _, title := range []string{"Workshop", "Dinner"} {
		input := entryInput(title, 2000, 5000, importer)
		input.ParentTimeEntryID = festival.ID
		mustCreate(t, store, input)
	}
	entries, err := store.Entries(EntryFilter{From: 3000, To: 4000})
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries over 3000–4000: %v %d", err, len(entries))
	}
}

func TestAMachineNeverUndoesTheOperator(t *testing.T) {
	store := testStore(t)
	entry := mustCreate(t, store, entryInput("Salsa", 1000, 2000, importer))
	title := "Salsa at the museum"
	if _, err := store.UpdateEntry(entry.ID, EntryChange{Title: &title, By: operator.By, ByKind: operator.Kind, Reason: "named it"}); err != nil {
		t.Fatal(err)
	}
	calendarTitle := "Salsa"
	if _, err := store.UpdateEntry(entry.ID, EntryChange{Title: &calendarTitle, By: importer.By, ByKind: importer.Kind, Reason: "calendar"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the importer renamed what the operator named: %v", err)
	}
	visit := LinkInput{LinkType: "visits", Ref: "visit_002273", OverlapSeconds: pointer(600), BearsOn: BearsOnContext, Reason: "overlaps"}
	if _, err := store.AddLink(entry.ID, visit, importer); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveLink(entry.ID, "visits", "visit_002273", nil, operator); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddLink(entry.ID, visit, importer); !errors.Is(err, ErrConflict) {
		t.Fatalf("the importer linked again what the operator took off: %v", err)
	}
	if _, err := store.AddLink(entry.ID, visit, operator); err != nil {
		t.Fatalf("the operator may link it again: %v", err)
	}
	revisions, err := store.Revisions(entry.ID)
	if err != nil || len(revisions) != 1 {
		t.Fatalf("revisions = %v %v", revisions, err)
	}
}

func TestTheOperatorsReviewStandsOverEveryModelReview(t *testing.T) {
	store := testStore(t)
	entry := mustCreate(t, store, entryInput("Workout", 1000, 2000, importer))
	review := func(actor Actor, outcome string) {
		t.Helper()
		input := ReviewInput{Outcome: outcome, Reason: "test", By: actor.By, ByKind: actor.Kind}
		if actor.Kind == ActorModel {
			input.Model = "claude-opus-5-5"
		}
		if _, err := store.AddReview(entry.ID, input); err != nil {
			t.Fatal(err)
		}
	}
	outcome := func() string {
		t.Helper()
		read, err := store.Entry(entry.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		return read.Outcome
	}
	review(reviewer, OutcomeDone)
	if got := outcome(); got != OutcomeDone {
		t.Fatalf("after the model: %s", got)
	}
	review(operator, OutcomeSkipped)
	review(reviewer, OutcomeDone)
	if got := outcome(); got != OutcomeSkipped {
		t.Fatalf("a later model review replaced the operator's: %s", got)
	}
	if _, err := store.AddReview(entry.ID, ReviewInput{Outcome: OutcomeDone, Reason: "rule", By: importer.By, ByKind: importer.Kind}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a program reviewed: %v", err)
	}
	skipped, err := store.Entries(EntryFilter{Outcome: OutcomeSkipped})
	if err != nil || len(skipped) != 1 {
		t.Fatalf("outcome filter: %v %d", err, len(skipped))
	}
}

func TestNewEvidenceAsksForAnotherReview(t *testing.T) {
	store := testStore(t)
	entry := mustCreate(t, store, entryInput("Haircut", 1000, 2000, importer))
	needs := func() int {
		t.Helper()
		entries, err := store.Entries(EntryFilter{NeedsReview: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	if needs() != 1 {
		t.Fatal("an unreviewed entry does not need review")
	}
	if _, err := store.AddReview(entry.ID, ReviewInput{Outcome: OutcomeUnknown, Reason: "no evidence", By: reviewer.By, ByKind: reviewer.Kind, Model: "claude-opus-5-5"}); err != nil {
		t.Fatal(err)
	}
	if needs() != 0 {
		t.Fatal("a reviewed entry still needs review")
	}
	if _, err := store.AddLink(entry.ID, LinkInput{LinkType: "photos", Ref: "photo_000010", BearsOn: BearsOnContext, Reason: "taken then"}, importer); err != nil {
		t.Fatal(err)
	}
	if needs() != 1 {
		t.Fatal("new evidence did not ask for another review")
	}
}

func TestAnEntryCarriesManyKinds(t *testing.T) {
	store := testStore(t)
	kinds, err := store.Kinds()
	if err != nil || len(kinds) != len(SeedKinds) {
		t.Fatalf("seed kinds: %v %d", err, len(kinds))
	}
	byName := map[string]string{}
	for _, kind := range kinds {
		byName[kind.Name] = kind.ID
	}
	input := entryInput("SF Tech Week day", 1000, 2000, importer)
	input.KindIDs = []string{byName["conference"], byName["work"]}
	entry := mustCreate(t, store, input)
	if len(entry.Kinds) != 2 {
		t.Fatalf("kinds = %+v", entry.Kinds)
	}
	if err := store.RemoveKind(entry.ID, byName["work"], operator); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignKind(entry.ID, byName["work"], reviewer, "looks like work"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a model put back a kind the operator took off: %v", err)
	}
	if _, err := store.CreateKind("Tech Meetup", "x", operator); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a capitalized kind name: %v", err)
	}
}

func TestLinksAreChecked(t *testing.T) {
	store := testStore(t)
	entry := mustCreate(t, store, entryInput("Dinner", 1000, 2000, importer))
	for name, link := range map[string]LinkInput{
		"a name instead of an id":   {LinkType: "visits", Ref: "Cafe", BearsOn: BearsOnDone, Reason: "x"},
		"a visit as a source":       {LinkType: "visits", Ref: "visit_000001", BearsOn: BearsOnSource, Reason: "x"},
		"a calendar event, no date": {LinkType: "calendar_events", Ref: "calendar_event_000001", BearsOn: BearsOnContext, Reason: "x"},
		"an amount on a photo":      {LinkType: "photos", Ref: "photo_000001", AmountCents: pointer(5), BearsOn: BearsOnCost, Reason: "x"},
		"no reason":                 {LinkType: "photos", Ref: "photo_000001", BearsOn: BearsOnDone},
		"an unknown type":           {LinkType: "tweets", Ref: "1", BearsOn: BearsOnDone, Reason: "x"},
	} {
		if _, err := store.AddLink(entry.ID, link, importer); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cost := LinkInput{LinkType: "transactions", Ref: "transaction_000042", AmountCents: pointer(2350), BearsOn: BearsOnCost, Reason: "the bill"}
	first, err := store.AddLink(entry.ID, cost, importer)
	if err != nil || *first.AmountCents != 2350 {
		t.Fatalf("cost link: %v %+v", err, first)
	}
	if _, err := store.AddLink(entry.ID, cost, importer); err != nil {
		t.Fatalf("the same link again should answer the live one: %v", err)
	}
	cost.BearsOn = BearsOnDone
	if _, err := store.AddLink(entry.ID, cost, importer); !errors.Is(err, ErrConflict) {
		t.Fatalf("the same record bearing differently: %v", err)
	}
}

func TestRoutesRefuseUnknownFields(t *testing.T) {
	store := testStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	request := httptest.NewRequest("POST", "/time-entries", strings.NewReader(`{"title":"x","starts_at":1,"ends_at":2,"time_zone":"UTC","by":"a","by_kind":"program","reason":"r","outcome":"done"}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", response.Code, response.Body)
	}
	request = httptest.NewRequest("POST", "/time-entries", strings.NewReader(`{"title":"x","starts_at":1,"ends_at":2,"time_zone":"UTC","by":"a","by_kind":"program","reason":"r",
		"links":[{"link_type":"trips","ref":"trip_002596","bears_on":"source"}]}`))
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/links/trips?ref=trip_002596", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "time_entry_000001") {
		t.Fatalf("links to the trip: %d %s", response.Code, response.Body)
	}
}
