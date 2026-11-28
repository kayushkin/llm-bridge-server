package possessions

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAnOutfitHoldsLiveWearablesOnly(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	shirt, err := store.CreatePossession(ctx, fields(t, `{"name":"shirt","garment":{"category":"top","garment_type":"t_shirt"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreatePossession(ctx, fields(t, `{"name":"dive watch","watch":{"movement":"automatic","dial_color":"green"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	lamp, _ := store.CreatePossession(ctx, fields(t, `{"name":"lamp","art_and_decor":{"decor_type":"lamp"}}`), "principal_000001")
	shirtID, watchID := shirt["id"].(string), watch["id"].(string)
	status := "kept"
	outfit, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{shirtID, watchID}}, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if outfit.ID != "outfit_000001" || len(outfit.PossessionIDs) != 2 {
		t.Fatalf("outfit = %+v", outfit)
	}
	if _, err := store.RecordWearing(outfit.ID, "2026-11-06"); err != nil {
		t.Fatal(err)
	}
	view, _ := store.GetPossession(watchID, false)
	if ids := view["outfit_ids"].([]string); len(ids) != 1 || ids[0] != outfit.ID {
		t.Fatalf("the watch's outfits = %v", ids)
	}
	if _, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{lamp["id"].(string)}}, "principal_000001"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an outfit with a lamp = %v, want invalid", err)
	}
	if err := store.DeletePossession(shirtID, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{shirtID}}, "principal_000001"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an outfit of a deleted shirt = %v, want invalid", err)
	}
}

