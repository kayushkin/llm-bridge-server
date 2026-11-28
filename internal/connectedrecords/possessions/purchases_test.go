package possessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
)

func TestDeletingAPurchaseWithdrawsItsQuestionsAndRefusesWhileAPossessionCameFromIt(t *testing.T) {
	store := possessionstest.OpenProductTestStore(t)
	ctx := context.Background()
	purchase, err := store.CreatePurchase(ctx, possessions.Purchase{Marketplace: "ebay", OrderReference: "1", OrderedAt: 100, Source: "manual", Lines: []possessions.PurchaseLine{{Title: "Lamp"}}})
	if err != nil {
		t.Fatal(err)
	}
	lamp, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"desk lamp","purchase_line_id":"`+purchase.Lines[0].ID+`"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeletePurchase(ctx, purchase.ID, "principal_000001", "moved"); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("deleting a purchase a possession came from = %v, want a conflict", err)
	}
	if _, err := store.PatchPossession(ctx, lamp["id"].(string), possessionstest.Fields(t, `{"purchase_line_id":null}`), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeletePurchase(ctx, purchase.ID, "principal_000001", "moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPurchase(purchase.ID); !errors.Is(err, possessions.ErrNotFound) {
		t.Fatalf("a deleted purchase read = %v, want not found", err)
	}
}
