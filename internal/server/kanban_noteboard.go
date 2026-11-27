package server

import (
	"encoding/json"
	"errors"
	"fmt"

	issuesnoteboard "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/noteboard"
	noteboardapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	noteboardmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// kanbanNoteboard is noteboard as kanban-store calls it: noteboard's own API
// in this process, called in Go, so every item kanban-store writes is checked
// as one written through noteboard's routes. kanban-store treats an item as
// opaque JSON, so each item goes over as noteboard's routes encode it.
type kanbanNoteboard struct {
	notes *noteboardapi.API
}

var _ issuesnoteboard.Notes = kanbanNoteboard{}

// asJSONAs carries value over to target as the routes' JSON would: what one
// side writes is what the other reads.
func asJSONAs(value, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func opaqueItem(item *noteboardmodel.Item) (issuesnoteboard.Item, error) {
	if item == nil {
		return nil, nil
	}
	var opaque issuesnoteboard.Item
	if err := asJSONAs(item, &opaque); err != nil {
		return nil, fmt.Errorf("noteboard item %s: %w", item.ID, err)
	}
	return opaque, nil
}

// noteboardCallError is a noteboard error as kanban-store reads it: an item
// that is not there is issuesnoteboard.NotFound, and anything else names the
// call.
func noteboardCallError(call, itemID string, err error) error {
	if errors.Is(err, noteboardapi.ErrNotFound) && itemID != "" {
		return issuesnoteboard.NotFound(itemID)
	}
	var refusal *noteboardapi.RequestRefusal
	if errors.As(err, &refusal) {
		return fmt.Errorf("noteboard %s: 400 Bad Request — %v", call, refusal)
	}
	return fmt.Errorf("noteboard %s: %w", call, err)
}

func (noteboard kanbanNoteboard) CreateItem(payload issuesnoteboard.CreateItemPayload) (issuesnoteboard.Item, error) {
	var request noteboardmodel.CreateItemRequest
	if err := asJSONAs(payload, &request); err != nil {
		return nil, err
	}
	item, err := noteboard.notes.Create(&request)
	if err != nil {
		return nil, noteboardCallError("POST /api/items", "", err)
	}
	return opaqueItem(item)
}

func (noteboard kanbanNoteboard) GetItem(id string) (issuesnoteboard.Item, error) {
	item, err := noteboard.notes.Item(id)
	if err != nil {
		return nil, noteboardCallError("GET /api/items/{id}", id, err)
	}
	return opaqueItem(item)
}

func (noteboard kanbanNoteboard) PatchItem(id string, patch map[string]any) (issuesnoteboard.Item, error) {
	return noteboard.PatchItemIfMatch(id, patch, "")
}

func (noteboard kanbanNoteboard) PatchItemIfMatch(id string, patch map[string]any, ifMatch string) (issuesnoteboard.Item, error) {
	var request noteboardmodel.UpdateItemRequest
	if err := asJSONAs(patch, &request); err != nil {
		return nil, fmt.Errorf("noteboard PATCH /api/items/{id}: 400 Bad Request — invalid JSON: %w", err)
	}
	item, err := noteboard.notes.Patch(id, &request, ifMatch)
	var changed *items.ItemChangedError
	if errors.As(err, &changed) {
		body, encodeErr := noteboardapi.ItemChangedBody(changed)
		if encodeErr != nil {
			return nil, encodeErr
		}
		return nil, &issuesnoteboard.ItemChangedError{ETag: noteboardapi.ItemVersionTag(changed.Current), Body: body}
	}
	if err != nil {
		return nil, noteboardCallError("PATCH /api/items/{id}", id, err)
	}
	return opaqueItem(item)
}

func (noteboard kanbanNoteboard) HoldItem(id, reason string) (issuesnoteboard.Item, error) {
	item, err := noteboard.notes.Hold(id, reason)
	if err != nil {
		return nil, noteboardCallError("POST /api/items/{id}/hold", id, err)
	}
	return opaqueItem(item)
}

func (noteboard kanbanNoteboard) UnholdItem(id string) (issuesnoteboard.Item, error) {
	item, err := noteboard.notes.Unhold(id)
	if err != nil {
		return nil, noteboardCallError("POST /api/items/{id}/unhold", id, err)
	}
	return opaqueItem(item)
}

func (noteboard kanbanNoteboard) DeleteItem(id string, hard bool) error {
	if err := noteboard.notes.Delete(id, hard); err != nil {
		return noteboardCallError("DELETE /api/items/{id}", id, err)
	}
	return nil
}

func (noteboard kanbanNoteboard) GetItems(ids []string) ([]issuesnoteboard.Item, error) {
	if len(ids) == 0 {
		return []issuesnoteboard.Item{}, nil
	}
	batch, err := noteboard.notes.Batch(ids)
	if err != nil {
		return nil, noteboardCallError("batch", "", err)
	}
	if len(batch.Items) != len(ids) {
		return nil, fmt.Errorf("noteboard batch returned %d slots for %d ids", len(batch.Items), len(ids))
	}
	out := make([]issuesnoteboard.Item, len(ids))
	for i, item := range batch.Items {
		if item != nil && item.ID != ids[i] {
			return nil, fmt.Errorf("noteboard batch item %d does not match requested id", i)
		}
		if out[i], err = opaqueItem(item); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Search includes held items: a kanban board is where a person finds parked
// work to resume it, and a card that vanished from its board could never be
// un-parked.
func (noteboard kanbanNoteboard) Search(query string, limit int) ([]issuesnoteboard.Item, error) {
	found, err := noteboard.notes.SearchItems(items.SearchParams{Query: query, Limit: limit, IncludeHeld: true})
	if err != nil {
		return nil, noteboardCallError("search", "", err)
	}
	out := make([]issuesnoteboard.Item, 0, len(found))
	for _, item := range found {
		opaque, err := opaqueItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, opaque)
	}
	return out, nil
}

func (noteboard kanbanNoteboard) QueryItems(query issuesnoteboard.ItemsQuery) (*issuesnoteboard.ItemsQueryResult, error) {
	var request noteboardmodel.ItemsQuery
	if err := asJSONAs(query, &request); err != nil {
		return nil, err
	}
	result, err := noteboard.notes.Query(&request)
	var refusal *noteboardapi.RequestRefusal
	if errors.As(err, &refusal) {
		body, encodeErr := json.Marshal(map[string]string{"error": refusal.Error()})
		if encodeErr != nil {
			return nil, encodeErr
		}
		return nil, &issuesnoteboard.QueryRefusedError{Body: append(body, '\n')}
	}
	if err != nil {
		return nil, noteboardCallError("POST /api/items/query", "", err)
	}
	var out issuesnoteboard.ItemsQueryResult
	if err := asJSONAs(result, &out); err != nil {
		return nil, fmt.Errorf("noteboard answered a query result that does not parse: %w", err)
	}
	if query.IncludeItems && len(out.Items) != len(out.IDs) {
		return nil, fmt.Errorf("noteboard answered %d ids and %d items for one page", len(out.IDs), len(out.Items))
	}
	return &out, nil
}
