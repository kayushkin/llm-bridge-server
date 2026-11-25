package api

import (
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/model"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/noteboard"
	"net/http"
	"net/url"
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
	page, err := a.boardCardPageFor(principalBoardAccessFrom(r), boardID, r.URL.Query())
	if err != nil {
		writeRefusal(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// boardCardPageFor is one page of a board's cards across its columns, as
// access may see them, with q the page's query string values.
func (a *API) boardCardPageFor(access *PrincipalBoardAccess, boardID string, q url.Values) (*model.BoardCardPage, error) {
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		return nil, &Refusal{Status: 400, Message: "limit must be a positive integer"}
	}
	offset := 0
	if q.Has("offset") {
		offset, err = strconv.Atoi(q.Get("offset"))
		if err != nil || offset < 0 {
			return nil, &Refusal{Status: 400, Message: "offset must be a nonnegative integer"}
		}
	}
	filter, err := cardFilterFromValues(q)
	if err != nil {
		return nil, &Refusal{Status: 400, Message: err.Error()}
	}
	filter.CardID = q.Get("card_id")
	content, err := cardContentQueryFromValues(q)
	if err != nil {
		return nil, &Refusal{Status: 400, Message: err.Error()}
	}
	query := noteboard.ItemsQuery{IDs: []string{}, Tags: content.Tags, Priorities: content.Priorities, Statuses: content.Statuses, DueBefore: content.DueBefore, Sort: content.Sort, Limit: limit, Offset: offset, IncludeItems: true, Q: q.Get("q"), CreatedFrom: q.Get("created_from"), CreatedTo: q.Get("created_to")}
	for name, value := range map[string]string{"created_from": query.CreatedFrom, "created_to": query.CreatedTo} {
		if value != "" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return nil, &Refusal{Status: 400, Message: name + " must be an RFC3339 timestamp"}
			}
		}
	}
	included, excluded := map[model.TicketLifecycleState]bool{}, map[model.TicketLifecycleState]bool{}
	for name, target := range map[string]map[model.TicketLifecycleState]bool{"lifecycle": included, "exclude_lifecycle": excluded} {
		for _, raw := range q[name] {
			value, ok := model.NormalizeTicketLifecycleState(raw)
			if !ok {
				return nil, &Refusal{Status: 400, Message: model.ErrUnknownTicketLifecycleState(raw).Error()}
			}
			target[value] = true
		}
	}
	board, err := a.store.GetBoard(boardID)
	if err != nil {
		return nil, dbRefusal(err)
	}
	columns, err := a.store.ListColumns(boardID)
	if err != nil {
		return nil, &Refusal{Status: 500, Message: err.Error()}
	}
	ladder, err := a.store.GetPriorityLadder(boardID)
	if err != nil {
		return nil, &Refusal{Status: 500, Message: err.Error()}
	}
	if q.Has("priority_label") && q.Has("priority") {
		return nil, &Refusal{Status: 400, Message: "choose priority or priority_label, not both"}
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
			return nil, &Refusal{Status: 500, Message: err.Error()}
		}
		for _, placement := range rows {
			if id := q.Get("card_id"); id != "" && placement.CardID != id {
				continue
			}
			if _, duplicate := placements[placement.CardID]; duplicate {
				return nil, &Refusal{Status: 500, Message: "card has multiple placements on board"}
			}
			placements[placement.CardID] = placement
			query.IDs = append(query.IDs, placement.CardID)
		}
	}
	// Even an empty set goes to the owner so an invalid content query is refused.
	result, err := a.noteboard.QueryItems(query)
	if err != nil {
		return nil, cardReadRefusal(err)
	}
	page := make([]*model.Placement, len(result.IDs))
	for i, id := range result.IDs {
		placement, ok := placements[id]
		if !ok || result.Items[i] == nil || result.Items[i]["id"] != id {
			return nil, &Refusal{Status: 502, Message: fmt.Sprintf("noteboard returned an unexpected page item %q", id)}
		}
		page[i] = placement
	}
	cards, err := a.assembleCards(board, page, result.Items, ladder, access)
	if err != nil {
		return nil, &Refusal{Status: 500, Message: err.Error()}
	}
	return &model.BoardCardPage{Board: board, Columns: columns, PriorityLadder: ladder, Cards: cards, Total: result.Total, Limit: limit, Offset: offset}, nil
}
