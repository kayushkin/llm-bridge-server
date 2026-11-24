package api

import (
	"fmt"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"net/http"
	"strconv"
	"time"
)

// boardCardPage pages across columns, AFTER lifecycle/assignment and content
// filters. Content selection belongs to noteboard; only the page crosses back.
func (a *API) boardCardPage(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		writeError(w, 400, "limit must be a positive integer")
		return
	}
	offset := 0
	if q.Has("offset") {
		offset, err = strconv.Atoi(q.Get("offset"))
		if err != nil || offset < 0 {
			writeError(w, 400, "offset must be a nonnegative integer")
			return
		}
	}
	filter, err := cardFilterFromQuery(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	filter.CardID = q.Get("card_id")
	content, err := cardContentQueryFromRequest(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	query := noteboard.ItemsQuery{IDs: []string{}, Tags: content.Tags, Priorities: content.Priorities, Statuses: content.Statuses, DueBefore: content.DueBefore, Sort: content.Sort, Limit: limit, Offset: offset, IncludeItems: true, Q: q.Get("q"), CreatedFrom: q.Get("created_from"), CreatedTo: q.Get("created_to")}
	for name, value := range map[string]string{"created_from": query.CreatedFrom, "created_to": query.CreatedTo} {
		if value != "" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				writeError(w, 400, name+" must be an RFC3339 timestamp")
				return
			}
		}
	}
	included, excluded := map[model.TicketLifecycleState]bool{}, map[model.TicketLifecycleState]bool{}
	for name, target := range map[string]map[model.TicketLifecycleState]bool{"lifecycle": included, "exclude_lifecycle": excluded} {
		for _, raw := range q[name] {
			value, ok := model.NormalizeTicketLifecycleState(raw)
			if !ok {
				writeError(w, 400, model.ErrUnknownTicketLifecycleState(raw).Error())
				return
			}
			target[value] = true
		}
	}
	board, err := a.store.GetBoard(boardID)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	columns, err := a.store.ListColumns(boardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ladder, err := a.store.GetPriorityLadder(boardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if q.Has("priority_label") && q.Has("priority") {
		writeError(w, 400, "choose priority or priority_label, not both")
		return
	}
	priorityMatches := true
	if label := q.Get("priority_label"); label != "" {
		priorityMatches = false
		for _, level := range ladder.Levels {
			if level.Label == label {
				query.Priorities = append(query.Priorities, level.PriorityValue)
				priorityMatches = true
			}
		}
	}
	placements := map[string]*model.Placement{}
	for _, column := range columns {
		if !priorityMatches {
			break
		}
		if len(included) > 0 && (column.LifecycleState == nil || !included[*column.LifecycleState]) {
			continue
		}
		if column.LifecycleState != nil && excluded[*column.LifecycleState] {
			continue
		}
		rows, err := a.store.ListPlacementsByColumnMatching(column.ID, filter, 0, 0)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		for _, placement := range rows {
			if id := q.Get("card_id"); id != "" && placement.CardID != id {
				continue
			}
			if _, duplicate := placements[placement.CardID]; duplicate {
				writeError(w, 500, "card has multiple placements on board")
				return
			}
			placements[placement.CardID] = placement
			query.IDs = append(query.IDs, placement.CardID)
		}
	}
	// Even an empty set goes to the owner so an invalid content query is refused.
	result, err := a.noteboard.QueryItems(query)
	if err != nil {
		writeCardReadError(w, err)
		return
	}
	page := make([]*model.Placement, len(result.IDs))
	for i, id := range result.IDs {
		placement, ok := placements[id]
		if !ok || result.Items[i] == nil || result.Items[i]["id"] != id {
			writeError(w, 502, fmt.Sprintf("noteboard returned an unexpected page item %q", id))
			return
		}
		page[i] = placement
	}
	cards, err := a.assembleCards(board, page, result.Items, ladder, principalBoardAccessFrom(r))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, model.BoardCardPage{Board: board, Columns: columns, PriorityLadder: ladder, Cards: cards, Total: result.Total, Limit: limit, Offset: offset})
}
