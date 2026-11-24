package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
)

const sourceEmailRef = "gmail-work:18f2a0c4b1d9e7aa"

func putTicketFrom(t *testing.T, h http.Handler, cardID, sourceType, sourceRef string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "PUT", "/api/cards/"+cardID+"/ticket", model.TicketWriteRequest{
		RequesterPrincipalID: requesterContact, Channel: "email",
		SourceEntityType: sourceType, SourceEntityRef: sourceRef,
	})
}

// TestATicketKeepsTheEmailItCameFrom: the source is written once. A later
// write without one keeps it, the same one again is fine, and a different
// one is refused rather than quietly replacing the record.
func TestATicketKeepsTheEmailItCameFrom(t *testing.T) {
	f := newTicketFixture(t)

	w := putTicketFrom(t, f.handler, f.cardID, "email", sourceEmailRef)
	if w.Code != 200 {
		t.Fatalf("put ticket with source: %d %s", w.Code, w.Body.String())
	}
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if view.Ticket.SourceEntityType != "email" || view.Ticket.SourceEntityRef != sourceEmailRef {
		t.Fatalf("source not stored: %+v", view.Ticket)
	}

	w = putTicket(t, f.handler, f.cardID, requesterContact, "email")
	if w.Code != 200 {
		t.Fatalf("rewrite without source: %d %s", w.Code, w.Body.String())
	}
	decodeSuccessfulResponse(t, w, &view)
	if view.Ticket.SourceEntityRef != sourceEmailRef {
		t.Fatalf("a write without a source cleared it: %+v", view.Ticket)
	}

	if w = putTicketFrom(t, f.handler, f.cardID, "email", sourceEmailRef); w.Code != 200 {
		t.Fatalf("the same source again: %d %s", w.Code, w.Body.String())
	}

	if w = putTicketFrom(t, f.handler, f.cardID, "email", "gmail-work:another"); w.Code != 409 {
		t.Fatalf("a different source: want 409, got %d %s", w.Code, w.Body.String())
	}
	w = do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil)
	decodeSuccessfulResponse(t, w, &view)
	if view.Ticket.SourceEntityRef != sourceEmailRef {
		t.Fatalf("the refused write changed the source: %+v", view.Ticket)
	}
}

// TestATicketWithNoSourceTakesOneLater is the backfill path: a ticket written
// before the field existed gets its source once.
func TestATicketWithNoSourceTakesOneLater(t *testing.T) {
	f := newTicketFixture(t)
	if w := putTicket(t, f.handler, f.cardID, requesterContact, "email"); w.Code != 200 {
		t.Fatalf("put ticket: %d %s", w.Code, w.Body.String())
	}
	w := putTicketFrom(t, f.handler, f.cardID, "email", sourceEmailRef)
	if w.Code != 200 {
		t.Fatalf("add source: %d %s", w.Code, w.Body.String())
	}
	var view model.TicketView
	decodeSuccessfulResponse(t, w, &view)
	if view.Ticket.SourceEntityRef != sourceEmailRef {
		t.Fatalf("source not added: %+v", view.Ticket)
	}
}

