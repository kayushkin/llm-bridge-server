package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// ============================ Stub principal-store ============================

// fakePrincipalStore stands in for principal-store's GET /principals/{id}. It
// knows a handful of principals — active humans, one disabled, one
// administrator and one contact — and 404s on everything else. The kind
// matters to the ticket write path: a requester must be a contact.
type fakePrincipalStore struct {
	disabledAtByID map[string]int64
	requests       atomic.Int64
	// availableMembersByGroup is what GET /principals/{group}/members?available_at=
	// answers: the members principal-store would call available. Which hours
	// and time off made them so is principal-store's business, not this fake's.
	availableMembersByGroup map[string][]string
	// membersStatus, when set, is the status the members route answers with
	// instead, to stand in for a principal-store that cannot say.
	membersStatus int
}

const (
	activePrincipal      = "principal_000001"
	otherActivePrincipal = "principal_000003"
	disabledPrincipal    = "principal_000002"
	unknownPrincipal     = "principal_000099"
	// requesterContact is the outside person a ticket is from; disabledContact
	// is one principal-store has retired.
	requesterContact = "principal_000010"
	disabledContact  = "principal_000011"
	// organizationGroup and poolGroup are the groups the fake knows.
	organizationGroup = "principal_000023"
	poolGroup         = "principal_000030"
)

func newFakePrincipalStore() *fakePrincipalStore {
	return &fakePrincipalStore{disabledAtByID: map[string]int64{
		activePrincipal:         0,
		otherActivePrincipal:    0,
		disabledPrincipal:       1_757_000_000,
		"principal_000004":      0,
		"principal_000005":      0,
		deploymentAdministrator: 0,
		requesterContact:        0,
		disabledContact:         1_757_000_000,
		organizationGroup:       0,
		poolGroup:               0,
	}, availableMembersByGroup: map[string][]string{}}
}

// kindOf answers what fakePrincipalStore says a principal is.
func kindOf(id string) string {
	if id == requesterContact || id == disabledContact {
		return "contact"
	}
	if id == organizationGroup || id == poolGroup {
		return "group"
	}
	return "human"
}

func (f *fakePrincipalStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		id := r.PathValue("id")
		disabledAt, ok := f.disabledAtByID[id]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "principal not found"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "kind": kindOf(id), "display_name": "Test Person", "email": "test@example.com",
			"disabled_at": disabledAt, "is_administrator": id == deploymentAdministrator,
		})
	})
	mux.HandleFunc("GET /principals/{id}/members", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.membersStatus != 0 {
			writeJSON(w, f.membersStatus, map[string]string{"error": "principal-store is having a bad day"})
			return
		}
		if r.URL.Query().Get("available_at") == "" {
			writeJSON(w, 400, map[string]string{"error": "this fake answers only the available_at question"})
			return
		}
		id := r.PathValue("id")
		if _, ok := f.disabledAtByID[id]; !ok {
			writeJSON(w, 404, map[string]string{"error": "principal not found"})
			return
		}
		members := []map[string]any{}
		for _, member := range f.availableMembersByGroup[id] {
			members = append(members, map[string]any{"id": member, "kind": kindOf(member), "display_name": "Member", "disabled_at": 0})
		}
		writeJSON(w, 200, members)
	})
	return mux
}

// ============================ Helpers ============================

