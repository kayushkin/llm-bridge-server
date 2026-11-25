package timeledgerstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

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

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("answer %d does not decode: %v: %s", recorder.Code, err, recorder.Body.String())
	}
	return value
}

const trainingEntry = `{"title":"Training","starts_at":1000,"ends_at":9000,"time_zone":"UTC","by":"me","by_kind":"operator","reason":"went",
	"links":[{"link_type":"calendar_events","ref":"calendar_event_000007","occurrence_starts_at":1000,"bears_on":"source"}]}`

func TestARequestWithNoCallerIsRefused(t *testing.T) {
	mux := routes(t)
	for _, route := range [][2]string{{"GET", "/health"}, {"GET", "/vocabularies"}, {"GET", "/kinds"}, {"POST", "/kinds"},
		{"GET", "/time-entries"}, {"POST", "/time-entries"}, {"GET", "/time-entries/time_entry_000001"}, {"GET", "/links/trips?ref=trip_000001"},
		{"GET", "/reviews/time_entry_review_000001"}} {
		if code := request(t, mux, nil, route[0], route[1], `{}`).Code; code != http.StatusUnauthorized {
			t.Errorf("%s %s with no caller = %d", route[0], route[1], code)
		}
	}
	// This server's own service names nobody, so it cannot own an entry or a kind.
	if code := request(t, mux, thisServer, "POST", "/time-entries", trainingEntry).Code; code != http.StatusForbidden {
		t.Errorf("an entry made for nobody = %d", code)
	}
	if code := request(t, mux, thisServer, "POST", "/kinds", `{"name":"chores","description":"x","by":"me","by_kind":"operator"}`).Code; code != http.StatusForbidden {
		t.Errorf("a kind made for nobody = %d", code)
	}
}

