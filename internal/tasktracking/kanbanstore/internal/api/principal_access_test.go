package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

const testServiceToken = "service-token-that-is-at-least-32-characters"

// fakeGrantStore answers /principals/{id}/effective from a table that already
// includes group grants, which is what grant-store's expansion produces, and
// records POST /grants so board creation can be observed.
type fakeGrantStore struct {
	mu                sync.Mutex
	grantsByPrincipal map[string][]grantstore.BoardGrant
	refuseNewGrants   bool
}

func (f *fakeGrantStore) give(principalID, relation, boardID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grantsByPrincipal[principalID] = append(f.grantsByPrincipal[principalID],
		grantstore.BoardGrant{PrincipalID: principalID, Relation: relation, BoardID: boardID})
}

func (f *fakeGrantStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}/effective", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(grantstore.ServiceTokenHeader) != "grant-store-token" {
			writeJSON(w, 401, map[string]string{"error": "no grant-store service token"})
			return
		}
		if r.URL.Query().Get("resource_type") != "board" {
			writeJSON(w, 400, map[string]string{"error": "test fake only serves resource_type=board"})
			return
		}
		f.mu.Lock()
		grants, known := f.grantsByPrincipal[r.PathValue("id")]
		f.mu.Unlock()
		if !known {
			writeJSON(w, 404, map[string]string{"error": "principal does not exist in principal-store"})
			return
		}
		out := []map[string]string{}
		for _, grant := range grants {
			out = append(out, map[string]string{"principal_id": grant.PrincipalID, "relation": grant.Relation, "resource_type": "board", "resource_id": grant.BoardID})
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /grants", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(grantstore.ServiceTokenHeader) != "grant-store-token" {
			writeJSON(w, 401, map[string]string{"error": "no grant-store service token"})
			return
		}
		var request map[string]string
		json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		refuse := f.refuseNewGrants
		f.mu.Unlock()
		if refuse {
			writeJSON(w, 502, map[string]string{"error": "kanban-store could not be asked"})
			return
		}
		f.give(request["principal_id"], request["relation"], request["resource_id"])
		writeJSON(w, 201, request)
	})
	return mux
}

func setupWithPrincipalEnforcement(t *testing.T) (http.Handler, *fakeGrantStore, *db.Store) {
	t.Helper()
	grants := &fakeGrantStore{grantsByPrincipal: map[string][]grantstore.BoardGrant{}}
	grantServer := httptest.NewServer(grants.handler())
	t.Cleanup(grantServer.Close)
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	t.Cleanup(principals.Close)
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	t.Cleanup(bridge.Close)
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	t.Cleanup(bundles.Close)
	store, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	notes := httptest.NewServer(newFakeNoteboard().handler())
	t.Cleanup(notes.Close)
	a := api.New(store, noteboard.New(notes.URL), principalstore.New(principals.URL), llmbridge.New(bridge.URL), bundlestore.New(bundles.URL), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	// Cleanups run last-registered first, so this waits for trigger
	// dispatches before the store above is closed.
	t.Cleanup(a.WaitForMessageTriggers)
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{ServiceToken: testServiceToken, Grants: grantstore.New(grantServer.URL, "grant-store-token")})
	return a.Handler(), grants, store
}

// requestAs sends a request with the given headers; asService sends the token,
// a principal id sends X-Principal-Id, and neither sends nothing.
func requestAs(t *testing.T, h http.Handler, headers map[string]string, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

var asService = map[string]string{api.ServiceTokenHeader: testServiceToken}

func asPrincipal(principalID string) map[string]string {
	return map[string]string{api.PrincipalIDHeader: principalID}
}

func mustStatus(t *testing.T, w *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("%s: want %d, got %d: %s", what, want, w.Code, w.Body.String())
	}
}

type twoBoardFixture struct {
	supportBoardID, supportColumnID, financeBoardID, financeColumnID string
	supportCardID, financeCardID, sharedCardID                       string
}

// Two principals in two groups: alice edits Support, bob views Finance, carol
// administers Support. The fake already holds the group-expanded answer.
const (
	alice = "principal_000001"
	bob   = "principal_000004"
	carol = "principal_000005"
	// principal_000008 carries is_administrator in the fake principal-store.
	deploymentAdministrator = "principal_000008"
)

