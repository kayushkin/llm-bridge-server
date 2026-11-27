package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendar"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journal"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/people"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enableConnectedRecordsOnTestDatabase serves people-store, calendar-store and
// journal-store from a Postgres database of the test's own, after the
// noteboard and reference-store they read.
func enableConnectedRecordsOnTestDatabase(t *testing.T, gated *gatedTestServer) *items.Store {
	t.Helper()
	noteboard, _ := enableTaskTrackingOnTestDatabase(t, gated.server)
	enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	peopleModule, err := people.Module()
	if err != nil {
		t.Fatal(err)
	}
	calendarModule, err := calendar.Module()
	if err != nil {
		t.Fatal(err)
	}
	journalModule, err := journal.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, peopleModule, calendarModule, journalModule)
	if err := gated.server.EnableConnectedRecords(context.Background(), ConnectedRecordsModules{
		People: database.Open(peopleModule), Calendar: database.Open(calendarModule), Journal: database.Open(journalModule),
	}); err != nil {
		t.Fatal(err)
	}
	return noteboard
}

// requestAsPrincipalWithBody is a call through dash for a principal, with a
// body that is not JSON.
func (gated *gatedTestServer) requestAsPrincipalWithBody(t *testing.T, principalID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set(serviceTokenHeader, testServiceToken)
	request.Header.Set(principalIdentityHeader, principalID)
	return serve(gated.server, request)
}

func decodeRecord(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatalf("answer %d is not a JSON object: %v", response.Code, err)
	}
	return record
}

func TestTheConnectedRecordsStoresAnswer503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for path, code := range map[string]string{"/people-store/persons": "people_store_off", "/calendar-store/events": "calendar_store_off", "/journal-store/entries": "journal_store_off"} {
		if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), code) {
			t.Errorf("GET %s = %d %s, want 503 %s", path, response.Code, response.Body, code)
		}
	}
}

// A person belongs to whoever made it: another principal cannot read, list or
// change it, an administrator and the internal service see it, and nobody at
// all is 401.
func TestAPersonIsReachedOnlyByItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Alice",
		"identities": []map[string]any{{"kind": "email", "value": "alice@example.com", "linked_by": "operator"}}})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	person := decodeRecord(t, created)
	id, _ := person["id"].(string)
	if person["owner_principal_id"] != firstTestPrincipalID || id == "" {
		t.Fatalf("the new person's id %q and owner %v", id, person["owner_principal_id"])
	}
	path := "/people-store/persons/" + id

	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", path, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "PATCH", path, map[string]any{"how_met": "school"}); response.Code != http.StatusNotFound {
		t.Errorf("another principal's PATCH = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/people-store/persons", nil); response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Errorf("another principal's list = %d %s, want an empty list", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/people-store/identities/resolve?kind=email&value=alice@example.com", nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal resolving the owner's handle = %d, want 404", response.Code)
	}
	// Request authorization reads the session agent token and the cookie the
	// same way; a login cookie is the second credential checked here.
	second := gated.loginAs(t, secondTestPrincipalID)
	if response := gated.requestAs(t, second, "GET", path, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET with a login cookie = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the owner's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", path, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusOK {
		t.Errorf("the internal service's GET = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
		t.Errorf("GET with no credential = %d, want 401", response.Code)
	}
	// The internal service names nobody, so it cannot own a new person.
	if response := gated.requestAsServiceWithJSON(t, "POST", "/people-store/persons", map[string]any{"display_name": "Nobody's"}); response.Code != http.StatusForbidden {
		t.Errorf("the internal service making a person = %d %s, want 403", response.Code, response.Body)
	}
}