func assignmentTestCard(t *testing.T, h http.Handler) (boardID, columnID, cardID string) {
	t.Helper()
	boardID = mkBoard(t, h, "Board")
	columnID = mkColumn(t, h, boardID, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "assignable", ColumnID: columnID})
	if w.Code != 201 {
		t.Fatalf("create card: %d %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	return boardID, columnID, cv.Placement.CardID
}

func eventsOfKind(t *testing.T, h http.Handler, cardID string, kind model.EventKind) []model.CardEvent {
	t.Helper()
	w := do(t, h, "GET", "/api/cards/"+cardID+"/events", nil)
	if w.Code != 200 {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	var all []model.CardEvent
	decodeSuccessfulResponse(t, w, &all)
	var out []model.CardEvent
	for _, e := range all {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ============================ Tests ============================

func TestAssignIsIdempotent(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	path := "/api/cards/" + cardID + "/assignments/" + activePrincipal

	w := do(t, h, "PUT", path+"?actor=lead", nil)
	if w.Code != 201 {
		t.Fatalf("first PUT: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first model.CardAssignment
	decodeSuccessfulResponse(t, w, &first)
	if first.CardID != cardID || first.PrincipalID != activePrincipal || first.AssignedBy != "lead" || first.CreatedAt.IsZero() {
		t.Fatalf("unexpected assignment row: %+v", first)
	}

	w = do(t, h, "PUT", path+"?actor=someone-else", nil)
	if w.Code != 200 {
		t.Fatalf("repeat PUT: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var second model.CardAssignment
	decodeSuccessfulResponse(t, w, &second)
	if second.AssignedBy != "lead" || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("repeat PUT rewrote the row: first %+v, second %+v", first, second)
	}

	w = do(t, h, "GET", "/api/cards/"+cardID+"/assignments", nil)
	var list []model.CardAssignment
	decodeSuccessfulResponse(t, w, &list)
	if len(list) != 1 {
		t.Fatalf("expected exactly one assignment, got %d", len(list))
	}
	if got := eventsOfKind(t, h, cardID, model.EventAssigned); len(got) != 1 {
		t.Fatalf("expected one assigned event (only the 201 path logs), got %d", len(got))
	}
}

func TestUnassignIs204ThenA404(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	path := "/api/cards/" + cardID + "/assignments/" + activePrincipal
	if w := do(t, h, "PUT", path, nil); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	if w := do(t, h, "DELETE", path, nil); w.Code != 204 {
		t.Fatalf("DELETE: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "DELETE", path, nil); w.Code != 404 {
		t.Fatalf("second DELETE: expected 404, got %d: %s", w.Code, w.Body.String())
	}
	w := do(t, h, "GET", "/api/cards/"+cardID+"/assignments", nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected an empty array after unassign, got %s", w.Body.String())
	}
	if got := eventsOfKind(t, h, cardID, model.EventUnassigned); len(got) != 1 {
		t.Fatalf("expected one unassigned event, got %d", len(got))
	}
}

func TestAssignRejectsAMalformedPrincipalIDBeforeAskingPrincipalStore(t *testing.T) {
	principals := newFakePrincipalStore()
	srv := httptest.NewServer(principals.handler())
	defer srv.Close()
	h, _, cleanup := setupAgainstPrincipalStore(t, srv.URL)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)

	for _, bad := range []string{"bob", "principal_123", "prediction_000001", "PRINCIPAL_000001"} {
		w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+bad, nil)
		if w.Code != 400 {
			t.Fatalf("%q: expected 400, got %d: %s", bad, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `principal_\\d{6,}`) {
			t.Fatalf("%q: 400 body should name the required shape, got %s", bad, w.Body.String())
		}
	}
	if n := principals.requests.Load(); n != 0 {
		t.Fatalf("a malformed id must be refused locally; principal-store was asked %d times", n)
	}
}

func TestAssignAnUnknownPrincipalIs400(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+unknownPrincipal, nil)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	want := "principal_id " + unknownPrincipal + " does not exist in principal-store"
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("body %s does not contain %q", w.Body.String(), want)
	}
	if w := do(t, h, "GET", "/api/cards/"+cardID+"/assignments", nil); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("a refused assignment must not leave a row: %s", w.Body.String())
	}
}

func TestAssignADisabledPrincipalIs400(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+disabledPrincipal, nil)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "is disabled") {
		t.Fatalf("body should say the principal is disabled, got %s", w.Body.String())
	}
}

func TestAssignWithPrincipalStoreUnreachableIs502AndWritesNothing(t *testing.T) {
	// Port 1 is not listening, so connect() fails immediately with ECONNREFUSED.
	h, _, cleanup := setupAgainstPrincipalStore(t, "http://127.0.0.1:1")
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil)
	if w.Code != 502 {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "principal-store check failed") || !strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("502 body should carry the transport error verbatim, got %s", w.Body.String())
	}
	if w := do(t, h, "GET", "/api/cards/"+cardID+"/assignments", nil); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("a failed check must not write the row: %s", w.Body.String())
	}
	if got := eventsOfKind(t, h, cardID, model.EventAssigned); len(got) != 0 {
		t.Fatalf("a failed check must not log an assigned event, got %d", len(got))
	}
}

