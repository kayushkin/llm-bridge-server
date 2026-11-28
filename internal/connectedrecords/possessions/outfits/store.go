// Package outfits keeps possession-store's outfits: sets of wearables read
// from photos of the owner, their merges and covers, the looks and slots
// read from a day's photos, the rebuilds that make outfits of looks, and the
// passes that read, identify and consolidate them. The records are in the
// possessions module with every other record; this package reaches them
// through the possessions store and the photo review it builds on.
package outfits

import (
	"context"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

// Store is the photo review's store with the outfits' records. Like the store
// it wraps, it reaches one scope's records once narrowed.
type Store struct {
	*review.Store
}

// Narrowed is the store reaching scope's records.
func (s *Store) Narrowed(ctx context.Context, scope ownership.Scope) (*Store, error) {
	narrowed, err := s.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &Store{Store: narrowed}, nil
}

// NarrowedToOwner is Narrowed to one owner's records.
func (s *Store) NarrowedToOwner(ctx context.Context, ownerPrincipalID string) (*Store, error) {
	return s.Narrowed(ctx, ownership.OwnedBy(ownerPrincipalID))
}

// ReviewerSettings are what the outfits' passes need besides the photo
// review's reviewer.
type ReviewerSettings struct {
	// OwnerLocation is the owner's time zone: an outfit read from a photo
	// was worn on the photo's day there.
	OwnerLocation *time.Location
	// ReadCoverWearersInBackgroundFor says whose outfits the outfit list may
	// start reading the wearer box of covers for, when they have none
	// (QueueCoverWearerReads); nil reads none.
	ReadCoverWearersInBackgroundFor func(ownerPrincipalID string) bool
	// CoverImagesDirectory keeps outfit covers already cut (outfit_covers.go);
	// any file there can be deleted and is cut again when asked for.
	CoverImagesDirectory string
}

// Reviewer runs the outfits' passes that ask the readers, through the photo
// review's reviewer, on the same scope's records.
type Reviewer struct {
	*review.Reviewer
	ReviewerSettings
	// Store is the photo review's store with the outfits' records.
	Store *Store
	// shared is what every Reviewer narrowed from one holds in common.
	shared *reviewerShared
}

// reviewerShared is what Narrowed copies share: what outfit covers cost to
// work out (outfit_covers.go).
type reviewerShared struct {
	covers *outfitCovers
}

// NewReviewer is the outfits' reviewer working through reviewer, the photo
// review's. It wires itself as the outfit steps of reviewer's store
// (review.Store.UseOutfits), so a review that reads an outfit, and an
// answer that changes one, run here.
func NewReviewer(reviewer *review.Reviewer, settings ReviewerSettings) *Reviewer {
	outfitReviewer := &Reviewer{Reviewer: reviewer, ReviewerSettings: settings, Store: &Store{Store: reviewer.Store},
		shared: &reviewerShared{covers: newOutfitCovers()}}
	reviewer.Store.UseOutfits(outfitReviewer)
	return outfitReviewer
}

// over is this reviewer working through narrowed, a photo review's
// reviewer narrowed to one scope.
func (reviewer *Reviewer) over(narrowed *review.Reviewer) *Reviewer {
	copied := *reviewer
	copied.Reviewer = narrowed
	copied.Store = &Store{Store: narrowed.Store}
	return &copied
}

// Narrowed is the reviewer working on scope's records.
func (reviewer *Reviewer) Narrowed(ctx context.Context, scope ownership.Scope) (*Reviewer, error) {
	store, err := reviewer.Reviewer.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	return reviewer.over(reviewer.Reviewer.Narrowed(store)), nil
}

// NarrowedToOwner is the reviewer working on one owner's records.
func (reviewer *Reviewer) NarrowedToOwner(ctx context.Context, ownerPrincipalID string) (*Reviewer, error) {
	return reviewer.Narrowed(ctx, ownership.OwnedBy(ownerPrincipalID))
}