// journal-store answers only its owner's or an administrator's principal:
// the internal service acting as nobody is refused on every route.
func TestTheJournalAnswersOnlyItsOwnerAndAnAdministrator(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	noteboard := enableConnectedRecordsOnTestDatabase(t, gated)
	for _, path := range []string{"/journal-store/health", "/journal-store/entry-kinds", "/journal-store/entries"} {
		if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "journal_store_requires_a_principal") {
			t.Errorf("GET %s as the internal service = %d %s, want 403 journal_store_requires_a_principal", path, response.Code, response.Body)
		}
	}
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/journal-store/entries", map[string]any{"kind": "note", "title": "Groceries", "body": "- [ ] buy milk"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	entry := decodeRecord(t, created)
	id, _ := entry["id"].(string)
	if entry["owner_principal_id"] != firstTestPrincipalID {
		t.Fatalf("the entry's owner is %v", entry["owner_principal_id"])
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/journal-store/entries/"+id, nil); response.Code != http.StatusNotFound {
		t.Errorf("another principal's GET = %d, want 404", response.Code)
	}
	if response := gated.requestAsPrincipal(t, secondTestPrincipalID, "GET", "/journal-store/entries", nil); response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Errorf("another principal's list = %d %s, want an empty list", response.Code, response.Body)
	}
	if response := gated.requestAsPrincipal(t, administratorTestPrincipalID, "GET", "/journal-store/entries/"+id, nil); response.Code != http.StatusOK {
		t.Errorf("the administrator's GET = %d %s", response.Code, response.Body)
	}

	// promote-todo writes into noteboard, which every agent reads: the owner
	// who is not an administrator is refused, and an administrator's own
	// entry makes the todo through noteboard in this process.
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/journal-store/entries/"+id+"/promote-todo", map[string]any{"line": 1}); response.Code != http.StatusForbidden {
		t.Errorf("promote-todo by an owner who is not an administrator = %d %s, want 403", response.Code, response.Body)
	}
	own := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/journal-store/entries", map[string]any{"kind": "note", "title": "Errands", "body": "- [ ] post a letter"})
	ownID, _ := decodeRecord(t, own)["id"].(string)
	promoted := gated.requestAsPrincipal(t, administratorTestPrincipalID, "POST", "/journal-store/entries/"+ownID+"/promote-todo", map[string]any{"line": 1})
	if promoted.Code != http.StatusCreated {
		t.Fatalf("the administrator's promote-todo = %d %s", promoted.Code, promoted.Body)
	}
	todoID, _ := decodeRecord(t, promoted)["todo_id"].(string)
	todo, err := noteboard.GetItem(todoID)
	if err != nil || todo.Type != model.TypeTodo || todo.Title != "post a letter" {
		t.Fatalf("the todo in noteboard: %+v %v", todo, err)
	}
}

// An import links attendees by address in the calendar owner's own people,
// never in anyone else's.
func TestACalendarImportLinksAttendeesFromItsOwnersPeople(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	if response := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Carol",
		"identities": []map[string]any{{"kind": "email", "value": "carol@example.com", "linked_by": "operator"}}}); response.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", response.Code, response.Body)
	}
	calendarFile := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:lunch-1\r\nDTSTART:20240310T200000Z\r\nDTEND:20240310T210000Z\r\nSUMMARY:Lunch\r\n" +
		"ATTENDEE;CN=Carol:mailto:carol@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for principalID, wantLinked := range map[string]float64{firstTestPrincipalID: 1, secondTestPrincipalID: 0} {
		made := gated.requestAsPrincipal(t, principalID, "POST", "/calendar-store/calendars", map[string]any{"name": "Main", "source": "google", "source_ref": "main"})
		if made.Code != http.StatusCreated {
			t.Fatalf("calendar for %s = %d %s", principalID, made.Code, made.Body)
		}
		calendarID, _ := decodeRecord(t, made)["id"].(string)
		imported := gated.requestAsPrincipalWithBody(t, principalID, "POST", "/calendar-store/calendars/"+calendarID+"/imports", calendarFile)
		if imported.Code != http.StatusCreated {
			t.Fatalf("import for %s = %d %s", principalID, imported.Code, imported.Body)
		}
		if linked := decodeRecord(t, imported)["people_linked"]; linked != wantLinked {
			t.Errorf("%s's import linked %v people, want %v", principalID, linked, wantLinked)
		}
	}
}

