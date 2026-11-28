package possessions

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// OutfitsRoute is the route the record readers answer outfits under.
const OutfitsRoute = "outfits"

// outfitWithCoverBox is an outfit as GET /outfits/{id} answers it.
func (server *OutfitServer) outfitWithCoverBox(ctx context.Context, id string, includeDeleted bool) (Outfit, error) {
	outfit, err := server.Reviewer.Store.GetOutfit(id, includeDeleted)
	if err != nil {
		return Outfit{}, err
	}
	if err := server.Reviewer.FillOutfitCoverBox(ctx, &outfit); err != nil {
		return Outfit{}, err
	}
	return outfit, nil
}

// OutfitInScope reads the outfit GET /outfits/{id} answers, in scope. An
// outfit outside scope is ErrNotFound, as a missing one is.
func (server *OutfitServer) OutfitInScope(ctx context.Context, scope ownership.Scope, id string) (Outfit, error) {
	narrowed, err := server.narrowedTo(ctx, scope)
	if err != nil {
		return Outfit{}, err
	}
	return narrowed.outfitWithCoverBox(ctx, id, false)
}

// OutfitImageInScope is the JPEG GET /outfits/{id}/image answers, in scope.
func (server *OutfitServer) OutfitImageInScope(ctx context.Context, scope ownership.Scope, id string) ([]byte, error) {
	narrowed, err := server.narrowedTo(ctx, scope)
	if err != nil {
		return nil, err
	}
	return narrowed.Reviewer.OutfitCoverImage(ctx, id)
}
