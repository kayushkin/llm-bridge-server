package possessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
)

func TestGroupsAreCountedAsTheOwnerCountsThem(t *testing.T) {
	store := possessionstest.OpenAuditStore(t)
	ctx := context.Background()
	for fieldsText, want := range map[string]string{
		`{"name":"a","jewelry":{"jewelry_type":"pendant"}}`:                      "jewelry:necklace",
		`{"name":"b","jewelry":{"jewelry_type":"chain"}}`:                        "jewelry:necklace",
		`{"name":"c","garment":{"category":"accessory","garment_type":"scarf"}}`: "garment:scarf",
		`{"name":"d","garment":{"category":"accessory","garment_type":"socks"}}`: "",
		`{"name":"e","garment":{"category":"bottom","garment_type":"jeans"}}`:    "garment:bottom",
	} {
		made, err := store.CreatePossession(ctx, possessionstest.Fields(t, fieldsText), "principal_000001")
		if err != nil {
			t.Fatal(err)
		}
		if got := possessions.PossessionGroup(made); got != want {
			t.Fatalf("%s is in %q, want %q", fieldsText, got, want)
		}
	}
	low, high := 3, 9
	group, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", possessions.WearableGroupChange{ExpectedLow: &low, ExpectedHigh: &high, Actor: "principal_000001"})
	if err != nil || group.ExpectedLow == nil || *group.ExpectedLow != 3 || *group.ExpectedHigh != 9 || group.Members != 2 || group.Closed {
		t.Fatalf("group = %+v, %v", group, err)
	}
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:pendant", possessions.WearableGroupChange{ExpectedHigh: &high, Actor: "principal_000001"}); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("pendants are necklaces, not a group: %v", err)
	}
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", possessions.WearableGroupChange{ExpectedLow: &high, ExpectedHigh: &low, Actor: "principal_000001"}); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("a range upside down: %v", err)
	}
	groups, _ := store.ListWearableGroups()
	for _, listed := range groups {
		if listed.Group == "" {
			t.Fatalf("socks should be in no group: %+v", groups)
		}
	}
}