// Chat links read a sender in the session owner's people, and sign-in trusts
// only people an administrator keeps: a record anyone else keeps cannot point
// a handle at an administrator's login.
func TestChatLinksAndSignInReadPeopleInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	ctx := context.Background()
	for principalID, handle := range map[string]string{firstTestPrincipalID: "pretender", administratorTestPrincipalID: "rootuser"} {
		if response := gated.requestAsPrincipal(t, principalID, "POST", "/people-store/persons", map[string]any{"display_name": "Root",
			"principal_id": administratorTestPrincipalID,
			"identities":   []map[string]any{{"kind": "discord", "value": handle, "linked_by": "operator"}}}); response.Code != http.StatusCreated {
			t.Fatalf("create for %s = %d %s", principalID, response.Code, response.Body)
		}
	}

	senders := chatLinkSendersInPeopleStore{server: gated.server}
	if sender, err := senders.SenderOfAccount(ctx, firstTestPrincipalID, "discord", "pretender"); err != nil || sender.PrincipalID != administratorTestPrincipalID {
		t.Errorf("the owner's own record of a sender: %+v %v", sender, err)
	}
	if sender, err := senders.SenderOfAccount(ctx, secondTestPrincipalID, "discord", "pretender"); err != nil || sender.PersonID != "" {
		t.Errorf("a sender only another principal keeps: %+v %v", sender, err)
	}

	signIn := signInPeople{server: gated.server}
	if person, err := signIn.PersonOwningHandle(ctx, "discord", "pretender"); err != nil || person != nil {
		t.Errorf("sign-in trusted a record a non-administrator keeps: %+v %v", person, err)
	}
	person, err := signIn.PersonOwningHandle(ctx, "discord", "rootuser")
	if err != nil || person == nil || person.PrincipalID != administratorTestPrincipalID || len(person.Identities) != 1 {
		t.Fatalf("sign-in on the administrator's own record: %+v %v", person, err)
	}
	pointing, err := signIn.PersonPointingAtPrincipal(ctx, administratorTestPrincipalID)
	if err != nil || pointing == nil || pointing.ID != person.ID {
		t.Errorf("the person pointing at the administrator: %+v %v, want only the administrator's own record %s", pointing, err, person.ID)
	}
}

// A person's chip needs both the grant and the owner: the owner without a
// grant is refused, another principal is told of nothing, and an
// administrator sees it.
func TestAPersonChipNeedsTheGrantAndTheOwner(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableConnectedRecordsOnTestDatabase(t, gated)
	created := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/people-store/persons", map[string]any{"display_name": "Alice"})
	id, _ := decodeRecord(t, created)["id"].(string)
	resolve := func(principalID string) []map[string]any {
		response := gated.requestAsPrincipal(t, principalID, "POST", "/reference-store/resolve", map[string]any{"ids": []string{id}})
		if response.Code != http.StatusOK {
			t.Fatalf("resolve as %s = %d %s", principalID, response.Code, response.Body)
		}
		var answer struct {
			Results map[string][]map[string]any `json:"results"`
			Errors  []any                       `json:"errors"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || len(answer.Errors) > 0 {
			t.Fatalf("resolve as %s: %v %v", principalID, err, answer.Errors)
		}
		return answer.Results[id]
	}
	if matches := resolve(administratorTestPrincipalID); len(matches) != 1 || matches[0]["refused"] == true || matches[0]["label"] != "Alice" {
		t.Errorf("the administrator: %+v", matches)
	}
	if matches := resolve(firstTestPrincipalID); len(matches) != 1 || matches[0]["refused"] != true {
		t.Errorf("the owner holding no grant: %+v", matches)
	}
	if matches := resolve(secondTestPrincipalID); len(matches) != 0 {
		t.Errorf("another principal: %+v", matches)
	}
}
