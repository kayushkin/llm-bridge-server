// Package referencestore is reference-store's list of reference types as
// kanban-store reads it: what each kind of record a card can link is called,
// what its ids look like and where it is fetched. kanban-store used to keep
// that list itself; it now serves reference-store's list unchanged at
// GET /api/entity-types and checks a ticket's source type against it.
//
// reference-store runs in the same process; the server that mounts
// kanban-store gives it the list as Types.
package referencestore

import (
	"context"
	"encoding/json"
	"fmt"
)

// Types is reference-store's list of reference types.
type Types interface {
	// Types answers every reference type row, as reference-store serves it.
	// A reference-store that cannot answer is an error, never an empty list.
	Types(ctx context.Context) ([]json.RawMessage, error)
	// Known says whether a type name is on the list.
	Known(ctx context.Context, typeName string) (bool, error)
}

// List is a list of type rows, read once.
type List struct {
	rows  []json.RawMessage
	names map[string]bool
}

// NewList reads rows, each one reference type as reference-store serves it.
// A row with no type is an error.
func NewList(rows []json.RawMessage) (*List, error) {
	names := make(map[string]bool, len(rows))
	for _, row := range rows {
		var named struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(row, &named); err != nil || named.Type == "" {
			return nil, fmt.Errorf("reference-store's list of types holds a row with no type")
		}
		names[named.Type] = true
	}
	return &List{rows: rows, names: names}, nil
}

func (list *List) Types(context.Context) ([]json.RawMessage, error) { return list.rows, nil }

func (list *List) Known(_ context.Context, typeName string) (bool, error) {
	return list.names[typeName], nil
}
