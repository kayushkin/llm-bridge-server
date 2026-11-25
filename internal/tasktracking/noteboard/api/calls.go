package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// The calls a module in the same process makes, with no request between. Each
// route answers through the same method, so a refusal is the same whichever
// way the call came: a *RequestRefusal is the route's 400, ErrNotFound its 404,
// a *items.ItemChangedError its 412, and any other error its 500.

// ErrNotFound is an id that names no item the call may see.
var ErrNotFound = errors.New("not found")

// Item reads one live item, as GET /api/items/{id} does.
func (a *API) Item(id string) (*model.Item, error) {
	item, err := a.store.GetItem(id)
	if err != nil {
		// The route answers every failure to read the item as not found.
		return nil, ErrNotFound
	}
	return item, nil
}

// Patch changes an item as PATCH /api/items/{id} does. ifMatch is the
// If-Match the caller holds, empty for none: the version the patch was made
// against, refused with *items.ItemChangedError when the item has changed
// since.
func (a *API) Patch(id string, req *model.UpdateItemRequest, ifMatch string) (*model.Item, error) {
	if err := req.Validate(); err != nil {
		return nil, &RequestRefusal{err}
	}
	expectedUpdatedAt, err := expectedUpdatedAtFromIfMatch(ifMatch)
	if err != nil {
		return nil, &RequestRefusal{err}
	}
	req.ExpectedUpdatedAt = expectedUpdatedAt
	item, err := a.store.UpdateItem(id, req)
	var changed *items.ItemChangedError
	if errors.As(err, &changed) {
		return nil, changed
	}
	if err != nil {
		// A rejected schedule is a bad request, not a missing item. Collapsing
		// every store error to 404 would report "not found" for an item that is
		// plainly there, and hide the one message that says what is wrong with
		// the rule.
		if _, missing := a.store.GetItem(id); missing != nil {
			return nil, ErrNotFound
		}
		return nil, &RequestRefusal{err}
	}
	return item, nil
}

// ItemVersionTag is an item's version as an ETag, as the routes send it.
func ItemVersionTag(item *model.Item) string { return itemVersionTag(item) }

// ItemChangedBody is the body of the 412 a patch made against an old version
// answers: the error and the item as it is now.
func ItemChangedBody(changed *items.ItemChangedError) ([]byte, error) {
	encoded, err := json.Marshal(itemChangedResponse{Error: changed.Error(), Current: changed.Current})
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Delete deletes an item as DELETE /api/items/{id}[?hard=true] does:
// reversibly unless hard.
func (a *API) Delete(id string, hard bool) error {
	err := a.store.DeleteItem(id, hard)
	if errors.Is(err, items.ErrItemNotFound) {
		return ErrNotFound
	}
	return err
}

// Hold parks an item, as POST /api/items/{id}/hold does.
func (a *API) Hold(id, reason string) (*model.Item, error) {
	item, err := a.store.HoldItem(id, reason)
	if errors.Is(err, items.ErrTextHoldsNulCharacter) {
		return nil, &RequestRefusal{err}
	}
	if err != nil {
		return nil, ErrNotFound
	}
	return item, nil
}

// Unhold clears an item's hold, as POST /api/items/{id}/unhold does.
func (a *API) Unhold(id string) (*model.Item, error) {
	item, err := a.store.UnholdItem(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return item, nil
}

// Query answers which of the named items match, in what order, as
// POST /api/items/query does.
func (a *API) Query(query *model.ItemsQuery) (*model.ItemsQueryResult, error) {
	if err := query.Validate(); err != nil {
		return nil, &RequestRefusal{err}
	}
	return a.store.QueryItems(query)
}

// Batch reads the named items in the order named, nil for one that names no
// live item, as POST /api/items/batch does.
func (a *API) Batch(ids []string) (*model.ItemsBatchResult, error) {
	if ids == nil {
		return nil, &RequestRefusal{errors.New("ids is required")}
	}
	query := model.ItemsQuery{IDs: ids, IncludeItems: true}
	if err := query.Validate(); err != nil {
		return nil, &RequestRefusal{err}
	}
	result, err := a.store.QueryItems(&query)
	if err != nil {
		return nil, err
	}
	byID := map[string]*model.Item{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	out := model.ItemsBatchResult{Items: make([]*model.Item, len(ids))}
	for i, id := range ids {
		out.Items[i] = byID[id]
	}
	return &out, nil
}

// SearchItems is a full-text search, as GET /api/search does.
func (a *API) SearchItems(params items.SearchParams) ([]*model.Item, error) {
	if params.Query == "" {
		return nil, &RequestRefusal{errors.New("q parameter is required")}
	}
	return a.store.Search(params)
}

// writeCallError answers err from one of these calls as the routes answer
// it.
func writeCallError(w http.ResponseWriter, err error) {
	var refusal *RequestRefusal
	switch {
	case errors.As(err, &refusal):
		writeError(w, 400, refusal.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, 404, "not found")
	default:
		writeError(w, 500, err.Error())
	}
}