func buildTwoBoards(t *testing.T, h http.Handler, grants *fakeGrantStore) twoBoardFixture {
	t.Helper()
	var f twoBoardFixture
	makeBoard := func(name string) (string, string) {
		w := requestAs(t, h, asService, "POST", "/api/boards", model.CreateBoardRequest{Name: name})
		mustStatus(t, w, 201, "service creates board")
		var board model.Board
		decodeSuccessfulResponse(t, w, &board)
		w = requestAs(t, h, asService, "POST", "/api/boards/"+board.ID+"/columns", model.CreateColumnRequest{Name: "Open"})
		mustStatus(t, w, 201, "service creates column")
		var column model.Column
		decodeSuccessfulResponse(t, w, &column)
		return board.ID, column.ID
	}
	makeCard := func(boardID, columnID, title string) string {
		w := requestAs(t, h, asService, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: title, ColumnID: columnID})
		mustStatus(t, w, 201, "service creates card")
		var card model.CardView
		decodeSuccessfulResponse(t, w, &card)
		return card.Placement.CardID
	}
	f.supportBoardID, f.supportColumnID = makeBoard("Support")
	f.financeBoardID, f.financeColumnID = makeBoard("Finance")
	f.supportCardID = makeCard(f.supportBoardID, f.supportColumnID, "printer on fire")
	f.financeCardID = makeCard(f.financeBoardID, f.financeColumnID, "salary review")
	f.sharedCardID = makeCard(f.supportBoardID, f.supportColumnID, "refund for customer")
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/boards/"+f.financeBoardID+"/cards/"+f.sharedCardID,
		model.AttachCardRequest{ColumnID: f.financeColumnID}), 201, "service puts the shared card on Finance too")

	grants.give(alice, "can_edit", f.supportBoardID)
	grants.give(bob, "can_view", f.financeBoardID)
	grants.give(carol, "can_administer", f.supportBoardID)
	return f
}

func boardIDsIn(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var boards []model.Board
	decodeSuccessfulResponse(t, w, &boards)
	ids := []string{}
	for _, board := range boards {
		ids = append(ids, board.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestPrincipalEnforcementRefusesRequestsWithoutIdentity(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	buildTwoBoards(t, h, grants)

	mustStatus(t, requestAs(t, h, nil, "GET", "/api/boards", nil), 401, "no principal and no token")
	mustStatus(t, requestAs(t, h, map[string]string{api.ServiceTokenHeader: "wrong"}, "GET", "/api/boards", nil), 401, "wrong service token")
	mustStatus(t, requestAs(t, h, asPrincipal("alice"), "GET", "/api/boards", nil), 401, "a name instead of a principal id")
	mustStatus(t, requestAs(t, h, asPrincipal("principal_000099"), "GET", "/api/boards", nil), 401, "a principal grant-store does not know")
	mustStatus(t, requestAs(t, h, nil, "GET", "/health", nil), 200, "health stays open")

	w := requestAs(t, h, asService, "GET", "/api/boards", nil)
	mustStatus(t, w, 200, "service lists boards")
	if got := len(boardIDsIn(t, w)); got != 2 {
		t.Fatalf("service sees every board: want 2, got %d", got)
	}
}

func TestPrincipalSeesOnlyBoardsItHoldsAGrantOn(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/boards", nil)
	mustStatus(t, w, 200, "alice lists boards")
	if got := boardIDsIn(t, w); len(got) != 1 || got[0] != f.supportBoardID {
		t.Fatalf("alice should see only Support, got %v", got)
	}
	w = requestAs(t, h, asPrincipal(bob), "GET", "/api/boards", nil)
	if got := boardIDsIn(t, w); len(got) != 1 || got[0] != f.financeBoardID {
		t.Fatalf("bob should see only Finance, got %v", got)
	}

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/boards/"+f.financeBoardID, nil), 404, "alice reads Finance")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/boards/"+f.financeBoardID+"/cards", nil), 404, "alice reads Finance's cards")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/boards/"+f.supportBoardID+"/cards", nil), 200, "alice reads Support's cards")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.financeCardID+"/timeline", nil), 404, "alice reads a Finance card")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/columns/"+f.financeColumnID+"/cards", nil), 404, "alice reads a Finance column")

	// The shared card is visible to both, and its placements show each only the
	// board they can see.
	w = requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.sharedCardID+"/placements", nil)
	mustStatus(t, w, 200, "alice reads shared card placements")
	var placements []model.Placement
	decodeSuccessfulResponse(t, w, &placements)
	if len(placements) != 1 || placements[0].BoardID != f.supportBoardID {
		t.Fatalf("alice should see only the Support placement, got %+v", placements)
	}
}

