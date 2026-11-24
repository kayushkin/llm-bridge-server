package projectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// Noteboard is the part of noteboard the store calls: the title of the item a
// note link names, the items a project's note links name, and a new todo for a
// next step. NoteboardModule answers it from the noteboard module in this
// process.
type Noteboard interface {
	// ItemTitle is the title of a live item; ErrNotFound when no live item has
	// the id.
	ItemTitle(ctx context.Context, id string) (string, error)
	// LinkedItems answers the live items among ids, and the ids that name none.
	LinkedItems(ctx context.Context, ids []string) ([]NoteboardItem, []string, error)
	// CreateTodo makes a todo and answers it.
	CreateTodo(ctx context.Context, title, body string, tags []string, priority *int) (NoteboardItem, error)
}

// NoteboardModule is Noteboard over the noteboard module's store. It answers
// what noteboard's routes answered over HTTP: the same items, the same
// refusals and the same fields in the same form, because each item is read
// through the JSON noteboard writes.
type NoteboardModule struct {
	Store *items.Store
}

// noteboardItemOf is item as noteboard's API writes it.
func noteboardItemOf(item *model.Item) (NoteboardItem, error) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return NoteboardItem{}, err
	}
	var read NoteboardItem
	if err := json.Unmarshal(encoded, &read); err != nil {
		return NoteboardItem{}, err
	}
	return read, nil
}

func (module NoteboardModule) ItemTitle(_ context.Context, id string) (string, error) {
	item, err := module.Store.GetItem(id)
	if errors.Is(err, items.ErrItemNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: noteboard item %s: %v", ErrOwnerUnavailable, id, err)
	}
	return item.Title, nil
}

func (module NoteboardModule) LinkedItems(_ context.Context, ids []string) ([]NoteboardItem, []string, error) {
	query := model.ItemsQuery{IDs: ids, IncludeItems: true, Limit: len(ids)}
	if err := query.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: noteboard refuses the query: %v", ErrOwnerUnavailable, err)
	}
	result, err := module.Store.QueryItems(&query)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: noteboard items: %v", ErrOwnerUnavailable, err)
	}
	found := make([]NoteboardItem, 0, len(result.Items))
	for _, item := range result.Items {
		read, err := noteboardItemOf(item)
		if err != nil {
			return nil, nil, err
		}
		found = append(found, read)
	}
	return found, result.MissingIDs, nil
}

func (module NoteboardModule) CreateTodo(_ context.Context, title, body string, tags []string, priority *int) (NoteboardItem, error) {
	request := model.CreateItemRequest{Type: model.TypeTodo, Title: title, Body: &body, Tags: tags, Priority: priority}
	if err := request.Validate(); err != nil {
		return NoteboardItem{}, fmt.Errorf("%w: noteboard refuses the todo: %v", ErrOwnerUnavailable, err)
	}
	item, err := module.Store.CreateItem(&request)
	if err != nil {
		return NoteboardItem{}, fmt.Errorf("%w: making a noteboard todo: %v", ErrOwnerUnavailable, err)
	}
	return noteboardItemOf(item)
}
