package server

import (
	"encoding/json"
	"errors"
	"fmt"

	issuecontent "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuecontent"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	itemsapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/api"
	itemsmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// kanbanIssueContent is the content of issues as the boards and cards call it:
// the issue items API of the issues module, in this process, called in Go, so
// every issue a card writes is checked as one written through /issues/items.
// The boards treat an issue as opaque JSON, so each goes over as those routes
// encode it.
type kanbanIssueContent struct {
	issues *itemsapi.API
}

var _ issuecontent.Contents = kanbanIssueContent{}

// asJSONAs carries value over to target as the routes' JSON would: what one
// side writes is what the other reads.
func asJSONAs(value, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func opaqueItem(item *itemsmodel.Item) (issuecontent.Item, error) {
	if item == nil {
		return nil, nil
	}
	var opaque issuecontent.Item
	if err := asJSONAs(item, &opaque); err != nil {
		return nil, fmt.Errorf("issue %s: %w", item.ID, err)
	}
	return opaque, nil
}

// issueContentCallError is an issue items error as the boards read it: an item
// that is not there is issuecontent.NotFound, and anything else names the
// call.
func issueContentCallError(call, itemID string, err error) error {
	if errors.Is(err, itemsapi.ErrNotFound) && itemID != "" {
		return issuecontent.NotFound(itemID)
	}
	var refusal *itemsapi.RequestRefusal
	if errors.As(err, &refusal) {
		return fmt.Errorf("issues %s: 400 Bad Request — %v", call, refusal)
	}
	return fmt.Errorf("issues %s: %w", call, err)
}

func (content kanbanIssueContent) CreateItem(payload issuecontent.CreateItemPayload) (issuecontent.Item, error) {
	var request itemsmodel.CreateItemRequest
	if err := asJSONAs(payload, &request); err != nil {
		return nil, err
	}
	item, err := content.issues.Create(&request)
	if err != nil {
		return nil, issueContentCallError("POST /items", "", err)
	}
	return opaqueItem(item)
}

func (content kanbanIssueContent) GetItem(id string) (issuecontent.Item, error) {
	item, err := content.issues.Item(id)
	if err != nil {
		return nil, issueContentCallError("GET /items/{id}", id, err)
	}
	return opaqueItem(item)
}

func (content kanbanIssueContent) PatchItem(id string, patch map[string]any) (issuecontent.Item, error) {
	return content.PatchItemIfMatch(id, patch, "")
}

func (content kanbanIssueContent) PatchItemIfMatch(id string, patch map[string]any, ifMatch string) (issuecontent.Item, error) {
	var request itemsmodel.UpdateItemRequest
	if err := asJSONAs(patch, &request); err != nil {
		return nil, fmt.Errorf("issues PATCH /items/{id}: 400 Bad Request — invalid JSON: %w", err)
	}
	item, err := content.issues.Patch(id, &request, ifMatch)
	var changed *items.ItemChangedError
	if errors.As(err, &changed) {
		body, encodeErr := itemsapi.ItemChangedBody(changed)
		if encodeErr != nil {
			return nil, encodeErr
		}
		return nil, &issuecontent.ItemChangedError{ETag: itemsapi.ItemVersionTag(changed.Current), Body: body}
	}
	if err != nil {
		return nil, issueContentCallError("PATCH /items/{id}", id, err)
	}
	return opaqueItem(item)
}

func (content kanbanIssueContent) HoldItem(id, reason string) (issuecontent.Item, error) {
	item, err := content.issues.Hold(id, reason)
	if err != nil {
		return nil, issueContentCallError("POST /items/{id}/hold", id, err)
	}
	return opaqueItem(item)
}

func (content kanbanIssueContent) UnholdItem(id string) (issuecontent.Item, error) {
	item, err := content.issues.Unhold(id)
	if err != nil {
		return nil, issueContentCallError("POST /items/{id}/unhold", id, err)
	}
	return opaqueItem(item)
}

func (content kanbanIssueContent) DeleteItem(id string, hard bool) error {
	if err := content.issues.Delete(id, hard); err != nil {
		return issueContentCallError("DELETE /items/{id}", id, err)
	}
	return nil
}

func (content kanbanIssueContent) GetItems(ids []string) ([]issuecontent.Item, error) {
	if len(ids) == 0 {
		return []issuecontent.Item{}, nil
	}
	batch, err := content.issues.Batch(ids)
	if err != nil {
		return nil, issueContentCallError("batch", "", err)
	}
	if len(batch.Items) != len(ids) {
		return nil, fmt.Errorf("issues batch returned %d slots for %d ids", len(batch.Items), len(ids))
	}
	out := make([]issuecontent.Item, len(ids))
	for i, item := range batch.Items {
		if item != nil && item.ID != ids[i] {
			return nil, fmt.Errorf("issues batch item %d does not match requested id", i)
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
func (content kanbanIssueContent) Search(query string, limit int) ([]issuecontent.Item, error) {
	found, err := content.issues.SearchItems(items.SearchParams{Query: query, Limit: limit, IncludeHeld: true})
	if err != nil {
		return nil, issueContentCallError("search", "", err)
	}
	out := make([]issuecontent.Item, 0, len(found))
	for _, item := range found {
		opaque, err := opaqueItem(item)
		if err != nil {
			return nil, err
		}
		out = append(out, opaque)
	}
	return out, nil
}

func (content kanbanIssueContent) QueryItems(query issuecontent.ItemsQuery) (*issuecontent.ItemsQueryResult, error) {
	var request itemsmodel.ItemsQuery
	if err := asJSONAs(query, &request); err != nil {
		return nil, err
	}
	result, err := content.issues.Query(&request)
	var refusal *itemsapi.RequestRefusal
	if errors.As(err, &refusal) {
		body, encodeErr := json.Marshal(map[string]string{"error": refusal.Error()})
		if encodeErr != nil {
			return nil, encodeErr
		}
		return nil, &issuecontent.QueryRefusedError{Body: append(body, '\n')}
	}
	if err != nil {
		return nil, issueContentCallError("POST /items/query", "", err)
	}
	var out issuecontent.ItemsQueryResult
	if err := asJSONAs(result, &out); err != nil {
		return nil, fmt.Errorf("issues answered a query result that does not parse: %w", err)
	}
	if query.IncludeItems && len(out.Items) != len(out.IDs) {
		return nil, fmt.Errorf("issues answered %d ids and %d items for one page", len(out.IDs), len(out.Items))
	}
	return &out, nil
}
