package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
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

type contentDesk struct {
	h        http.Handler
	notes    *fakeNoteboard
	boardID  string
	columnID string
	titleOf  map[string]string
}

// Five cards in one column, in this stored order:
//
//	refund (billing, P1) · chargeback (billing+urgent, P3) · printer (hardware, P3)
//	· invoice (billing, P3, alice's) · laptop (hardware, P2)
func buildContentDesk(t *testing.T) *contentDesk {
	t.Helper()
	notes := newFakeNoteboard()
	urls := map[string]string{}
	grants := &fakeGrantStore{grantsByPrincipal: map[string][]grantstore.BoardGrant{}}
	for name, handler := range map[string]http.Handler{
		"notes": notes.handler(), "grants": grants.handler(), "principals": newFakePrincipalStore().handler(),
		"bridge": newFakeLLMBridgeServer().handler(), "bundles": newFakeBundleStore().handler(),
	} {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[name] = server.URL
	}
	store, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := api.New(store, noteboard.New(urls["notes"]), principalstore.New(urls["principals"]), llmbridge.New(urls["bridge"]), bundlestore.New(urls["bundles"]), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	// Cleanups run last-registered first, so this waits for trigger
	// dispatches before the store above is closed.
	t.Cleanup(a.WaitForMessageTriggers)
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{ServiceToken: testServiceToken, Grants: grantstore.New(urls["grants"], "grant-store-token")})
	d := &contentDesk{h: a.Handler(), notes: notes, titleOf: map[string]string{}}

	w := requestAs(t, d.h, asService, "POST", "/api/boards", model.CreateBoardRequest{Name: "Support Desk"})
	mustStatus(t, w, 201, "board")
	var board model.Board
	decodeSuccessfulResponse(t, w, &board)
	d.boardID = board.ID
	w = requestAs(t, d.h, asService, "POST", "/api/boards/"+d.boardID+"/columns", model.CreateColumnRequest{Name: "Open"})
	mustStatus(t, w, 201, "column")
	var column model.Column
	decodeSuccessfulResponse(t, w, &column)
	d.columnID = column.ID
	for _, card := range []struct {
		title    string
		tags     []string
		priority int
	}{
		{"refund", []string{"billing"}, 1}, {"chargeback", []string{"billing", "urgent"}, 3}, {"printer", []string{"hardware"}, 3},
		{"invoice", []string{"billing"}, 3}, {"laptop", []string{"hardware"}, 2},
	} {
		priority := card.priority
		w := requestAs(t, d.h, asService, "POST", "/api/boards/"+d.boardID+"/cards", model.CreateCardRequest{Title: card.title, Tags: card.tags, Priority: &priority, ColumnID: d.columnID})
		mustStatus(t, w, 201, "card "+card.title)
		var view model.CardView
		decodeSuccessfulResponse(t, w, &view)
		d.titleOf[view.Placement.CardID] = card.title
		if card.title == "invoice" {
			mustStatus(t, requestAs(t, d.h, asService, "PUT", "/api/cards/"+view.Placement.CardID+"/assignments/"+alice, nil), 201, "assign alice")
		}
	}
	return d
}

func (d *contentDesk) titles(cards []model.CardView) []string {
	titles := []string{}
	for _, card := range cards {
		titles = append(titles, d.titleOf[card.Placement.CardID])
	}
	return titles
}

// A desk asks for the urgent billing tickets, highest priority first. What a
// card says lives in noteboard, so that question is noteboard's to answer; this
// store narrows by what IT knows first, and the page is cut after both.
func TestABoardIsFilteredAndSortedByWhatItsCardsSay(t *testing.T) {
	d := buildContentDesk(t)
	for what, c := range map[string]struct {
		query string
		want  []string
	}{
		"one tag":                             {"?tag=billing", []string{"refund", "chargeback", "invoice"}},
		"two tags means both":                 {"?tag=billing&tag=urgent", []string{"chargeback"}},
		"a priority":                          {"?priority=3", []string{"chargeback", "printer", "invoice"}},
		"either priority":                     {"?priority=1&priority=2", []string{"refund", "laptop"}},
		"sorted, ties in stored order":        {"?sort=priority", []string{"chargeback", "printer", "invoice", "laptop", "refund"}},
		"a tag, sorted":                       {"?tag=billing&sort=priority", []string{"chargeback", "invoice", "refund"}},
		"this store's filter AND noteboard's": {"?assignee=" + alice + "&tag=billing", []string{"invoice"}},
		"this store's filter leaves nothing":  {"?unassigned=true&tag=billing&priority=3", []string{"chargeback"}},
		"a status nothing has":                {"?status=done", []string{}},
	} {
		w := requestAs(t, d.h, asService, "GET", "/api/boards/"+d.boardID+"/cards"+c.query, nil)
		mustStatus(t, w, 200, "board "+c.query)
		var view model.BoardView
		decodeSuccessfulResponse(t, w, &view)
		if got := d.titles(view.Columns[0].Cards); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (%s): %v, want %v", what, c.query, got, c.want)
		}
		if view.Columns[0].Total != len(c.want) {
			t.Errorf("%s: total %d, want %d", what, view.Columns[0].Total, len(c.want))
		}
		w = requestAs(t, d.h, asService, "GET", "/api/columns/"+d.columnID+"/cards"+c.query, nil)
		mustStatus(t, w, 200, "column "+c.query)
		var page model.ColumnView
		decodeSuccessfulResponse(t, w, &page)
		if got := d.titles(page.Cards); !reflect.DeepEqual(got, c.want) || page.Total != len(c.want) {
			t.Errorf("%s, column read: %v total %d, want %v", what, got, page.Total, c.want)
		}
	}
}

