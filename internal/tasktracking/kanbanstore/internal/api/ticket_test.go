package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/model"
)

// ticketFixture is a board whose columns are classified for tickets, with one
// card sitting in the first of them.
type ticketFixture struct {
	handler                   http.Handler
	boardID                   string
	newColumnID, openColumnID string
	resolvedColumnID, cardID  string
}

func newTicketFixture(t *testing.T) ticketFixture {
	t.Helper()
	h, _, _ := setup(t)
	boardID := mkBoard(t, h, "Support Desk")
	fixture := ticketFixture{handler: h, boardID: boardID}
	fixture.newColumnID = mkLifecycleColumn(t, h, boardID, "Inbox", model.TicketLifecycleNew)
	fixture.openColumnID = mkLifecycleColumn(t, h, boardID, "Working", model.TicketLifecycleOpen)
	fixture.resolvedColumnID = mkLifecycleColumn(t, h, boardID, "Resolved", model.TicketLifecycleResolved)

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards",
		model.CreateCardRequest{Title: "printer on fire", ColumnID: fixture.newColumnID})
	if w.Code != 201 {
		t.Fatalf("create card: %d %s", w.Code, w.Body.String())
	}
	var card model.CardView
	decodeSuccessfulResponse(t, w, &card)
	fixture.cardID = card.Placement.CardID
	return fixture
}

func mkLifecycleColumn(t *testing.T, h http.Handler, boardID, name string, state model.TicketLifecycleState) string {
	t.Helper()
	lifecycle := string(state)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns",
		model.CreateColumnRequest{Name: name, LifecycleState: &lifecycle})
	if w.Code != 201 {
		t.Fatalf("create column %s: %d %s", name, w.Code, w.Body.String())
	}
	var column model.Column
	decodeSuccessfulResponse(t, w, &column)
	if column.LifecycleState == nil || *column.LifecycleState != state {
		t.Fatalf("column %s did not keep its lifecycle state: %+v", name, column.LifecycleState)
	}
	return column.ID
}

func putTicket(t *testing.T, h http.Handler, cardID, requester, channel string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "PUT", "/api/cards/"+cardID+"/ticket",
		model.TicketWriteRequest{RequesterPrincipalID: requester, Channel: channel})
}

// TestATicketReadsItsLifecycleFromItsColumn is the decision the model rests
// on: the ticket stores no status, so moving the card is what changes it.
func TestATicketReadsItsLifecycleFromItsColumn(t *testing.T) {
	f := newTicketFixture(t)

	w := putTicket(t, f.handler, f.cardID, requesterContact, "email")
	if w.Code != 200 {
		t.Fatalf("put ticket: %d %s", w.Code, w.Body.String())
	}
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if view.Ticket.RequesterPrincipalID != requesterContact || view.Ticket.Channel != model.TicketChannelEmail {
		t.Fatalf("stored ticket: %+v", view.Ticket)
	}
	if view.RequesterDisplayName == "" {
		t.Fatal("the requester's name should come back for display")
	}
	if len(view.States) != 1 || view.States[0].LifecycleState == nil || *view.States[0].LifecycleState != model.TicketLifecycleNew {
		t.Fatalf("a card in the Inbox column is new: %+v", view.States)
	}

	// Move the card; the state follows, with nothing written to the ticket.
	w = do(t, f.handler, "POST", "/api/cards/"+f.cardID+"/move",
		model.MoveCardRequest{BoardID: f.boardID, ColumnID: f.resolvedColumnID, Position: 1})
	if w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	w = do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil)
	decodeSuccessfulResponse(t, w, &view)
	if *view.States[0].LifecycleState != model.TicketLifecycleResolved {
		t.Fatalf("after the move the ticket is resolved, got %+v", view.States[0])
	}
	if view.States[0].ColumnName != "Resolved" {
		t.Fatalf("the state names the column it came from, got %q", view.States[0].ColumnName)
	}
}

// TestAnUnclassifiedColumnReportsNoLifecycle pins the honest answer: a board
// nobody classified does not make every ticket on it "new".
func TestAnUnclassifiedColumnReportsNoLifecycle(t *testing.T) {
	f := newTicketFixture(t)
	plainColumn := mkColumn(t, f.handler, f.boardID, "Unclassified", "")
	if w := putTicket(t, f.handler, f.cardID, requesterContact, "portal"); w.Code != 200 {
		t.Fatalf("put ticket: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, f.handler, "POST", "/api/cards/"+f.cardID+"/move",
		model.MoveCardRequest{BoardID: f.boardID, ColumnID: plainColumn, Position: 1}); w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil)
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if len(view.States) != 1 || view.States[0].LifecycleState != nil {
		t.Fatalf("an unclassified column reports no state, got %+v", view.States)
	}
}

