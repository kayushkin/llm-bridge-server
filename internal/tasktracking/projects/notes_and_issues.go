package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// NotesAndIssues is the part of notes and issues the store calls: the title
// of the item a note link names, the items a project's note links name, and a
// new issue for a next step. A note link names a note or an issue, as it
// named a noteboard item before noteboard's todos became issues, so both are
// asked. NotesAndIssuesModules answers it from the two modules in this
// process.
type NotesAndIssues interface {
	// ItemTitle is the title of a live note or issue; ErrNotFound when
	// neither module has a live item with the id.
	ItemTitle(ctx context.Context, id string) (string, error)
	// LinkedItems answers the live items among ids, in the order of ids, and
	// the ids that name none in either module.
	LinkedItems(ctx context.Context, ids []string) ([]LinkedItem, []string, error)
	// CreateTodo makes an issue and answers it.
	CreateTodo(ctx context.Context, title, body string, tags []string, priority *int) (LinkedItem, error)
}

// NotesAndIssuesModules is NotesAndIssues over the notes store and the
// issues module's store of issues. It answers what noteboard's routes
// answered over HTTP: the same items, the same refusals and the same fields
// in the same form, because each item is read through the JSON the items
// routes write.
type NotesAndIssuesModules struct {
	Notes  *items.Store
	Issues *items.Store
}

// linkedItemOf is item as the items routes write it.
func linkedItemOf(item *model.Item) (LinkedItem, error) {
	encoded, err := json.Marshal(item)
	if err != nil {
		return LinkedItem{}, err
	}
	var read LinkedItem
	if err := json.Unmarshal(encoded, &read); err != nil {
		return LinkedItem{}, err
	}
	return read, nil
}

func (modules NotesAndIssuesModules) ItemTitle(_ context.Context, id string) (string, error) {
	for _, store := range []*items.Store{modules.Notes, modules.Issues} {
		item, err := store.GetItem(id)
		if errors.Is(err, items.ErrItemNotFound) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%w: %s item %s: %v", ErrOwnerUnavailable, store.Holding().Name, id, err)
		}
		return item.Title, nil
	}
	return "", ErrNotFound
}

func (modules NotesAndIssuesModules) LinkedItems(_ context.Context, ids []string) ([]LinkedItem, []string, error) {
	byID := map[string]*model.Item{}
	remaining := ids
	for _, store := range []*items.Store{modules.Notes, modules.Issues} {
		if len(remaining) == 0 {
			break
		}
		query := model.ItemsQuery{IDs: remaining, IncludeItems: true, Limit: len(remaining)}
		if err := query.Validate(); err != nil {
			return nil, nil, fmt.Errorf("%w: %s refuses the query: %v", ErrOwnerUnavailable, store.Holding().Name, err)
		}
		result, err := store.QueryItems(&query)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s items: %v", ErrOwnerUnavailable, store.Holding().Name, err)
		}
		for _, item := range result.Items {
			byID[item.ID] = item
		}
		remaining = result.MissingIDs
	}
	found := make([]LinkedItem, 0, len(byID))
	for _, id := range ids {
		item, live := byID[id]
		if !live {
			continue
		}
		read, err := linkedItemOf(item)
		if err != nil {
			return nil, nil, err
		}
		found = append(found, read)
	}
	return found, remaining, nil
}

func (modules NotesAndIssuesModules) CreateTodo(_ context.Context, title, body string, tags []string, priority *int) (LinkedItem, error) {
	request := model.CreateItemRequest{Type: model.TypeTodo, Title: title, Body: &body, Tags: tags, Priority: priority}
	if err := request.Validate(); err != nil {
		return LinkedItem{}, fmt.Errorf("%w: issues refuses the todo: %v", ErrOwnerUnavailable, err)
	}
	item, err := modules.Issues.CreateItem(&request)
	if err != nil {
		return LinkedItem{}, fmt.Errorf("%w: making an issue: %v", ErrOwnerUnavailable, err)
	}
	return linkedItemOf(item)
}
