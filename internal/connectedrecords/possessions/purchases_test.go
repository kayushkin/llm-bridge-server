package possessions

import (
	"context"
	"errors"
	"testing"
)

func TestDeletingAPurchaseWithdrawsItsQuestionsAndRefusesWhileAPossessionCameFromIt(t *testing.T) {
	store := openProductTestStore(t)
	ctx := context.Background()
	purchase, err := store.CreatePurchase(ctx, Purchase{Marketplace: "ebay", OrderReference: "1", OrderedAt: 100, Source: "manual", Lines: []PurchaseLine{{Title: "Lamp"}}})
	if err != nil {
		t.Fatal(err)
	}
	lamp, err := store.CreatePossession(ctx, fields(t, `{"name":"desk lamp","purchase_line_id":"`+purchase.Lines[0].ID+`"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeletePurchase(ctx, purchase.ID, "principal_000001", "moved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting a purchase a possession came from = %v, want a conflict", err)
	}
	if _, err := store.PatchPossession(ctx, lamp["id"].(string), fields(t, `{"purchase_line_id":null}`), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeletePurchase(ctx, purchase.ID, "principal_000001", "moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPurchase(purchase.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted purchase read = %v, want not found", err)
	}
}