func TestATicketSourceIsAKnownTypeAndARef(t *testing.T) {
	f := newTicketFixture(t)
	cases := []struct{ name, sourceType, sourceRef string }{
		{"type without ref", "email", ""},
		{"ref without type", "", sourceEmailRef},
		{"unregistered type", "fax", "0123"},
	}
	for _, c := range cases {
		if w := putTicketFrom(t, f.handler, f.cardID, c.sourceType, c.sourceRef); w.Code != 400 {
			t.Errorf("%s: want 400, got %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if w := do(t, f.handler, "GET", "/api/cards/"+f.cardID+"/ticket", nil); w.Code != 404 {
		t.Fatalf("a refused write made a ticket: %d %s", w.Code, w.Body.String())
	}
}

func listTicketsPage(t *testing.T, h http.Handler, query url.Values) model.TicketList {
	t.Helper()
	w := do(t, h, "GET", "/api/tickets?"+query.Encode(), nil)
	if w.Code != 200 {
		t.Fatalf("list tickets %s: %d %s", query.Encode(), w.Code, w.Body.String())
	}
	var page model.TicketList
	decodeSuccessfulResponse(t, w, &page)
	return page
}

// TestTicketsListNewestFirstAndPage walks three email tickets two at a time
// and checks the chat ticket stays out of a channel=email read.
func TestTicketsListNewestFirstAndPage(t *testing.T) {
	f := newTicketFixture(t)
	cardIDs := []string{f.cardID}
	for _, title := range []string{"second", "third", "chat one"} {
		w := do(t, f.handler, "POST", "/api/boards/"+f.boardID+"/cards",
			model.CreateCardRequest{Title: title, ColumnID: f.newColumnID})
		var card model.CardView
		decodeSuccessfulResponse(t, w, &card)
		cardIDs = append(cardIDs, card.Placement.CardID)
	}
	for index, cardID := range cardIDs[:3] {
		if w := putTicketFrom(t, f.handler, cardID, "email", sourceEmailRef+string(rune('a'+index))); w.Code != 200 {
			t.Fatalf("put ticket: %d %s", w.Code, w.Body.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if w := putTicket(t, f.handler, cardIDs[3], requesterContact, "chat"); w.Code != 200 {
		t.Fatalf("put chat ticket: %d %s", w.Code, w.Body.String())
	}

	first := listTicketsPage(t, f.handler, url.Values{"channel": {"email"}, "limit": {"2"}})
	if len(first.Tickets) != 2 || first.Tickets[0].Ticket.CardID != cardIDs[2] || first.Tickets[1].Ticket.CardID != cardIDs[1] {
		t.Fatalf("first page should be the two newest email tickets: %+v", first.Tickets)
	}
	if first.NextBefore == nil {
		t.Fatal("first page should point at a next page")
	}
	entry := first.Tickets[0]
	if entry.CardCreatedAt == nil || entry.Ticket.SourceEntityType != "email" || len(entry.States) != 1 || entry.RequesterDisplayName == "" {
		t.Fatalf("an entry should carry its card's creation, source, state and requester name: %+v", entry)
	}

	second := listTicketsPage(t, f.handler, url.Values{"channel": {"email"}, "limit": {"2"},
		"before": {first.NextBefore.Format(time.RFC3339Nano)}})
	if len(second.Tickets) != 1 || second.Tickets[0].Ticket.CardID != cardIDs[0] || second.NextBefore != nil {
		t.Fatalf("second page should be the oldest email ticket and the end: %+v", second)
	}

	all := listTicketsPage(t, f.handler, url.Values{})
	if len(all.Tickets) != 4 {
		t.Fatalf("no channel filter should list every ticket, got %d", len(all.Tickets))
	}
	if w := do(t, f.handler, "GET", "/api/tickets?channel=fax", nil); w.Code != 400 {
		t.Fatalf("unknown channel: want 400, got %d", w.Code)
	}
}

// TestTheTicketListHidesBoardsTheCallerCannotView: bob views Finance only,
// so he sees the Finance ticket and the one on both boards, and the shared
// card's lifecycle names Finance alone.
func TestTheTicketListHidesBoardsTheCallerCannotView(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	for _, cardID := range []string{f.supportCardID, f.financeCardID, f.sharedCardID} {
		mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+cardID+"/ticket",
			model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: "email"}), 200, "service writes ticket")
	}

	w := requestAs(t, h, asPrincipal(bob), "GET", "/api/tickets?limit=1", nil)
	mustStatus(t, w, 200, "bob lists tickets")
	var page model.TicketList
	decodeSuccessfulResponse(t, w, &page)
	seen := map[string]model.TicketLogEntry{}
	for _, entry := range page.Tickets {
		seen[entry.Ticket.CardID] = entry
	}
	for page.NextBefore != nil {
		w = requestAs(t, h, asPrincipal(bob), "GET", "/api/tickets?limit=1&before="+url.QueryEscape(page.NextBefore.Format(time.RFC3339Nano)), nil)
		mustStatus(t, w, 200, "bob lists the next page")
		page = model.TicketList{}
		decodeSuccessfulResponse(t, w, &page)
		for _, entry := range page.Tickets {
			seen[entry.Ticket.CardID] = entry
		}
	}
	if len(seen) != 2 {
		t.Fatalf("bob should see two tickets, saw %d", len(seen))
	}
	if _, found := seen[f.supportCardID]; found {
		t.Fatal("bob saw a ticket on Support, a board he cannot view")
	}
	shared := seen[f.sharedCardID]
	if len(shared.States) != 1 || shared.States[0].BoardID != f.financeBoardID {
		t.Fatalf("the shared ticket should name Finance only: %+v", shared.States)
	}
}