// TestACardOnTwoBoardsAnswersBothStates pins why States is a list: the mail
// board and the team board can disagree, and this store does not pick.
func TestACardOnTwoBoardsAnswersBothStates(t *testing.T) {
	f := newTicketFixture(t)
	if w := putTicket(t, f.handler, f.cardID, requesterContact, "email"); w.Code != 200 {
		t.Fatalf("put ticket: %d %s", w.Code, w.Body.String())
	}
	secondBoard := mkBoard(t, f.handler, "Team board")
	secondColumn := mkLifecycleColumn(t, f.handler, secondBoard, "In progress", model.TicketLifecycleOpen)
	if w := do(t, f.handler, "PUT", "/api/boards/"+secondBoard+"/cards/"+f.cardID,
		model.AttachCardRequest{ColumnID: secondColumn}); w.Code != 201 {
		t.Fatalf("attach to the second board: %d %s", w.Code, w.Body.String())
	}

	w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil)
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if len(view.States) != 2 {
		t.Fatalf("a card on two boards answers two states, got %+v", view.States)
	}
	byBoard := map[string]model.TicketLifecycleState{}
	for _, state := range view.States {
		if state.LifecycleState != nil {
			byBoard[state.BoardID] = *state.LifecycleState
		}
	}
	if byBoard[f.boardID] != model.TicketLifecycleNew || byBoard[secondBoard] != model.TicketLifecycleOpen {
		t.Fatalf("each board reports its own column's state: %+v", byBoard)
	}
}

// TestTheRequesterMustBeAContact pins the boundary: a colleague recorded as
// the person who asked would answer every later "who reported this?" wrongly.
func TestTheRequesterMustBeAContact(t *testing.T) {
	f := newTicketFixture(t)

	w := putTicket(t, f.handler, f.cardID, activePrincipal, "email")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "contacts/resolve") {
		t.Fatalf("a human requester should be refused with the way to get a contact: %d %s", w.Code, w.Body.String())
	}
	if w := putTicket(t, f.handler, f.cardID, unknownPrincipal, "email"); w.Code != 400 {
		t.Fatalf("an unknown principal: want 400, got %d %s", w.Code, w.Body.String())
	}
	if w := putTicket(t, f.handler, f.cardID, disabledContact, "email"); w.Code != 400 {
		t.Fatalf("a disabled contact: want 400, got %d %s", w.Code, w.Body.String())
	}
	if w := putTicket(t, f.handler, f.cardID, "not-a-principal-id", "email"); w.Code != 400 {
		t.Fatalf("a malformed id: want 400, got %d %s", w.Code, w.Body.String())
	}
	// Nothing was written by any of those.
	if w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil); w.Code != 404 {
		t.Fatalf("no ticket should exist yet: %d %s", w.Code, w.Body.String())
	}
}

// TestAnUnknownChannelNamesTheVocabulary — a caller who guessed wrong cannot
// guess right from a bare rejection.
func TestAnUnknownChannelNamesTheVocabulary(t *testing.T) {
	f := newTicketFixture(t)
	w := putTicket(t, f.handler, f.cardID, requesterContact, "carrier-pigeon")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
	for _, channel := range []string{"email", "portal", "chat", "phone", "agent"} {
		if !strings.Contains(w.Body.String(), channel) {
			t.Fatalf("the refusal should name %q: %s", channel, w.Body.String())
		}
	}
	// An unknown field is refused too, rather than dropped.
	raw := httptest.NewRequest("PUT", "/api/cards/"+f.cardID+"/ticket",
		strings.NewReader(`{"requester_principal_id":"`+requesterContact+`","channel":"email","status":"open"}`))
	raw.Header.Set(api.ServiceTokenHeader, testServiceToken)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, raw)
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "status") {
		t.Fatalf("an unknown field should be refused naming it: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestDeletingATicketLeavesTheCard — "this is not a ticket" is not "this is
// not work".
func TestDeletingATicketLeavesTheCard(t *testing.T) {
	f := newTicketFixture(t)
	if w := putTicket(t, f.handler, f.cardID, requesterContact, "phone"); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, f.handler, "DELETE", "/api/cards/"+f.cardID+"/ticket", nil); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil); w.Code != 404 {
		t.Fatalf("the ticket is gone: %d", w.Code)
	}
	if w := do(t, f.handler, "DELETE", "/api/cards/"+f.cardID+"/ticket", nil); w.Code != 404 {
		t.Fatalf("deleting a ticket that is not there is a 404, got %d", w.Code)
	}
	// The card is still on its board.
	w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/placements", nil)
	var placements []model.Placement
	decodeSuccessfulResponse(t, w, &placements)
	if len(placements) != 1 {
		t.Fatalf("the card should still be placed, got %+v", placements)
	}
}

// TestTheBoardViewCarriesTickets — a board of 6,000 cards must not become
// 6,000 reads, so the view carries the ticket it already has.
func TestTheBoardViewCarriesTickets(t *testing.T) {
	f := newTicketFixture(t)
	if w := putTicket(t, f.handler, f.cardID, requesterContact, "chat"); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	w := do(t, f.handler, "GET", "/api/boards/"+f.boardID+"/cards", nil)
	var view model.BoardView
	decodeSuccessfulResponse(t, w, &view)
	found := false
	for _, column := range view.Columns {
		for _, card := range column.Cards {
			if card.Placement.CardID != f.cardID {
				continue
			}
			found = true
			if card.Ticket == nil || card.Ticket.Channel != model.TicketChannelChat {
				t.Fatalf("the board view should carry the ticket: %+v", card.Ticket)
			}
		}
	}
	if !found {
		t.Fatal("the card was not in the board view")
	}
}

// TestTicketVocabulariesAreServed — no caller builds a picker out of whichever
// values happen to be in the rows.
func TestTicketVocabulariesAreServed(t *testing.T) {
	h, _, _ := setup(t)
	w := do(t, h, "GET", "/api/ticket-channels", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"email"`) {
		t.Fatalf("channels: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/ticket-lifecycle-states", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"waiting_on_requester"`) {
		t.Fatalf("lifecycle states: %d %s", w.Code, w.Body.String())
	}
}