// Alice and Bob each keep an account with an entry from the same calendar
// date and a kind of the same name; neither reaches the other's records.
func TestOwnersReachOnlyTheirOwnRecords(t *testing.T) {
	mux := routes(t)
	aliceEntry := decode[TimeEntry](t, request(t, mux, alice, "POST", "/time-entries", trainingEntry))
	if aliceEntry.ID != "time_entry_000001" || aliceEntry.OwnerPrincipalID != aliceID {
		t.Fatalf("alice's entry = %+v", aliceEntry)
	}
	recorder := request(t, mux, bob, "POST", "/time-entries", trainingEntry)
	bobEntry := decode[TimeEntry](t, recorder)
	if recorder.Code != http.StatusCreated || bobEntry.ID != "time_entry_000002" || bobEntry.OwnerPrincipalID != bobID {
		t.Fatalf("bob's entry from the same source date = %d %+v; a source is one entry's within one owner", recorder.Code, bobEntry)
	}
	kind := `{"name":"chores","description":"housework","by":"me","by_kind":"operator"}`
	aliceKind := decode[Kind](t, request(t, mux, alice, "POST", "/kinds", kind))
	recorder = request(t, mux, bob, "POST", "/kinds", kind)
	bobKind := decode[Kind](t, recorder)
	if recorder.Code != http.StatusCreated || bobKind.OwnerPrincipalID != bobID || bobKind.ID == aliceKind.ID {
		t.Fatalf("bob's kind with alice's name = %d %+v", recorder.Code, bobKind)
	}
	if code := request(t, mux, alice, "POST", "/kinds", kind).Code; code != http.StatusConflict {
		t.Errorf("alice's second kind of the same name = %d", code)
	}
	if code := request(t, mux, alice, "PUT", "/time-entries/time_entry_000001/kinds/"+aliceKind.ID, `{"by":"me","by_kind":"operator","reason":"r"}`).Code; code != http.StatusOK {
		t.Fatalf("alice puts her kind on = %d", code)
	}
	if code := request(t, mux, alice, "PUT", "/time-entries/time_entry_000001/kinds/"+bobKind.ID, `{"by":"me","by_kind":"operator","reason":"r"}`).Code; code != http.StatusBadRequest {
		t.Errorf("alice puts bob's kind on her entry = %d", code)
	}
	review := decode[Review](t, request(t, mux, alice, "POST", "/time-entries/time_entry_000001/reviews", `{"outcome":"done","reason":"photos","by":"me","by_kind":"operator"}`))
	if review.ID != "time_entry_review_000001" || review.OwnerPrincipalID != aliceID {
		t.Fatalf("alice's review = %+v", review)
	}

	// Bob reaches none of alice's records: each answers as a missing one.
	for _, route := range [][3]string{
		{"GET", "/time-entries/time_entry_000001", ""},
		{"GET", "/time-entries/time_entry_000001/revisions", ""},
		{"GET", "/time-entries/time_entry_000001/reviews", ""},
		{"GET", "/kinds/" + aliceKind.ID, ""},
		{"GET", "/reviews/time_entry_review_000001", ""},
		{"PATCH", "/time-entries/time_entry_000001", `{"title":"Mine","by":"bob","by_kind":"operator","reason":"r"}`},
		{"DELETE", "/time-entries/time_entry_000001?by=bob&by_kind=operator", ""},
		{"PUT", "/time-entries/time_entry_000001/kinds/" + bobKind.ID, `{"by":"bob","by_kind":"operator","reason":"r"}`},
		{"DELETE", "/time-entries/time_entry_000001/kinds/" + aliceKind.ID + "?by=bob&by_kind=operator", ""},
		{"POST", "/time-entries/time_entry_000001/links/photos", `{"ref":"photo_000001","bears_on":"done","reason":"r","by":"bob","by_kind":"operator"}`},
		{"DELETE", "/time-entries/time_entry_000001/links/calendar_events?ref=calendar_event_000007&occurrence_starts_at=1000&by=bob&by_kind=operator", ""},
		{"POST", "/time-entries/time_entry_000001/reviews", `{"outcome":"skipped","reason":"r","by":"bob","by_kind":"operator"}`},
	} {
		if recorder := request(t, mux, bob, route[0], route[1], route[2]); recorder.Code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d %s", route[0], route[1], recorder.Code, recorder.Body.String())
		}
	}
	after := decode[TimeEntry](t, request(t, mux, alice, "GET", "/time-entries/time_entry_000001", ""))
	if after.Title != "Training" || after.Outcome != OutcomeDone || len(after.Kinds) != 1 || len(after.Links) != 1 || after.DeletedAt != nil {
		t.Errorf("bob's refused calls changed alice's entry: %+v", after)
	}

	// A parent of another owner is refused as a missing one.
	child := `{"title":"Warm-up","starts_at":1000,"ends_at":2000,"time_zone":"UTC","by":"bob","by_kind":"operator","reason":"r","parent_time_entry_id":"time_entry_000001"}`
	if recorder := request(t, mux, bob, "POST", "/time-entries", child); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "does not exist") {
		t.Errorf("bob's entry inside alice's = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := request(t, mux, bob, "PATCH", "/time-entries/time_entry_000002", `{"parent_time_entry_id":"time_entry_000001","by":"bob","by_kind":"operator","reason":"r"}`); recorder.Code != http.StatusBadRequest {
		t.Errorf("bob moving his entry inside alice's = %d %s", recorder.Code, recorder.Body.String())
	}

	// Lists hold only the caller's own.
	if got := decode[[]TimeEntry](t, request(t, mux, bob, "GET", "/time-entries", "")); len(got) != 1 || got[0].ID != "time_entry_000002" {
		t.Errorf("bob's entries = %+v", got)
	}
	if got := decode[[]Link](t, request(t, mux, bob, "GET", "/links/calendar_events?ref=calendar_event_000007", "")); len(got) != 1 || got[0].TimeEntryID != "time_entry_000002" {
		t.Errorf("bob's links to the calendar event = %+v", got)
	}
	for _, kind := range decode[[]Kind](t, request(t, mux, bob, "GET", "/kinds", "")) {
		if kind.OwnerPrincipalID != bobID {
			t.Errorf("bob's kinds hold %+v", kind)
		}
	}
	if got := decode[[]TimeEntry](t, request(t, mux, bob, "GET", "/time-entries?kind_id="+aliceKind.ID, "")); len(got) != 0 {
		t.Errorf("bob's entries of alice's kind = %+v", got)
	}
	bobHealth := decode[map[string]any](t, request(t, mux, bob, "GET", "/health", ""))
	if counts := bobHealth["counts"].(map[string]any); counts["time_entries"] != 1.0 || counts["reviews"] != 0.0 || counts["links"] != 1.0 ||
		counts["kinds"] != float64(len(SeedKinds)+1) {
		t.Errorf("bob's health = %+v", bobHealth)
	}
	if code := request(t, mux, bob, "GET", "/time-entries?owner_principal_id="+aliceID, "").Code; code != http.StatusForbidden {
		t.Errorf("bob naming alice as owner = %d", code)
	}

	// An administrator sees both, and narrows to one owner when asked.
	if got := decode[[]TimeEntry](t, request(t, mux, administrator, "GET", "/time-entries", "")); len(got) != 2 {
		t.Errorf("the administrator's entries = %d, want both owners'", len(got))
	}
	narrowed := decode[[]TimeEntry](t, request(t, mux, administrator, "GET", "/time-entries?owner_principal_id="+bobID, ""))
	if len(narrowed) != 1 || narrowed[0].OwnerPrincipalID != bobID {
		t.Errorf("the administrator narrowed to bob = %+v", narrowed)
	}
	if code := request(t, mux, administrator, "GET", "/reviews/time_entry_review_000001", "").Code; code != http.StatusOK {
		t.Errorf("the administrator reading alice's review = %d", code)
	}
	if got := decode[[]Link](t, request(t, mux, thisServer, "GET", "/links/calendar_events?ref=calendar_event_000007", "")); len(got) != 2 {
		t.Errorf("this server's links to the calendar event = %+v", got)
	}
	// An administrator writing on alice's entry writes it in alice's account.
	recorder = request(t, mux, administrator, "POST", "/time-entries/time_entry_000001/links/photos", `{"ref":"photo_000001","bears_on":"done","reason":"r","by":"admin","by_kind":"operator"}`)
	if recorder.Code != http.StatusCreated {
		t.Errorf("the administrator linking a photo to alice's entry = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := decode[[]Link](t, request(t, mux, alice, "GET", "/links/photos?ref=photo_000001", "")); len(got) != 1 {
		t.Errorf("alice's photo links after the administrator's = %+v", got)
	}
}

func TestEachOwnerStartsWithTheSeedKinds(t *testing.T) {
	store := testStore(t)
	for _, scope := range []ownership.Scope{aliceScope, bobScope} {
		kinds, err := store.Kinds(testContext, scope)
		if err != nil || len(kinds) != len(SeedKinds) || kinds[0].OwnerPrincipalID != scope.OwnerPrincipalID() {
			t.Fatalf("%s's kinds = %d %v", scope, len(kinds), err)
		}
	}
	again, err := store.Kinds(testContext, aliceScope)
	if err != nil || len(again) != len(SeedKinds) {
		t.Fatalf("alice's kinds read again = %d %v", len(again), err)
	}
	every, err := store.Kinds(testContext, ownership.EveryOwner())
	if err != nil || len(every) != 2*len(SeedKinds) {
		t.Fatalf("every owner's kinds = %d %v", len(every), err)
	}
}

func TestAZeroScopeReachesNothing(t *testing.T) {
	store := testStore(t)
	if _, err := store.Entries(testContext, ownership.Scope{}, EntryFilter{}); err == nil {
		t.Error("a scope that was never set listed entries")
	}
	if _, err := store.Kinds(testContext, ownership.Scope{}); err == nil {
		t.Error("a scope that was never set listed kinds")
	}
	if _, err := store.CreateEntry(testContext, "", entryInput("x", 1, 2, importer)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an entry with no owner: %v", err)
	}
	if _, err := store.CreateKind(testContext, "alice", "chores", "x", operator); !errors.Is(err, ErrInvalid) {
		t.Errorf("a kind owned by a name: %v", err)
	}
}

func TestMalformedIDsAreNotFound(t *testing.T) {
	mux := routes(t)
	for _, path := range []string{"/time-entries/nonsense", "/time-entries/time_entry_1", "/time-entries/time_entry_kind_000001", "/kinds/time_entry_000001",
		"/reviews/nonsense", "/time-entries/time_entry_000009"} {
		if code := request(t, mux, alice, "GET", path, "").Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, code)
		}
	}
	if got := decode[[]TimeEntry](t, request(t, mux, alice, "GET", "/time-entries?parent=nonsense", "")); len(got) != 0 {
		t.Errorf("entries inside a malformed parent = %+v", got)
	}
}
