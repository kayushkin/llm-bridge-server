package possessions

import "context"

// outfitSteps carries out the review's OutfitSteps with the outfit code.
type outfitSteps struct{}

func (outfitSteps) ReviewOutfit(ctx context.Context, reviewer *Reviewer, review PhotoReview) error {
	return reviewer.reviewOutfit(ctx, review)
}

func (outfitSteps) AddToReviewOutfit(store *Store, transaction *databaseTransaction, sightingID string, now int64) error {
	return store.addToReviewOutfit(transaction, sightingID, now)
}

func (outfitSteps) ApplySlotAnswer(store *Store, transaction *databaseTransaction, resolution Resolution, actor string, now int64) error {
	return store.applySlotAnswer(transaction, resolution, actor, now)
}

func (outfitSteps) CombineOutfits(store *Store, transaction *databaseTransaction, duplicateOutfitID, keptOutfitID, actor, reason string, now int64) (string, error) {
	return store.combineOutfitsInTransaction(transaction, duplicateOutfitID, keptOutfitID, actor, reason, now)
}
