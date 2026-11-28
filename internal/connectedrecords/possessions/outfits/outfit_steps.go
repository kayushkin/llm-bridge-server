package outfits

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

// Reviewer carries out the photo review's OutfitSteps (NewReviewer wires
// it): each step works through the review's store or reviewer it is handed,
// already narrowed to the scope the step runs in.

// ReviewOutfit runs an outfit reading of one photo to its end.
func (root *Reviewer) ReviewOutfit(ctx context.Context, reviewer *review.Reviewer, photoReview review.PhotoReview) error {
	return root.over(reviewer).reviewOutfit(ctx, photoReview)
}

// AddToReviewOutfit puts an accepted piece of an outfit reading in that
// reading's outfit.
func (root *Reviewer) AddToReviewOutfit(store *review.Store, transaction *possessions.DatabaseTransaction, sightingID string, now int64) error {
	return (&Store{Store: store}).addToReviewOutfit(transaction, sightingID, now)
}

// ApplySlotAnswer relinks an outfit slot as the owner answered.
func (root *Reviewer) ApplySlotAnswer(store *review.Store, transaction *possessions.DatabaseTransaction, resolution review.Resolution, actor string, now int64) error {
	return (&Store{Store: store}).applySlotAnswer(transaction, resolution, actor, now)
}

// CombineOutfits folds one outfit into another, and answers the merge's id.
func (root *Reviewer) CombineOutfits(store *review.Store, transaction *possessions.DatabaseTransaction, duplicateOutfitID, keptOutfitID, actor, reason string, now int64) (string, error) {
	return (&Store{Store: store}).combineOutfitsInTransaction(transaction, duplicateOutfitID, keptOutfitID, actor, reason, now)
}
