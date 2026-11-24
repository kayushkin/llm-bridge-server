package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

func createItem(t *testing.T, h http.Handler, fields map[string]any) string {
	t.Helper()
	if _, given := fields["type"]; !given {
		fields["type"] = "todo"
	}
	body, _ := json.Marshal(fields)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items", bytes.NewReader(body)))
	if w.Code != 201 {
		t.Fatalf("create %v: %d %s", fields, w.Code, w.Body.String())
	}
	var item model.Item
	json.Unmarshal(w.Body.Bytes(), &item)
	return item.ID
}

func queryItems(t *testing.T, h http.Handler, query map[string]any, want int) model.ItemsQueryResult {
	t.Helper()
	body, _ := json.Marshal(query)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items/query", bytes.NewReader(body)))
	if w.Code != want {
		t.Fatalf("query %v: %d %s, want %d", query, w.Code, w.Body.String(), want)
	}
	var result model.ItemsQueryResult
	json.Unmarshal(w.Body.Bytes(), &result)
	return result
}

// kanban-store knows which cards sit in a column and nothing of what they say.
// It sends the column's ids and asks which carry a tag, at which priority, in
// what order — one call, instead of reading every card to look.
func TestOfTheNamedItemsWhichMatchAndInWhatOrder(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	billingLow := createItem(t, h, map[string]any{"title": "b refund", "tags": []string{"billing"}, "priority": 1})
	billingHigh := createItem(t, h, map[string]any{"title": "A chargeback", "tags": []string{"billing", "urgent"}, "priority": 3})
	hardware := createItem(t, h, map[string]any{"title": "c printer", "tags": []string{"hardware"}, "priority": 3})
	notAsked := createItem(t, h, map[string]any{"title": "on no board", "tags": []string{"billing"}, "priority": 3})
	column := []string{billingLow, billingHigh, hardware} // the order the cards sit in

	for what, c := range map[string]struct {
		query map[string]any
		want  []string
	}{
		"no filter keeps the order sent": {map[string]any{}, []string{billingLow, billingHigh, hardware}},
		"one tag":                        {map[string]any{"tags": []string{"billing"}}, []string{billingLow, billingHigh}},
		"two tags means both":            {map[string]any{"tags": []string{"billing", "urgent"}}, []string{billingHigh}},
		"a priority":                     {map[string]any{"priorities": []int{3}}, []string{billingHigh, hardware}},
		"either priority":                {map[string]any{"priorities": []int{1, 3}}, []string{billingLow, billingHigh, hardware}},
		"tag and priority":               {map[string]any{"tags": []string{"billing"}, "priorities": []int{3}}, []string{billingHigh}},
		"by priority, ties as sent":      {map[string]any{"sort": "priority"}, []string{billingHigh, hardware, billingLow}},
		"by title, ignoring case":        {map[string]any{"sort": "title"}, []string{billingHigh, billingLow, hardware}},
		"a status nothing has":           {map[string]any{"statuses": []string{"done"}}, []string{}},
	} {
		c.query["ids"] = column
		result := queryItems(t, h, c.query, 200)
		if !reflect.DeepEqual(result.IDs, c.want) {
			t.Errorf("%s: ids %v, want %v", what, result.IDs, c.want)
		}
		if result.Total != len(c.want) {
			t.Errorf("%s: total %d, want %d", what, result.Total, len(c.want))
		}
		for _, id := range result.IDs {
			if id == notAsked {
				t.Errorf("%s: answered an item that was not named", what)
			}
		}
	}
}

// A page is cut after the filter and the sort, and the total is of the matches.
func TestAQueryPagesAfterTheFilterAndCarriesItemsWhenAsked(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	ids := []string{}
	for _, title := range []string{"one", "two", "three", "four", "five"} {
		ids = append(ids, createItem(t, h, map[string]any{"title": title, "tags": []string{"keep"}}))
	}
	createdOther := createItem(t, h, map[string]any{"title": "other", "tags": []string{"drop"}})
	all := append([]string{createdOther}, ids...)

	page := queryItems(t, h, map[string]any{"ids": all, "tags": []string{"keep"}, "limit": 2, "offset": 2, "include_items": true}, 200)
	if page.Total != 5 || !reflect.DeepEqual(page.IDs, ids[2:4]) {
		t.Fatalf("page = total %d ids %v, want total 5 and the third and fourth matches %v", page.Total, page.IDs, ids[2:4])
	}
	if len(page.Items) != 2 || page.Items[0].Title != "three" || page.Items[1].ID != ids[3] {
		t.Errorf("items = %+v, want the same page as items, in the same order", page.Items)
	}
	if without := queryItems(t, h, map[string]any{"ids": all, "limit": 1}, 200); without.Items != nil {
		t.Errorf("items were answered without being asked for")
	}
}

