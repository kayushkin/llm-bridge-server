package possessions

import (
	"context"
	"fmt"
	"testing"
)

func TestEveryPairOfALargeGroupIsSeenTogether(t *testing.T) {
	for _, size := range []int{2, 20, 21, 25, 47} {
		seen := map[memberPair]bool{}
		for _, call := range duplicateCheckCalls(size) {
			if len(call) > duplicateCheckPicturesPerCall {
				t.Fatalf("%d members: a call holds %d pictures", size, len(call))
			}
			for i := range call {
				for j := i + 1; j < len(call); j++ {
					seen[orderedPair(call[i], call[j])] = true
				}
			}
		}
		if want := size * (size - 1) / 2; len(seen) != want {
			t.Fatalf("%d members: %d pairs seen, want %d", size, len(seen), want)
		}
	}

	store := openAuditStore(t)
	ctx := context.Background()
	ids := []string{}
	for number := 1; number <= 23; number++ {
		made, err := store.CreatePossession(ctx, fields(t, fmt.Sprintf(`{"name":"white t-shirt %d","garment":{"category":"top","garment_type":"t_shirt"},"photos":[{"photo_id":"photo_%06d"}]}`, number, number)), "principal_000001")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, made["id"].(string))
	}
	maximum := 0
	scripted := &scriptedModels{answers: map[string]any{}}
	scripted.respond = clusterAnswers(map[string][][]string{"first": {{ids[0], ids[21]}}, "second": {{ids[0], ids[21]}}}, &maximum)
	reviewer := auditReviewer(t, store, scripted, nil)
	run, err := reviewer.CheckWearableDuplicates(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["merged"] != 1 || run.Counts["asked"] != 0 || maximum > duplicateCheckPicturesPerCall || scripted.calls["first duplicates"] != 3 {
		t.Fatalf("run = %+v, most pictures in a call %d, calls %v", run, maximum, scripted.calls)
	}
	if gone, _ := store.GetPossession(ids[21], true); gone["merged_into_id"] != ids[0] {
		t.Fatalf("two shirts in different chunks should merge: %v", gone["merged_into_id"])
	}
}
