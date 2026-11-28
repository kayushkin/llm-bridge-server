// Package review is possession-store's photo review and everything else
// that asks the two readers: the review of a photo and the owner's questions
// it leaves, the wearable audit (links, duplicates, closed groups, picture
// matches), classifying kindless possessions, reading product listings,
// eBay order mail and budget-store's order lines. Its records are in the
// possessions module with every other record; it reaches them through the
// possessions store, and reaches outfits only through OutfitSteps.
package review

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
)

// Store is the possessions store with the photo review's records: reviews,
// readings, sightings and owner questions. Like the store it wraps, it
// reaches one scope's records once narrowed.
type Store struct {
	*possessions.Store
	// outfits carries out the review's steps that change outfits
	// (UseOutfits).
	outfits OutfitSteps
}

// NewStore is the photo review's store over records.
func NewStore(records *possessions.Store) *Store { return &Store{Store: records} }

// Narrowed is the store reaching scope's records.
func (s *Store) Narrowed(ctx context.Context, scope ownership.Scope) (*Store, error) {
	narrowed, err := s.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &Store{Store: narrowed, outfits: s.outfits}, nil
}

// NarrowedToOwner is Narrowed to one owner's records.
func (s *Store) NarrowedToOwner(ctx context.Context, ownerPrincipalID string) (*Store, error) {
	return s.Narrowed(ctx, ownership.OwnedBy(ownerPrincipalID))
}