func TestPrincipalWritesNeedTheRightLevel(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "POST", "/api/boards/"+f.supportBoardID+"/cards",
		model.CreateCardRequest{Title: "new ticket", ColumnID: f.supportColumnID}), 201, "alice (can_edit) creates a card on Support")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "POST", "/api/boards/"+f.financeBoardID+"/cards",
		model.CreateCardRequest{Title: "sneaky", ColumnID: f.financeColumnID}), 404, "alice creates a card on Finance")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "POST", "/api/boards/"+f.financeBoardID+"/cards",
		model.CreateCardRequest{Title: "view only", ColumnID: f.financeColumnID}), 403, "bob (can_view) creates a card on Finance")

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PATCH", "/api/columns/"+f.supportColumnID,
		map[string]any{"name": "Renamed"}), 403, "alice (can_edit) renames a Support column")
	mustStatus(t, requestAs(t, h, asPrincipal(carol), "PATCH", "/api/columns/"+f.supportColumnID,
		map[string]any{"name": "Renamed"}), 200, "carol (can_administer) renames a Support column")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/boards/"+f.supportBoardID+"/message-triggers", nil),
		403, "alice reads message triggers")

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PATCH", "/api/cards/"+f.supportCardID,
		map[string]any{"title": "printer extinguished"}), 200, "alice edits a Support-only card")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PATCH", "/api/cards/"+f.sharedCardID,
		map[string]any{"title": "rewritten"}), 403, "alice edits a card that also sits on Finance")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PUT", "/api/boards/"+f.supportBoardID+"/cards/"+f.financeCardID,
		model.AttachCardRequest{ColumnID: f.supportColumnID}), 404, "alice pulls a Finance card onto Support")
}

func TestPrincipalListsAreFiltered(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	for _, cardID := range []string{f.supportCardID, f.financeCardID} {
		mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+cardID+"/assignments/"+carol, nil), 201, "service assigns carol")
	}

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/assignments?principal_id="+carol, nil)
	mustStatus(t, w, 200, "alice lists carol's assignments")
	var assignments []model.CardAssignment
	decodeSuccessfulResponse(t, w, &assignments)
	if len(assignments) != 1 || assignments[0].CardID != f.supportCardID {
		t.Fatalf("alice should see only carol's Support assignment, got %+v", assignments)
	}

	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/tags", nil), 403, "tag listing spans boards")

	searchTitles := func(principalID, query string) []string {
		w := requestAs(t, h, asPrincipal(principalID), "GET", "/api/search?q="+query, nil)
		mustStatus(t, w, 200, principalID+" searches "+query)
		var items []map[string]any
		decodeSuccessfulResponse(t, w, &items)
		titles := []string{}
		for _, item := range items {
			titles = append(titles, item["title"].(string))
		}
		sort.Strings(titles)
		return titles
	}
	if got := searchTitles(alice, "salary"); len(got) != 0 {
		t.Fatalf("alice must not find a Finance-only card by search, got %v", got)
	}
	if got := searchTitles(bob, "salary"); len(got) != 1 {
		t.Fatalf("bob should find the Finance card, got %v", got)
	}
	if got := searchTitles(alice, "r"); len(got) != 2 || got[0] != "printer on fire" || got[1] != "refund for customer" {
		t.Fatalf("alice should find exactly the two cards on Support, got %v", got)
	}
}

func TestPrincipalCreatingABoardAdministersIt(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	w := requestAs(t, h, asPrincipal(bob), "GET", "/api/boards", nil)
	mustStatus(t, w, 401, "bob before holding any grant is unknown to the fake")

	grants.give(bob, "can_view", "some-other-board")
	w = requestAs(t, h, asPrincipal(bob), "POST", "/api/boards", model.CreateBoardRequest{Name: "Bob's"})
	mustStatus(t, w, 201, "bob creates a board")
	var board model.Board
	decodeSuccessfulResponse(t, w, &board)
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "PATCH", "/api/boards/"+board.ID, map[string]any{"name": "Bob's board"}), 200, "bob administers what he created")

	grants.mu.Lock()
	grants.refuseNewGrants = true
	grants.mu.Unlock()
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "POST", "/api/boards", model.CreateBoardRequest{Name: "Orphan"}), 502, "grant refused")
	w = requestAs(t, h, asService, "GET", "/api/boards", nil)
	var boards []model.Board
	decodeSuccessfulResponse(t, w, &boards)
	for _, listed := range boards {
		if listed.Name == "Orphan" {
			t.Fatalf("a board whose creator grant failed must not remain: %+v", boards)
		}
	}
}