func TestAssignmentsAppearInBoardAndColumnViews(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID, columnID, cardID := assignmentTestCard(t, h)
	if w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	w := do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil)
	if w.Code != 200 {
		t.Fatalf("board view: %d %s", w.Code, w.Body.String())
	}
	var board model.BoardView
	decodeSuccessfulResponse(t, w, &board)
	if len(board.Columns) != 1 || len(board.Columns[0].Cards) != 1 {
		t.Fatalf("expected one card in one column, got %+v", board.Columns)
	}
	if got := board.Columns[0].Cards[0].Assignments; len(got) != 1 || got[0].PrincipalID != activePrincipal {
		t.Fatalf("board view assignments: got %+v", got)
	}

	w = do(t, h, "GET", "/api/columns/"+columnID+"/cards", nil)
	if w.Code != 200 {
		t.Fatalf("column cards: %d %s", w.Code, w.Body.String())
	}
	var column model.ColumnView
	decodeSuccessfulResponse(t, w, &column)
	if len(column.Cards) != 1 {
		t.Fatalf("expected one card, got %d", len(column.Cards))
	}
	if got := column.Cards[0].Assignments; len(got) != 1 || got[0].PrincipalID != activePrincipal {
		t.Fatalf("column view assignments: got %+v", got)
	}

	// The field is omitted, not null, on a card nobody is on.
	if w := do(t, h, "DELETE", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	w = do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil)
	var raw map[string]any
	decodeSuccessfulResponse(t, w, &raw)
	card := raw["columns"].([]any)[0].(map[string]any)["cards"].([]any)[0].(map[string]any)
	if _, present := card["assignments"]; present {
		t.Fatalf("assignments should be omitted when empty, got %v", card["assignments"])
	}
}

func TestAssignmentsReverseLookupByPrincipal(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID, columnID, firstCard := assignmentTestCard(t, h)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "second", ColumnID: columnID})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	secondCard := cv.Placement.CardID

	for _, cardID := range []string{firstCard, secondCard} {
		if w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", cardID, w.Code, w.Body.String())
		}
	}

	if w := do(t, h, "GET", "/api/assignments", nil); w.Code != 400 {
		t.Fatalf("no principal_id: expected 400, got %d", w.Code)
	}
	w = do(t, h, "GET", "/api/assignments?principal_id="+activePrincipal, nil)
	if w.Code != 200 {
		t.Fatalf("reverse lookup: %d %s", w.Code, w.Body.String())
	}
	var list []model.CardAssignment
	decodeSuccessfulResponse(t, w, &list)
	if len(list) != 2 || list[0].CardID != firstCard || list[1].CardID != secondCard {
		t.Fatalf("expected [%s %s] oldest first, got %+v", firstCard, secondCard, list)
	}
	w = do(t, h, "GET", "/api/assignments?principal_id="+unknownPrincipal, nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("unknown principal: expected 200 [], got %d %s", w.Code, w.Body.String())
	}
}

// Assignment is a fact about who is on the card, not about whether the work is
// runnable, so the events it records carry the clock forward unchanged: a card
// whose clock is paused stays paused through an assign and an unassign, and a
// finished card stays finished.
func TestAssignmentEventsCarryTheClockForwardUnchanged(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	blocked := mkColumnWithClock(t, h, boardID, "Blocked", model.ClockPaused)
	done := mkColumnWithClock(t, h, boardID, "Done", model.ClockStopped)

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "waiting", ColumnID: blocked})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID

	if w := do(t, h, "PUT", "/api/cards/"+cardID+"/assignments/"+activePrincipal+"?actor=lead", nil); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	assigned := eventsOfKind(t, h, cardID, model.EventAssigned)
	if len(assigned) != 1 {
		t.Fatalf("expected one assigned event, got %d", len(assigned))
	}
	e := assigned[0]
	if e.ClockState != model.ClockPaused {
		t.Fatalf("assigned on a paused card must stay paused, got %s", e.ClockState)
	}
	if e.Summary != activePrincipal || e.Actor != "lead" || e.BoardID != "" {
		t.Fatalf("unexpected assigned event: %+v", e)
	}
	var detail map[string]string
	if err := json.Unmarshal(e.Detail, &detail); err != nil || detail["principal_id"] != activePrincipal {
		t.Fatalf("detail should be {\"principal_id\":%q}, got %s (%v)", activePrincipal, string(e.Detail), err)
	}

	// The card's standing does not change: still paused, no budget consumed.
	w = do(t, h, "GET", "/api/cards/"+cardID+"/timeline?board_id="+boardID, nil)
	var tl struct {
		Summary model.CardTimeSummary `json:"summary"`
	}
	decodeSuccessfulResponse(t, w, &tl)
	if tl.Summary.ClockState != model.ClockPaused || tl.Summary.BudgetClockSeconds != 0 {
		t.Fatalf("assignment moved the clock: %+v", tl.Summary)
	}

	// Finish the card, then unassign: the unassigned event is stopped, so the
	// card does not come back to life.
	if w := do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{BoardID: boardID, ColumnID: done}); w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "DELETE", "/api/cards/"+cardID+"/assignments/"+activePrincipal, nil); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	unassigned := eventsOfKind(t, h, cardID, model.EventUnassigned)
	if len(unassigned) != 1 || unassigned[0].ClockState != model.ClockStopped {
		t.Fatalf("unassigned on a finished card must stay stopped, got %+v", unassigned)
	}
	w = do(t, h, "GET", "/api/cards/"+cardID+"/timeline?board_id="+boardID, nil)
	decodeSuccessfulResponse(t, w, &tl)
	if tl.Summary.ClockState != model.ClockStopped {
		t.Fatalf("unassign restarted a finished card: %+v", tl.Summary)
	}
}