// due_at is stored with the offset its writer sent. 10:00-07:00 is 17:00Z, five
// hours AFTER 12:00Z, and as text it sorts before it.
func TestDueDatesAreComparedAsInstantsNotText(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	noonUTC := createItem(t, h, map[string]any{"title": "12:00Z", "due_at": "2026-07-19T12:00:00Z"})
	tenPacific := createItem(t, h, map[string]any{"title": "10:00-07:00 = 17:00Z", "due_at": "2026-07-19T10:00:00-07:00"})
	noDate := createItem(t, h, map[string]any{"title": "no due date"})
	ids := []string{noDate, tenPacific, noonUTC}

	if got, want := queryItems(t, h, map[string]any{"ids": ids, "sort": "due_at"}, 200).IDs, []string{noonUTC, tenPacific, noDate}; !reflect.DeepEqual(got, want) {
		t.Errorf("sorted by due date = %v, want 12:00Z, then 17:00Z, then the one with no date", got)
	}
	// 15:00Z falls between them.
	if got, want := queryItems(t, h, map[string]any{"ids": ids, "due_before": "2026-07-19T15:00:00Z"}, 200).IDs, []string{noonUTC}; !reflect.DeepEqual(got, want) {
		t.Errorf("due before 15:00Z = %v, want only the one due at 12:00Z", got)
	}
}

// "Did not match" and "is gone" are different answers, and a held item is one
// the caller named, not one it discovered.
func TestAQuerySaysWhichIdsAreGoneAndAnswersHeldItems(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	live := createItem(t, h, map[string]any{"title": "live", "tags": []string{"x"}})
	held := createItem(t, h, map[string]any{"title": "held", "tags": []string{"x"}})
	deleted := createItem(t, h, map[string]any{"title": "deleted", "tags": []string{"x"}})
	for _, call := range [][2]string{{"POST", "/api/items/" + held + "/hold"}, {"DELETE", "/api/items/" + deleted}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(call[0], call[1], bytes.NewReader([]byte(`{}`))))
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", call[0], call[1], w.Code, w.Body.String())
		}
	}
	result := queryItems(t, h, map[string]any{"ids": []string{live, held, deleted, "never-existed"}, "tags": []string{"no-item-has-this"}}, 200)
	if len(result.IDs) != 0 || !reflect.DeepEqual(result.MissingIDs, []string{deleted, "never-existed"}) {
		t.Errorf("ids %v missing %v; want no match, and the deleted and unknown ids reported gone whatever the filter", result.IDs, result.MissingIDs)
	}
	if got := queryItems(t, h, map[string]any{"ids": []string{live, held, deleted}, "tags": []string{"x"}}, 200).IDs; !reflect.DeepEqual(got, []string{live, held}) {
		t.Errorf("ids %v, want the live and the held item", got)
	}
}

// A filter that was dropped answers every item, which reads as "all match".
func TestAQueryThatCannotBeReadIsRefused(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	id := createItem(t, h, map[string]any{"title": "x"})
	for what, query := range map[string]map[string]any{
		"a misspelled filter": {"ids": []string{id}, "tag": []string{"billing"}},
		"an unknown sort":     {"ids": []string{id}, "sort": "importance"},
		"an empty tag":        {"ids": []string{id}, "tags": []string{""}},
		"an empty id":         {"ids": []string{""}},
		"a negative offset":   {"ids": []string{id}, "offset": -1},
		"a page too large":    {"ids": []string{id}, "limit": model.MaxItemsQueryLimit + 1},
	} {
		queryItems(t, h, query, 400)
		_ = what
	}
	if empty := queryItems(t, h, map[string]any{"ids": []string{}}, 200); empty.Total != 0 || empty.IDs == nil || empty.MissingIDs == nil {
		t.Errorf("no ids = %+v, want zero matches and empty lists, not nulls", empty)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/items/query-options", nil))
	var options model.ItemsQueryOptions
	json.Unmarshal(w.Body.Bytes(), &options)
	if w.Code != 200 || !reflect.DeepEqual(options.Sorts, model.ItemsQuerySorts) || options.MaxIDs != model.MaxItemsQueryIDs {
		t.Errorf("options: %d %+v", w.Code, options)
	}
}