func TestPrincipalIsTheActorWhateverTheQuerySays(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "POST", "/api/cards/"+f.supportCardID+"/move?actor=principal_000003",
		model.MoveCardRequest{BoardID: f.supportBoardID, ColumnID: f.supportColumnID, Position: 2}), 200, "alice moves a card claiming to be carol")
	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	mustStatus(t, w, 200, "alice reads events")
	var events []model.CardEvent
	decodeSuccessfulResponse(t, w, &events)
	last := events[len(events)-1]
	if last.Kind != model.EventCardMoved || last.Actor != alice {
		t.Fatalf("the move should be logged as alice, got %+v", last)
	}
}

func TestGrantStoreDownRefusesRatherThanServing(t *testing.T) {
	store, err := db.New(filepath.Join(t.TempDir(), "down.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	a := api.New(store, noteboard.New(closedURL), principalstore.New(closedURL), llmbridge.New(closedURL), bundlestore.New(closedURL), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{ServiceToken: testServiceToken, Grants: grantstore.New(closedURL, "")})
	mustStatus(t, requestAs(t, a.Handler(), asPrincipal(alice), "GET", "/api/boards", nil), 502, "grant-store unreachable")
}

// TestAnAdministratorSeesEveryBoard pins the one fact that reaches boards
// nobody granted: principal-store's is_administrator on a human.
func TestAnAdministratorSeesEveryBoard(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	w := requestAs(t, h, asPrincipal(deploymentAdministrator), "GET", "/api/boards", nil)
	mustStatus(t, w, 200, "the administrator lists boards")
	if got := boardIDsIn(t, w); len(got) != 2 {
		t.Fatalf("the administrator should see both boards, got %v", got)
	}
	mustStatus(t, requestAs(t, h, asPrincipal(deploymentAdministrator), "GET", "/api/boards/"+f.financeBoardID, nil),
		200, "the administrator reads a board it holds no grant on")
	mustStatus(t, requestAs(t, h, asPrincipal(deploymentAdministrator), "POST", "/api/boards/"+f.financeBoardID+"/cards",
		model.CreateCardRequest{Title: "from the administrator", ColumnID: f.financeColumnID}), 201, "the administrator writes")
	mustStatus(t, requestAs(t, h, asPrincipal(deploymentAdministrator), "GET", "/api/tags", nil),
		200, "the administrator reads the cross-board tag listing")

	// The administrator holds no grant at all; nothing above came from one.
	grants.mu.Lock()
	held := len(grants.grantsByPrincipal[deploymentAdministrator])
	grants.mu.Unlock()
	if held != 0 {
		t.Fatalf("the administrator should hold no board grants, has %d", held)
	}
}

// TestTicketRoutesFollowTheSameBoardRules pins that a ticket is card content:
// reading one needs can_view on a board the card sits on, and writing one
// needs can_edit, exactly as every other card write does.
func TestTicketRoutesFollowTheSameBoardRules(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)

	// The service files a ticket on the Support card.
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+f.supportCardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "email"}), 200, "service writes a ticket")

	// alice holds can_edit on Support: she reads it and may change it.
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.supportCardID+"/ticket", nil),
		200, "alice reads a ticket on her board")
	mustStatus(t, requestAs(t, h, asPrincipal(alice), "PUT", "/api/cards/"+f.supportCardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "phone"}), 200, "alice rewrites it")

	// bob holds nothing on Support: the ticket is as invisible as the card.
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.supportCardID+"/ticket", nil),
		404, "bob reads a ticket on a board he cannot see")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "PUT", "/api/cards/"+f.supportCardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "chat"}), 404, "bob writes it")

	// A view-only principal on Finance may read that board's ticket and not write it.
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+f.financeCardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "portal"}), 200, "service writes a Finance ticket")
	grants.give(bob, "can_view", f.financeBoardID)
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.financeCardID+"/ticket", nil),
		200, "bob reads the Finance ticket he can view")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "DELETE", "/api/cards/"+f.financeCardID+"/ticket", nil),
		403, "a viewer deletes a ticket")

	// The vocabularies need no board at all.
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/ticket-channels", nil), 200, "channels")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/ticket-lifecycle-states", nil), 200, "lifecycle states")
}