// Most cards on the live host predate the event log: a placement and nothing
// on the timeline. For those the column the card sits in is the only statement
// of what state it is in, and an assignment takes that rather than starting a
// clock — 6,928 of them sit in a stopped column, and "running" would have
// brought every one back to life the moment someone was assigned.
func TestAssigningACardWithNoHistoryTakesItsColumnsClock(t *testing.T) {
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	defer principals.Close()
	h, nb, store, cleanup := setupExposingStore(t, principals.URL)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	done := mkColumnWithClock(t, h, boardID, "Done", model.ClockStopped)
	unclassified := mkColumn(t, h, boardID, "Inbox", "")

	// Straight into the store: a placement with no event, as the legacy rows are.
	legacyDone := nb.seedItem("filed long ago")
	if _, err := store.AttachCard(boardID, legacyDone, &model.AttachCardRequest{ColumnID: done}); err != nil {
		t.Fatal(err)
	}
	legacyInbox := nb.seedItem("also filed long ago")
	if _, err := store.AttachCard(boardID, legacyInbox, &model.AttachCardRequest{ColumnID: unclassified}); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, "PUT", "/api/cards/"+legacyDone+"/assignments/"+activePrincipal, nil); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	assigned := eventsOfKind(t, h, legacyDone, model.EventAssigned)
	if len(assigned) != 1 || assigned[0].ClockState != model.ClockStopped {
		t.Fatalf("a no-history card in a stopped column must be assigned stopped, got %+v", assigned)
	}
	w := do(t, h, "GET", "/api/cards/"+legacyDone+"/timeline?board_id="+boardID, nil)
	var tl struct {
		Summary model.CardTimeSummary `json:"summary"`
	}
	decodeSuccessfulResponse(t, w, &tl)
	if tl.Summary.ClockState != model.ClockStopped || tl.Summary.BudgetClockSeconds != 0 || tl.Summary.ElapsedSeconds != 0 {
		t.Fatalf("assigning a finished legacy card started its clock: %+v", tl.Summary)
	}

	// No history and an unclassified column: the store's answer for any first
	// action on such a card, which is running.
	if w := do(t, h, "PUT", "/api/cards/"+legacyInbox+"/assignments/"+activePrincipal, nil); w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	assigned = eventsOfKind(t, h, legacyInbox, model.EventAssigned)
	if len(assigned) != 1 || assigned[0].ClockState != model.ClockRunning {
		t.Fatalf("a no-history card in an unclassified column runs, got %+v", assigned)
	}
}

// Posting one of the two kinds by hand, without a clock state, is refused the
// same way card_moved is: the kind has no default because the state is the
// card's, not the kind's.
func TestAssignedKindHasNoDefaultClockState(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, cardID := assignmentTestCard(t, h)
	for _, kind := range []model.EventKind{model.EventAssigned, model.EventUnassigned} {
		w := do(t, h, "POST", "/api/cards/"+cardID+"/events", model.CreateCardEventRequest{Kind: kind})
		if w.Code != 400 {
			t.Fatalf("%s without clock_state: expected 400, got %d: %s", kind, w.Code, w.Body.String())
		}
	}
}

func TestEntityTypesAreReferenceStoresListUnchanged(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	w := do(t, h, "GET", "/api/entity-types", nil)
	var types []map[string]any
	decodeSuccessfulResponse(t, w, &types)
	if len(types) != 3 {
		t.Fatalf("want the fake reference-store's 3 rows, got %d: %s", len(types), w.Body.String())
	}
	principal := types[1]
	if principal["type"] != "principal" || principal["label_field"] != "display_name" || principal["access"] != "grant" {
		t.Fatalf("a row lost a field on the way through: %+v", principal)
	}
}