// "Show more" on a sorted, filtered column: page two must carry on where page
// one stopped, in noteboard's order, with the total of everything that matched.
func TestASortedFilteredColumnPagesInOrder(t *testing.T) {
	d := buildContentDesk(t)
	var seen []string
	for offset := 0; offset < 4; offset += 2 {
		w := requestAs(t, d.h, asService, "GET", fmt.Sprintf("/api/columns/%s/cards?sort=priority&priority=3&priority=2&limit=2&offset=%d", d.columnID, offset), nil)
		mustStatus(t, w, 200, "page")
		var page model.ColumnView
		decodeSuccessfulResponse(t, w, &page)
		if page.Total != 4 {
			t.Fatalf("total %d at offset %d, want the 4 that match", page.Total, offset)
		}
		seen = append(seen, d.titles(page.Cards)...)
	}
	if want := []string{"chargeback", "printer", "invoice", "laptop"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("two pages of two = %v, want %v", seen, want)
	}
	// The cards of a filtered page are whole cards: assignments ride along.
	w := requestAs(t, d.h, asService, "GET", "/api/columns/"+d.columnID+"/cards?tag=billing&priority=3&sort=title", nil)
	var page model.ColumnView
	decodeSuccessfulResponse(t, w, &page)
	if len(page.Cards) != 2 || d.titleOf[page.Cards[1].Placement.CardID] != "invoice" || len(page.Cards[1].Assignments) != 1 {
		t.Errorf("a filtered page's cards = %+v, want chargeback then invoice with alice on it", d.titles(page.Cards))
	}
}

// The sort vocabulary is noteboard's. This store keeps no copy of it, so an
// unknown sort is refused by noteboard and its words come back unchanged. What
// this store CAN judge without asking — that a priority is a number — it does.
func TestNoteboardsRefusalIsRelayedAndThisStoresOwnIsItsOwn(t *testing.T) {
	d := buildContentDesk(t)
	w := requestAs(t, d.h, asService, "GET", "/api/boards/"+d.boardID+"/cards?sort=importance", nil)
	mustStatus(t, w, 400, "a sort noteboard does not have")
	if !strings.Contains(w.Body.String(), "GET /api/items/query-options") {
		t.Errorf("400 body = %s, want noteboard's own words naming where its sorts are served", w.Body.String())
	}
	asked := d.notes.queries
	for _, query := range []string{"?priority=high", "?due_before=tomorrow", "?due_before=" + url.QueryEscape("2026-09-20 17:00")} {
		mustStatus(t, requestAs(t, d.h, asService, "GET", "/api/boards/"+d.boardID+"/cards"+query, nil), 400, "board "+query)
	}
	if d.notes.queries != asked {
		t.Errorf("a request this store could refuse itself still went to noteboard")
	}
}

// A board polled every fifteen seconds with no content filter must not start
// sending its card ids to noteboard to be "filtered" by nothing.
func TestAnUnfilteredReadNeverAsksNoteboardToFilter(t *testing.T) {
	d := buildContentDesk(t)
	for _, target := range []string{"/api/boards/" + d.boardID + "/cards", "/api/boards/" + d.boardID + "/cards?assignee=" + alice + "&limit=2", "/api/columns/" + d.columnID + "/cards?limit=2&offset=2"} {
		mustStatus(t, requestAs(t, d.h, asService, "GET", target, nil), 200, target)
	}
	if d.notes.queries != 0 {
		t.Errorf("%d queries went to noteboard for reads that filter on nothing it knows", d.notes.queries)
	}
}
