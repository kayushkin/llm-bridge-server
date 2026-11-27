package possessions

import (
	"context"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// Reads of one record by the route a reference type names it under, for a
// caller in this process: the same reads the GET routes make, narrowed to a
// scope given as a Go value rather than read from a request.

// kindRecord is a kind's record as GET /<kind>/{id} answers it: the
// possession it belongs to.
func (server *Server) kindRecord(kind RecordType, id string, includeDeleted bool) (map[string]any, error) {
	possessionID, err := server.Store.PossessionIDForKindRecord(kind, id)
	if err != nil {
		return nil, err
	}
	return server.Store.GetPossession(possessionID, includeDeleted)
}

// kindImage is the picture GET /<kind>/{id}/image answers: its possession's.
func (server *Server) kindImage(ctx context.Context, kind RecordType, id string) ([]byte, error) {
	possessionID, err := server.Store.PossessionIDForKindRecord(kind, id)
	if err != nil {
		return nil, err
	}
	return server.Reviewer.PossessionImage(ctx, possessionID)
}

// outfitWithCoverBox is an outfit as GET /outfits/{id} answers it.
func (server *Server) outfitWithCoverBox(ctx context.Context, id string, includeDeleted bool) (Outfit, error) {
	outfit, err := server.Store.GetOutfit(id, includeDeleted)
	if err != nil {
		return Outfit{}, err
	}
	if err := server.Reviewer.FillOutfitCoverBox(ctx, &outfit); err != nil {
		return Outfit{}, err
	}
	return outfit, nil
}

// Routes the record readers answer, besides each kind's own.
const (
	PossessionsRoute = "possessions"
	OutfitsRoute     = "outfits"
)

func kindOfRoute(route string) (RecordType, bool) {
	for _, kind := range KindTypes {
		if kind.Route == route {
			return kind, true
		}
	}
	return RecordType{}, false
}

// RecordInScope reads the record GET /<route>/{id} answers, in scope: a
// possession, an outfit, or a kind's record (as its possession). A record
// outside scope is ErrNotFound, as a missing one is.
func (server *Server) RecordInScope(ctx context.Context, scope ownership.Scope, route, id string) (any, error) {
	narrowed, err := server.narrowedTo(ctx, scope)
	if err != nil {
		return nil, err
	}
	switch route {
	case PossessionsRoute:
		return narrowed.Store.GetPossession(id, false)
	case OutfitsRoute:
		return narrowed.outfitWithCoverBox(ctx, id, false)
	}
	kind, known := kindOfRoute(route)
	if !known {
		return nil, fmt.Errorf("possession-store answers no record under /%s", route)
	}
	return narrowed.kindRecord(kind, id, false)
}

// ImageInScope is the JPEG GET /<route>/{id}/image answers, in scope.
func (server *Server) ImageInScope(ctx context.Context, scope ownership.Scope, route, id string) ([]byte, error) {
	narrowed, err := server.narrowedTo(ctx, scope)
	if err != nil {
		return nil, err
	}
	switch route {
	case PossessionsRoute:
		return narrowed.Reviewer.PossessionImage(ctx, id)
	case OutfitsRoute:
		return narrowed.Reviewer.OutfitCoverImage(ctx, id)
	}
	kind, known := kindOfRoute(route)
	if !known {
		return nil, fmt.Errorf("possession-store has no picture under /%s", route)
	}
	return narrowed.kindImage(ctx, kind, id)
}