func TestAnOutfitReadingRecordsTheOutfitAndFindsItAgain(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	watch, err := store.CreatePossession(ctx, fields(t, `{"name":"dive watch","watch":{"movement":"automatic"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	watchID := watch["id"].(string)
	hoodieItem := item("hoodie", "gray hoodie", "", "garment", Box{0.3, 0.3, 0.7, 0.7}, fieldValue{Name: "category", Value: "top"}, fieldValue{Name: "garment_type", Value: "hoodie"})
	watchItem := item("watch", "dive watch", "", "watch", Box{0.6, 0.6, 0.65, 0.65}, fieldValue{Name: "movement", Value: "automatic"})
	shoesItem := item("shoes", "white sneakers", "", "garment", Box{0.4, 0.9, 0.6, 1.0}, fieldValue{Name: "category", Value: "footwear"})
	lampItem := item("lamp", "lamp", "", "art_and_decor", Box{0.0, 0.0, 0.1, 0.1}, fieldValue{Name: "decor_type", Value: "lamp"})
	scripted := &scriptedModels{answers: map[string]any{
		"first 1": finalReading{Items: []finalItem{
			{readerItem: hoodieItem},
			{readerItem: watchItem, ExistingID: watchID},
			{readerItem: shoesItem, Doubt: "Are these the Adidas or another pair?"},
			{readerItem: lampItem},
		}},
	}}
	reviewer := &Reviewer{Store: store, Models: scripted, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second", OwnerLocation: mustLocation(t, "America/Los_Angeles")}
	if _, _, err := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an outfit reading with no wearer = %v, want invalid", err)
	}
	first, _, err := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", "person_000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.Review(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	review, _ := store.GetPhotoReview(first.ID)
	if review.Status != "waiting_for_owner" || review.OutfitID == nil || review.WornOn != "2026-11-06" || len(review.Sightings) != 3 {
		t.Fatalf("review = %s, outfit %v, worn %q, %d sightings (the lamp is not worn)", review.Status, review.OutfitID, review.WornOn, len(review.Sightings))
	}
	outfit, _ := store.GetOutfit(*review.OutfitID, false)
	if len(outfit.PossessionIDs) != 2 || outfit.PossessionIDs[1] != watchID || len(outfit.WornOn) != 1 || outfit.WornOn[0] != "2026-11-06" || len(outfit.PhotoIDs) != 1 || outfit.PhotoIDs[0] != "photo_000001" {
		t.Fatalf("outfit = %+v", outfit)
	}
	hoodieID := outfit.PossessionIDs[0]
	hoodie, _ := store.GetPossession(hoodieID, false)
	if hoodie["status"] != "unconfirmed" {
		t.Fatalf("a piece seen only in a photo should start unconfirmed: %v", hoodie["status"])
	}
	decisions, _ := store.ListQuestions(false, first.ID, "decision", 0, 0)
	details, _ := store.ListQuestions(false, first.ID, "detail", 0, 0)
	if len(decisions) != 1 || len(details) != 1 || !strings.Contains(details[0].Question, "still have the gray hoodie") {
		t.Fatalf("decisions %+v, details %+v", decisions, details)
	}
	yes := 0
	if _, err := reviewer.AnswerQuestion(ctx, details[0].ID, Answer{Choice: &yes, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := store.GetPossession(hoodieID, false); confirmed["status"] != "owned" {
		t.Fatalf("after 'yes, I still have it': %v", confirmed["status"])
	}
	if _, err := reviewer.AnswerQuestion(ctx, decisions[0].ID, Answer{Choice: &yes, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	outfit, _ = store.GetOutfit(*review.OutfitID, false)
	if len(outfit.PossessionIDs) != 3 {
		t.Fatalf("the answered sneakers should join the outfit: %+v", outfit.PossessionIDs)
	}
	shoesID := outfit.PossessionIDs[2]

	scripted.answers["first 1"] = finalReading{Items: []finalItem{
		{readerItem: hoodieItem, ExistingID: hoodieID}, {readerItem: watchItem, ExistingID: watchID}, {readerItem: shoesItem, ExistingID: shoesID},
	}}
	scripted.respond = matchByLabel(map[string]string{"hoodie": hoodieID, "watch": watchID, "shoes": shoesID})
	second, _, _ := store.QueuePhotoReview(ctx, "photo_000002", "principal_000001", "outfit", "person_000001")
	if err := reviewer.Review(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := store.GetPhotoReview(second.ID)
	if again.Status != "done" || again.OutfitID == nil || *again.OutfitID != outfit.ID {
		t.Fatalf("the same three pieces should find outfit %s, got %v (%s)", outfit.ID, again.OutfitID, again.Status)
	}
	if final, _ := store.GetOutfit(outfit.ID, false); len(final.PhotoIDs) != 2 {
		t.Fatalf("both selfies should be the outfit's photos: %v", final.PhotoIDs)
	}
	if outfits, _ := store.ListOutfits("", "", 0, 0); len(outfits) != 1 {
		t.Fatalf("outfits = %d, want one", len(outfits))
	}
}

// matchByLabel answers the picture match with the id given for the piece's
// label, both readers alike.
func matchByLabel(ids map[string]string) func(role, step, prompt string, pictures int) (any, bool) {
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "match" {
			return nil, false
		}
		for label, id := range ids {
			if strings.Contains(prompt, "in a new photo: "+label+" (") {
				return map[string]string{"match": id, "reason": "same marks"}, true
			}
		}
		return map[string]string{"match": "none", "reason": "not shown"}, true
	}
}

func TestDoubtsAboutOnlyADetailRecordThePieceAndAskLater(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	necklace := item("necklace", "brown bead necklace", "", "jewelry", Box{0.4, 0.4, 0.6, 0.5}, fieldValue{Name: "jewelry_type", Value: "necklace"})
	belt := item("belt", "black belt", "", "garment", Box{0.4, 0.6, 0.6, 0.65}, fieldValue{Name: "category", Value: "accessory"}, fieldValue{Name: "garment_type", Value: "belt"})
	ring := item("ring", "silver ring", "", "jewelry", Box{0.7, 0.5, 0.72, 0.52}, fieldValue{Name: "jewelry_type", Value: "ring"})
	scripted := &scriptedModels{answers: map[string]any{
		"first 1": finalReading{Items: []finalItem{
			{readerItem: necklace, Doubt: "Is the necklace wood or another material?"},
			{readerItem: belt, Doubt: "Is this your black belt or another one?"},
			{readerItem: ring, Doubt: "Is the ring sterling silver?", DoubtKind: "detail"},
		}},
	}}
	reviewer := &Reviewer{Store: store, Models: scripted, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second", OwnerLocation: mustLocation(t, "America/Los_Angeles")}
	review, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", "person_000001")
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	decisions, _ := store.ListQuestions(false, review.ID, "decision", 0, 0)
	if len(decisions) != 2 {
		t.Fatalf("an unlabeled doubt is a decision and a labeled detail is not: %d decisions", len(decisions))
	}
	recorded, _ := store.GetPhotoReview(review.ID)
	if recorded.OutfitID == nil {
		t.Fatalf("the ring, a labeled detail, should already be in an outfit")
	}
	necklaceQuestion, beltQuestion := decisions[0].ID, decisions[1].ID
	scripted.answers["first sort"] = map[string]any{"items": []map[string]string{
		{"question_id": necklaceQuestion, "about": "detail"}, {"question_id": beltQuestion, "about": "identity"},
	}}
	result, err := reviewer.TriageOutfitDoubts(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if result.Looked != 2 || result.Detail != 1 || result.Identity != 1 || len(result.Failures) != 0 {
		t.Fatalf("triage = %+v", result)
	}
	if question, _ := store.GetQuestion(necklaceQuestion); question.Purpose != "detail" {
		t.Fatalf("the necklace question should be a detail now: %s", question.Purpose)
	}
	if question, _ := store.GetQuestion(beltQuestion); question.Purpose != "decision" {
		t.Fatalf("the belt question should stay a decision: %s", question.Purpose)
	}
	outfit, _ := store.GetOutfit(*recorded.OutfitID, false)
	if len(outfit.PossessionIDs) != 2 {
		t.Fatalf("the outfit should hold the ring and the necklace: %v", outfit.PossessionIDs)
	}
	again, _ := reviewer.TriageOutfitDoubts(ctx, "principal_000001")
	if again.Detail != 0 {
		t.Fatalf("a second pass recorded %d more", again.Detail)
	}
}
