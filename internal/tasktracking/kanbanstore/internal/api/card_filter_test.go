package api_test

import (
	"sort"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

type filteredDesk struct {
	boardID, columnID                          string
	aliceTicketByEmail, unassignedTicketByChat string
	carolOrdinaryCard                          string
}

// One column, three cards: alice's email ticket, a chat ticket nobody has
// picked up, and an ordinary card of carol's that is no ticket at all.
func buildFilteredDesk(t *testing.T) (filteredDesk, func(query string) model.BoardView, func(query string) model.ColumnView) {
	t.Helper()
	h, _, _ := setupWithPrincipalEnforcement(t)
	var d filteredDesk
	w := requestAs(t, h, asService, "POST", "/api/boards", model.CreateBoardRequest{Name: "Support Desk"})
	mustStatus(t, w, 201, "board")
	var board model.Board
	decodeSuccessfulResponse(t, w, &board)
	d.boardID = board.ID
	w = requestAs(t, h, asService, "POST", "/api/boards/"+d.boardID+"/columns", model.CreateColumnRequest{Name: "New"})
	mustStatus(t, w, 201, "column")
	var column model.Column
	decodeSuccessfulResponse(t, w, &column)
	d.columnID = column.ID

	makeCard := func(title string) string {
		w := requestAs(t, h, asService, "POST", "/api/boards/"+d.boardID+"/cards", model.CreateCardRequest{Title: title, ColumnID: d.columnID})
		mustStatus(t, w, 201, "card "+title)
		var card model.CardView
		decodeSuccessfulResponse(t, w, &card)
		return card.Placement.CardID
	}
	d.aliceTicketByEmail = makeCard("alice's email ticket")
	d.unassignedTicketByChat = makeCard("a chat ticket nobody has picked up")
	d.carolOrdinaryCard = makeCard("carol's ordinary card")
	for cardID, channel := range map[string]string{d.aliceTicketByEmail: "email", d.unassignedTicketByChat: "chat"} {
		mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+cardID+"/ticket",
			model.TicketWriteRequest{RequesterPrincipalID: requesterContact, Channel: channel}), 200, "ticket by "+channel)
	}
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+d.aliceTicketByEmail+"/assignments/"+alice, nil), 201, "assign alice")
	mustStatus(t, requestAs(t, h, asService, "PUT", "/api/cards/"+d.carolOrdinaryCard+"/assignments/"+carol, nil), 201, "assign carol")

	board_ := func(query string) model.BoardView {
		w := requestAs(t, h, asService, "GET", "/api/boards/"+d.boardID+"/cards"+query, nil)
		mustStatus(t, w, 200, "board read "+query)
		var view model.BoardView
		decodeSuccessfulResponse(t, w, &view)
		return view
	}
	column_ := func(query string) model.ColumnView {
		w := requestAs(t, h, asService, "GET", "/api/columns/"+d.columnID+"/cards"+query, nil)
		mustStatus(t, w, 200, "column read "+query)
		var view model.ColumnView
		decodeSuccessfulResponse(t, w, &view)
		return view
	}
	return d, board_, column_
}

func cardIDsOf(cards []model.CardView) []string {
	ids := []string{}
	for _, card := range cards {
		ids = append(ids, card.Placement.CardID)
	}
	sort.Strings(ids)
	return ids
}

func sameCards(got []string, want ...string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestABoardReadKeepsOnlyTheCardsItsFilterNames(t *testing.T) {
	d, board, column := buildFilteredDesk(t)
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{d.aliceTicketByEmail, d.unassignedTicketByChat, d.carolOrdinaryCard}},
		{"?assignee=" + alice, []string{d.aliceTicketByEmail}},
		{"?unassigned=true", []string{d.unassignedTicketByChat}},
		{"?tickets_only=true", []string{d.aliceTicketByEmail, d.unassignedTicketByChat}},
		{"?channel=chat", []string{d.unassignedTicketByChat}},
		{"?requester=" + requesterContact, []string{d.aliceTicketByEmail, d.unassignedTicketByChat}},
		{"?requester=" + requesterContact + "&assignee=" + alice, []string{d.aliceTicketByEmail}},
		{"?assignee=" + bob, []string{}},
	}
	for _, c := range cases {
		view := board(c.query)
		if got := cardIDsOf(view.Columns[0].Cards); !sameCards(got, c.want...) {
			t.Errorf("board%s: cards %v, want %v", c.query, got, c.want)
		}
		// The total is what makes "showing N of M" true, so it counts what
		// the filter keeps and not the whole column.
		if view.Columns[0].Total != len(c.want) {
			t.Errorf("board%s: total %d, want %d", c.query, view.Columns[0].Total, len(c.want))
		}
		page := column(c.query)
		if got := cardIDsOf(page.Cards); !sameCards(got, c.want...) || page.Total != len(c.want) {
			t.Errorf("column%s: cards %v total %d, want %v", c.query, got, page.Total, c.want)
		}
	}
}

// Paging happens after the filter: a page of one must be the first match, not
// the first card of the column with the filter applied to it afterwards.
func TestAFilteredColumnIsPagedAfterTheFilter(t *testing.T) {
	d, _, column := buildFilteredDesk(t)
	page := column("?tickets_only=true&channel=chat&limit=1")
	if got := cardIDsOf(page.Cards); !sameCards(got, d.unassignedTicketByChat) || page.Total != 1 {
		t.Fatalf("cards %v total %d, want the chat ticket, which is second in the column", got, page.Total)
	}
	// A page of a column carries the ticket, as the board view does.
	if page.Cards[0].Ticket == nil || page.Cards[0].Ticket.Channel != "chat" {
		t.Errorf("the column page carried ticket %#v, want the chat ticket's row", page.Cards[0].Ticket)
	}
}

// A filter that was dropped answers every card, which reads as "all of these
// match". So a value that cannot mean anything is refused.
func TestAFilterThatCannotMeanAnythingIsRefused(t *testing.T) {
	h, _, _ := setupWithPrincipalEnforcement(t)
	w := requestAs(t, h, asService, "POST", "/api/boards", model.CreateBoardRequest{Name: "Desk"})
	mustStatus(t, w, 201, "board")
	var board model.Board
	decodeSuccessfulResponse(t, w, &board)
	for _, query := range []string{
		"?assignee=alice", "?requester=someone@example.com", "?channel=carrier-pigeon",
		"?unassigned=yes", "?tickets_only=1", "?unassigned=true&assignee=" + alice,
	} {
		mustStatus(t, requestAs(t, h, asService, "GET", "/api/boards/"+board.ID+"/cards"+query, nil), 400, "board read "+query)
	}
}
