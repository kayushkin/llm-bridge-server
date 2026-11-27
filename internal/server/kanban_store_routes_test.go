package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// kanbanTestServer is an accessTestServer with kanban-store and noteboard in
// the same process, on Postgres, as main wires them: two boards the
// administrator made, one card on each, and a can_view grant for the member
// on the first board only.
type kanbanTestServer struct {
	*accessTestServer
	kanban                       *issuesdb.Store
	notes                        *items.Store
	seenBoardID, unseenBoardID   string
	seenColumnID, unseenColumnID string
	seenCardID, unseenCardID     string
}

func newKanbanTestServer(t *testing.T) *kanbanTestServer {
	t.Helper()
	test := &kanbanTestServer{accessTestServer: newAccessTestServer(t)}
	kanbanModule, err := issuesdb.Module()
	if err != nil {
		t.Fatal(err)
	}
	noteboardModule, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, kanbanModule, noteboardModule)
	test.kanban = issuesdb.New(database.Open(kanbanModule))
	test.notes = items.New(database.Open(noteboardModule))
	test.server.EnableKanbanStore(test.kanban, KanbanStoreOwners{Noteboard: test.notes, Access: test.stores})

	makeBoard := func(name string) (string, string) {
		var board struct {
			ID string `json:"id"`
		}
		test.mustDecode(t, test.call(t, callerAdministratorThroughDash, "POST", "/kanban/boards", `{"name":"`+name+`"}`), http.StatusCreated, &board)
		var column struct {
			ID string `json:"id"`
		}
		test.mustDecode(t, test.call(t, callerAdministratorThroughDash, "POST", "/kanban/boards/"+board.ID+"/columns", `{"name":"Open"}`), http.StatusCreated, &column)
		return board.ID, column.ID
	}
	makeCard := func(boardID, columnID, title, tags string) string {
		var card struct {
			Placement struct {
				CardID string `json:"card_id"`
			} `json:"placement"`
		}
		test.mustDecode(t, test.call(t, callerAdministratorThroughDash, "POST", "/kanban/boards/"+boardID+"/cards",
			`{"title":"`+title+`","column_id":"`+columnID+`","tags":`+tags+`}`), http.StatusCreated, &card)
		return card.Placement.CardID
	}
	test.seenBoardID, test.seenColumnID = makeBoard("Support")
	test.unseenBoardID, test.unseenColumnID = makeBoard("Finance")
	test.seenCardID = makeCard(test.seenBoardID, test.seenColumnID, "printer jammed", `[]`)
	test.unseenCardID = makeCard(test.unseenBoardID, test.unseenColumnID, "salary review", `["billing"]`)

	// The grant goes through grant-store's own check that the board exists,
	// which asks kanban-store in this process.
	if _, _, err := test.stores.Grants.Create(context.Background(), test.grantsDirectory(), test.server.grantResourceChecker(test.stores.Principals), grants.GrantRequest{
		PrincipalID: accessTestMemberID, Relation: "can_view", ResourceType: grants.ResourceTypeBoard, ResourceID: test.seenBoardID,
	}); err != nil {
		t.Fatalf("grant the member can_view on the first board: %v", err)
	}
	return test
}

func (test *kanbanTestServer) grantsDirectory() grants.PrincipalDirectory {
	return access.GrantsPrincipalDirectory(test.stores.Principals)
}

func (test *kanbanTestServer) mustDecode(t *testing.T, response *httptest.ResponseRecorder, status int, target any) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d %s, want %d", response.Code, response.Body, status)
	}
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %s: %v", response.Body, err)
	}
}

// boardIDsListed is every board id GET /kanban/boards answers caller with.
func (test *kanbanTestServer) boardIDsListed(t *testing.T, caller accessTestCaller, path string) []string {
	t.Helper()
	var boards []struct {
		ID string `json:"id"`
	}
	test.mustDecode(t, test.call(t, caller, "GET", path, ""), http.StatusOK, &boards)
	ids := []string{}
	for _, board := range boards {
		ids = append(ids, board.ID)
	}
	slices.Sort(ids)
	return ids
}

