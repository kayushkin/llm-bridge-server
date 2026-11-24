package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
)

// cardContentQuery filters and sorts cards by what they SAY — tags, priority,
// status, due date — which is noteboard's to know. db.CardFilter is the other
// half: what this store knows of a card, run in its own SQL. When nothing here
// is set a read never asks noteboard to filter anything.
type cardContentQuery struct {
	Tags       []string
	Priorities []int
	Statuses   []string
	DueBefore  string
	Sort       string
}

func (q cardContentQuery) isEmpty() bool {
	return len(q.Tags) == 0 && len(q.Priorities) == 0 && len(q.Statuses) == 0 && q.DueBefore == "" && q.Sort == ""
}

// cardContentQueryFromRequest reads ?tag=&tag=&priority=&status=&due_before=&sort=.
// It judges only what it can without asking: that a priority is a number and a
// time is a time. What a sort or a status may be is noteboard's vocabulary
// (its GET /api/items/query-options), so those go through as written and
// noteboard's refusal is relayed — this store keeps no copy of the list to
// fall out of date.
func cardContentQueryFromRequest(r *http.Request) (cardContentQuery, error) {
	values := r.URL.Query()
	query := cardContentQuery{Tags: values["tag"], Statuses: values["status"], DueBefore: values.Get("due_before"), Sort: values.Get("sort")}
	for _, raw := range values["priority"] {
		priority, err := strconv.Atoi(raw)
		if err != nil {
			return cardContentQuery{}, fmt.Errorf("priority must be a whole number, the value of a rung on the board's ladder; got %q", raw)
		}
		query.Priorities = append(query.Priorities, priority)
	}
	if query.DueBefore != "" {
		if _, err := time.Parse(time.RFC3339, query.DueBefore); err != nil {
			return cardContentQuery{}, fmt.Errorf("due_before must be an RFC 3339 time such as 2026-09-20T17:00:00-07:00; got %q", query.DueBefore)
		}
	}
	return query, nil
}

// columnPage is one column's cards as they are to be shown: the placements and
// their noteboard items, index for index, and how many matched in all.
//
// With no content query the page is cut in this store's SQL, in stored order,
// and the items are then read. With one, the column's card ids — already
// narrowed by everything this store knows — go to noteboard, which filters,
// sorts and pages them and answers the page's items in the same call. Either
// way the page is cut AFTER every filter, and total counts what matched.
//
// A card whose noteboard item is gone cannot match a content query, so such a
// read reports no orphans; an unfiltered one still does.
func (a *API) columnPage(columnID string, filter db.CardFilter, content cardContentQuery, limit, offset int) ([]*model.Placement, []noteboard.Item, int, error) {
	if content.isEmpty() {
		total, err := a.store.CountColumnCardsMatching(columnID, filter)
		if err != nil {
			return nil, nil, 0, &ownStoreError{err}
		}
		placements, err := a.store.ListPlacementsByColumnMatching(columnID, filter, limit, offset)
		if err != nil {
			return nil, nil, 0, &ownStoreError{err}
		}
		ids := make([]string, len(placements))
		for i, placement := range placements {
			ids[i] = placement.CardID
		}
		items, err := a.noteboard.GetItems(ids)
		return placements, items, total, err
	}

	every, err := a.store.ListPlacementsByColumnMatching(columnID, filter, 0, 0)
	if err != nil {
		return nil, nil, 0, &ownStoreError{err}
	}
	if len(every) == 0 {
		return []*model.Placement{}, []noteboard.Item{}, 0, nil
	}
	placementOfCard := make(map[string]*model.Placement, len(every))
	ids := make([]string, len(every))
	for i, placement := range every {
		ids[i] = placement.CardID // stored order: noteboard breaks a sort's ties by it
		placementOfCard[placement.CardID] = placement
	}
	result, err := a.noteboard.QueryItems(noteboard.ItemsQuery{
		IDs: ids, Tags: content.Tags, Priorities: content.Priorities, Statuses: content.Statuses, DueBefore: content.DueBefore,
		Sort: content.Sort, Limit: limit, Offset: offset, IncludeItems: true,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	page := make([]*model.Placement, len(result.IDs))
	for i, id := range result.IDs {
		placement, sent := placementOfCard[id]
		if !sent {
			return nil, nil, 0, fmt.Errorf("noteboard answered item %s, which was not among the %d ids sent for column %s", id, len(ids), columnID)
		}
		page[i] = placement
	}
	return page, result.Items, result.Total, nil
}

// ownStoreError marks a failure of this store's own database, so that a read
// which also asks noteboard can answer 500 for one and 502 for the other.
type ownStoreError struct{ err error }

func (e *ownStoreError) Error() string { return e.err.Error() }
func (e *ownStoreError) Unwrap() error { return e.err }

// writeCardReadError answers a failed board or column read. noteboard's refusal
// of a query is the caller's to read, as noteboard wrote it.
func writeCardReadError(w http.ResponseWriter, err error) {
	var refused *noteboard.QueryRefusedError
	if errors.As(err, &refused) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(refused.Body)
		return
	}
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, 404, "not found")
		return
	}
	var own *ownStoreError
	if errors.As(err, &own) {
		writeError(w, 500, err.Error())
		return
	}
	writeError(w, 502, err.Error())
}
