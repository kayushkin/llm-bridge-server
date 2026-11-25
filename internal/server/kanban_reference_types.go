package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	kanbanreferences "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/referencestore"
)

// kanbanReferenceTypes is reference-store's list of types as kanban-store
// reads it, from the resolver in this process. The list is compiled into
// this binary; until EnableReferenceStore has run it is an error.
type kanbanReferenceTypes struct {
	server *Server
}

func (references kanbanReferenceTypes) list() (*kanbanreferences.List, error) {
	resolver := references.server.referenceResolver.Load()
	if resolver == nil {
		return nil, errors.New("reference-store is not open on this server")
	}
	rows, err := referencestore.TypeRows(resolver.Types)
	if err != nil {
		return nil, err
	}
	return kanbanreferences.NewList(rows)
}

func (references kanbanReferenceTypes) Types(ctx context.Context) ([]json.RawMessage, error) {
	list, err := references.list()
	if err != nil {
		return nil, err
	}
	return list.Types(ctx)
}

func (references kanbanReferenceTypes) Known(ctx context.Context, typeName string) (bool, error) {
	list, err := references.list()
	if err != nil {
		return false, err
	}
	return list.Known(ctx, typeName)
}