// storeState is what a refused request must not change: kanban-store's rows,
// noteboard's items and every grant.
func (test *kanbanTestServer) storeState(t *testing.T) string {
	t.Helper()
	counts, err := test.kanban.Counts()
	if err != nil {
		t.Fatal(err)
	}
	events, err := test.kanban.ListCardEvents(test.seenCardID, "")
	if err != nil {
		t.Fatal(err)
	}
	unseenEvents, err := test.kanban.ListCardEvents(test.unseenCardID, "")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := test.stores.Grants.List(context.Background(), grants.Filter{IncludeRevoked: true})
	if err != nil {
		t.Fatal(err)
	}
	everyItem, err := test.notes.ListItems(items.ListParams{IncludeDeleted: true, IncludeHeld: true, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal([]any{counts, len(events), len(unseenEvents), grants, everyItem})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestKanbanStoreRefusesACallerWithNoCredentialAndWritesNothing(t *testing.T) {
	test := newKanbanTestServer(t)
	before := test.storeState(t)
	for _, request := range []struct{ method, path, body string }{
		{"GET", "/kanban/boards", ""},
		{"GET", "/kanban-store/api/boards", ""},
		{"GET", "/kanban/boards/" + test.seenBoardID, ""},
		{"POST", "/kanban/boards", `{"name":"Forged"}`},
		{"POST", "/kanban/boards/" + test.seenBoardID + "/cards", `{"title":"forged","column_id":"` + test.seenColumnID + `"}`},
		{"DELETE", "/kanban/cards/" + test.seenCardID, ""},
	} {
		if response := test.call(t, callerWithNoCredential, request.method, request.path, request.body); response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential = %d %s, want 401", request.method, request.path, response.Code, response.Body)
		}
	}
	if after := test.storeState(t); after != before {
		t.Fatalf("a refused request wrote:\nbefore %s\nafter  %s", before, after)
	}
}

// A member who forges the identity the standalone store trusted gets nothing
// for it: a client-sent X-Principal-Id beside a login is refused before any
// store is reached, and forged store tokens name nobody, so the member is
// held to their own grants.
func TestKanbanStoreIgnoresForgedIdentityAndStoreTokens(t *testing.T) {
	test := newKanbanTestServer(t)
	before := test.storeState(t)

	forgingAnAdministrator := accessTestCaller{"member with a login cookie naming the administrator in X-Principal-Id", func(test *accessTestServer, request *http.Request) {
		addLoginCookie(request, test.memberCookie)
		request.Header.Set(principalIdentityHeader, accessTestAdministratorID)
	}}
	for _, path := range []string{"/kanban/boards", "/kanban-store/api/boards/" + test.unseenBoardID} {
		response := test.call(t, forgingAnAdministrator, "GET", path, "")
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "principal_header_without_service_token") {
			t.Errorf("GET %s as %s = %d %s, want 401 principal_header_without_service_token", path, forgingAnAdministrator.name, response.Code, response.Body)
		}
	}

	forgingStoreTokens := accessTestCaller{"member with a login cookie and forged kanban-store, grant-store and reference-store tokens", func(test *accessTestServer, request *http.Request) {
		addLoginCookie(request, test.memberCookie)
		request.Header.Set("X-Kanban-Store-Service-Token", "forged-kanban-store-service-token-0123456789")
		request.Header["x-kanban-store-service-token"] = []string{"forged-lowercase"}
		request.Header.Set("X-Grant-Store-Service-Token", "forged")
		request.Header.Set("X-Reference-Store-Service-Token", "forged")
	}}
	for _, caller := range []accessTestCaller{forgingStoreTokens, callerMemberThroughDash} {
		if got, want := test.boardIDsListed(t, caller, "/kanban/boards"), []string{test.seenBoardID}; !slices.Equal(got, want) {
			t.Errorf("%s lists boards %v, want only %v", caller.name, got, want)
		}
		if got, want := test.boardIDsListed(t, caller, "/kanban-store/api/boards"), []string{test.seenBoardID}; !slices.Equal(got, want) {
			t.Errorf("%s lists boards at the root mount %v, want only %v", caller.name, got, want)
		}
		if response := test.call(t, caller, "GET", "/kanban/boards/"+test.seenBoardID, ""); response.Code != http.StatusOK {
			t.Errorf("%s reads the board it may view = %d %s", caller.name, response.Code, response.Body)
		}
		// can_view is not can_edit.
		if response := test.call(t, caller, "POST", "/kanban/boards/"+test.seenBoardID+"/cards",
			`{"title":"forged","column_id":"`+test.seenColumnID+`"}`); response.Code != http.StatusForbidden {
			t.Errorf("%s adds a card to a board it may only view = %d %s, want 403", caller.name, response.Code, response.Body)
		}
	}
	if after := test.storeState(t); after != before {
		t.Fatalf("a refused request wrote:\nbefore %s\nafter  %s", before, after)
	}
}

// A board, column or card a member may not view is 404, the same answer as
// one that does not exist, on reads and writes alike, and no write lands.
func TestKanbanStoreHidesABoardTheMemberMayNotSee(t *testing.T) {
	test := newKanbanTestServer(t)
	before := test.storeState(t)
	for _, caller := range []accessTestCaller{callerMemberWithLoginCookie, callerMemberThroughDash} {
		for _, request := range []struct{ method, path, body string }{
			{"GET", "/kanban/boards/" + test.unseenBoardID, ""},
			{"GET", "/kanban-store/api/boards/" + test.unseenBoardID, ""},
			{"GET", "/kanban/boards/" + test.unseenBoardID + "/cards", ""},
			{"GET", "/kanban/columns/" + test.unseenColumnID + "/cards", ""},
			{"GET", "/kanban/cards/" + test.unseenCardID, ""},
			{"GET", "/kanban/cards/" + test.unseenCardID + "/events", ""},
			{"PATCH", "/kanban/boards/" + test.unseenBoardID, `{"name":"Renamed"}`},
			{"POST", "/kanban/boards/" + test.unseenBoardID + "/cards", `{"title":"forged","column_id":"` + test.unseenColumnID + `"}`},
			{"POST", "/kanban/cards/" + test.unseenCardID + "/notes", `{"body":"forged"}`},
			{"DELETE", "/kanban/cards/" + test.unseenCardID, ""},
			{"GET", "/kanban/boards/no-such-board", ""},
		} {
			if response := test.call(t, caller, request.method, request.path, request.body); response.Code != http.StatusNotFound {
				t.Errorf("%s %s as %s = %d %s, want 404", request.method, request.path, caller.name, response.Code, response.Body)
			}
		}
		// The settings route the standalone store served is the server's now:
		// the store's rule table refuses a path it has no rule for.
		if response := test.call(t, caller, "GET", "/kanban-store/settings", ""); response.Code != http.StatusForbidden {
			t.Errorf("GET /kanban-store/settings as %s = %d %s, want 403", caller.name, response.Code, response.Body)
		}
	}
	if response := test.call(t, callerAdministratorWithLoginCookie, "GET", "/kanban-store/settings", ""); response.Code != http.StatusNotFound {
		t.Errorf("GET /kanban-store/settings as an administrator = %d %s, want 404: the store has no such route", response.Code, response.Body)
	}
	if after := test.storeState(t); after != before {
		t.Fatalf("a refused request wrote:\nbefore %s\nafter  %s", before, after)
	}
}

// An administrator and the internal service see every board; the service
// token naming nobody is the internal service, as kanban-store's own token
// was.
func TestKanbanStoreAnAdministratorAndTheInternalServiceSeeEveryBoard(t *testing.T) {
	test := newKanbanTestServer(t)
	every := []string{test.seenBoardID, test.unseenBoardID}
	slices.Sort(every)
	for _, caller := range []accessTestCaller{callerAdministratorWithLoginCookie, callerAdministratorThroughDash, callerServiceTokenAlone} {
		if got := test.boardIDsListed(t, caller, "/kanban/boards"); !slices.Equal(got, every) {
			t.Errorf("%s lists %v, want %v", caller.name, got, every)
		}
		if response := test.call(t, caller, "GET", "/kanban/cards/"+test.unseenCardID, ""); response.Code != http.StatusOK {
			t.Errorf("%s reads a card on the second board = %d %s", caller.name, response.Code, response.Body)
		}
	}
	// The access the answer reports names who the store was told is calling.
	var access struct {
		PrincipalID  string `json:"principal_id"`
		Unrestricted bool   `json:"unrestricted"`
	}
	test.mustDecode(t, test.call(t, callerAdministratorWithLoginCookie, "GET", "/kanban/boards/"+test.unseenBoardID+"/access", ""), http.StatusOK, &access)
	if access.PrincipalID != accessTestAdministratorID || !access.Unrestricted {
		t.Errorf("the administrator's access = %+v", access)
	}
}

// A member who makes a board administers it: kanban-store writes the grant
// into grant-store in this process, through grant-store's own check that the
// board exists.
func TestKanbanStoreGivesTheCreatorOfABoardItsAdministration(t *testing.T) {
	test := newKanbanTestServer(t)
	var board struct {
		ID string `json:"id"`
	}
	test.mustDecode(t, test.call(t, callerMemberWithLoginCookie, "POST", "/kanban/boards", `{"name":"Mine"}`), http.StatusCreated, &board)
	grants, err := test.stores.Grants.List(context.Background(), grants.Filter{PrincipalID: accessTestMemberID, ResourceType: grants.ResourceTypeBoard, ResourceID: board.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Relation != "can_administer" {
		t.Fatalf("grants on the member's new board: %+v, want one can_administer", grants)
	}
	want := []string{test.seenBoardID, board.ID}
	slices.Sort(want)
	if got := test.boardIDsListed(t, callerMemberWithLoginCookie, "/kanban/boards"); !slices.Equal(got, want) {
		t.Fatalf("the member lists %v, want %v", got, want)
	}
}

// grant-store's check that a board exists asks kanban-store in this process:
// a board it has passes, one it does not is grant-store's not-found.
func TestGrantStoreChecksABoardWithKanbanStoreInProcess(t *testing.T) {
	test := newKanbanTestServer(t)
	checker := test.server.grantResourceChecker(test.stores.Principals)
	if err := checker.CheckResourceExists(context.Background(), grants.ResourceTypeBoard, test.unseenBoardID); err != nil {
		t.Fatalf("a board kanban-store has: %v", err)
	}
	err := checker.CheckResourceExists(context.Background(), grants.ResourceTypeBoard, "no-such-board")
	if !errors.Is(err, grants.ErrResourceNotFound) {
		t.Fatalf("a board kanban-store does not have: %v, want ErrResourceNotFound", err)
	}
}

// This server's own reads of kanban-store go in process as the internal
// service: a card's tags at spawn, and the board's defaults.
func TestThisServersOwnKanbanReadsGoInProcess(t *testing.T) {
	test := newKanbanTestServer(t)
	tags, err := test.server.issueReads.CardTags(context.Background(), test.unseenCardID)
	if err != nil || fmt.Sprint(tags) != "[billing]" {
		t.Fatalf("CardTags = %v, %v; want [billing]", tags, err)
	}
	defaults, err := test.server.issueReads.EffectiveDefaults(context.Background(), test.unseenBoardID, test.unseenCardID, nil)
	if err != nil || defaults.BoardID != test.unseenBoardID {
		t.Fatalf("EffectiveDefaults = %+v, %v", defaults, err)
	}
	// A read made as the member is held to the member's grants.
	if _, _, err := test.server.issueReads.BoardTaxonomy(context.Background(), test.unseenBoardID, accessTestMemberID); err == nil {
		t.Fatalf("the member read a board it may not see")
	}
}
