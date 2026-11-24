package api_test

import (
	"fmt"
	"github.com/kayushkin/kanban-store/internal/model"
	"net/url"
	"reflect"
	"testing"
)

func TestGlobalBoardPageFiltersBeforePagingAcrossColumns(t *testing.T) {
	d := buildContentDesk(t)
	response := requestAs(t, d.h, asService, "POST", "/api/boards/"+d.boardID+"/columns", model.CreateColumnRequest{Name: "Resolved"})
	mustStatus(t, response, 201, "second column")
	var column model.Column
	decodeSuccessfulResponse(t, response, &column)
	mustStatus(t, requestAs(t, d.h, asService, "PATCH", "/api/columns/"+column.ID, map[string]any{"lifecycle_state": "resolved"}), 200, "lifecycle")
	priority := 4
	response = requestAs(t, d.h, asService, "POST", "/api/boards/"+d.boardID+"/cards", model.CreateCardRequest{Title: "late urgent", Priority: &priority, ColumnID: column.ID})
	mustStatus(t, response, 201, "card in second column")
	var created model.CardView
	decodeSuccessfulResponse(t, response, &created)
	d.titleOf[created.Placement.CardID] = "late urgent"
	base := "/api/boards/" + d.boardID + "/card-page"
	var seen []string
	for offset := 0; offset < 6; offset += 2 {
		w := requestAs(t, d.h, asService, "GET", fmt.Sprintf("%s?limit=2&offset=%d&sort=priority", base, offset), nil)
		mustStatus(t, w, 200, "global page")
		var page model.BoardCardPage
		decodeSuccessfulResponse(t, w, &page)
		if page.Total != 6 || len(page.Cards) != 2 || len(page.Columns) != 2 {
			t.Fatalf("wrong page: %+v", page)
		}
		seen = append(seen, d.titles(page.Cards)...)
	}
	if want := []string{"late urgent", "chargeback", "printer", "invoice", "laptop", "refund"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("global order %v, want %v", seen, want)
	}
	for _, test := range []struct {
		query string
		want  []string
		total int
	}{
		{"exclude_lifecycle=resolved&sort=priority&limit=2", []string{"chargeback", "printer"}, 5},
		{"lifecycle=resolved&limit=25", []string{"late urgent"}, 1},
		{"assignee=" + alice + "&limit=25", []string{"invoice"}, 1},
		{"unassigned=true&tag=billing&priority=3&limit=1", []string{"chargeback"}, 1},
		{"q=INVOICE&limit=1", []string{"invoice"}, 1},
		{"card_id=" + created.Placement.CardID + "&limit=1", []string{"late urgent"}, 1},
		{"card_id=absent&limit=1", []string{}, 0},
		{"priority_label=absent&limit=1", []string{}, 0},
		{"created_from=" + url.QueryEscape("2030-01-01T00:00:00Z") + "&limit=1", []string{}, 0},
		{"limit=2&offset=999", []string{}, 6},
	} {
		w := requestAs(t, d.h, asService, "GET", base+"?"+test.query, nil)
		mustStatus(t, w, 200, test.query)
		var page model.BoardCardPage
		decodeSuccessfulResponse(t, w, &page)
		if got := d.titles(page.Cards); !reflect.DeepEqual(got, test.want) || page.Total != test.total {
			t.Errorf("%s: cards %v total %d", test.query, got, page.Total)
		}
	}
	for _, query := range []string{"limit=0", "limit=1&offset=-1", "limit=1&lifecycle=guess", "limit=1&sort=guess", "limit=1&created_from=bad", "limit=1&priority=3&priority_label=other"} {
		mustStatus(t, requestAs(t, d.h, asService, "GET", base+"?"+query, nil), 400, query)
	}
}

func TestGlobalBoardPageRequiresBoardViewAccess(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/boards/"+f.financeBoardID+"/card-page?limit=25", nil), 200, "read-only granted board")
	mustStatus(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/boards/"+f.supportBoardID+"/card-page?limit=25", nil), 404, "ungranted board")
	mustStatus(t, requestAs(t, h, nil, "GET", "/api/boards/"+f.financeBoardID+"/card-page?limit=25", nil), 401, "anonymous board")
}
