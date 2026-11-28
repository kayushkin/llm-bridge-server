package outfits

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

func TestAnOutfitHoldsLiveWearablesOnly(t *testing.T) {
	store := storeOver(possessionstest.OpenTestStore(t))
	ctx := context.Background()
	shirt, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"shirt","garment":{"category":"top","garment_type":"t_shirt"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"dive watch","watch":{"movement":"automatic","dial_color":"green"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	lamp, _ := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"lamp","art_and_decor":{"decor_type":"lamp"}}`), "principal_000001")
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
	if _, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{lamp["id"].(string)}}, "principal_000001"); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("an outfit with a lamp = %v, want invalid", err)
	}
	if err := store.DeletePossession(shirtID, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOutfit(ctx, OutfitInput{Status: &status, PossessionIDs: []string{shirtID}}, "principal_000001"); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("an outfit of a deleted shirt = %v, want invalid", err)
	}
}

func TestAnOutfitReadingRecordsTheOutfitAndFindsItAgain(t *testing.T) {
	store := storeOver(possessionstest.OpenTestStore(t))
	ctx := context.Background()
	watch, err := store.CreatePossession(ctx, possessionstest.Fields(t, `{"name":"dive watch","watch":{"movement":"automatic"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	watchID := watch["id"].(string)
	hoodieItem := item("hoodie", "gray hoodie", "", "garment", possessions.Box{Left: 0.3, Top: 0.3, Right: 0.7, Bottom: 0.7}, review.FieldValue{Name: "category", Value: "top"}, review.FieldValue{Name: "garment_type", Value: "hoodie"})
	watchItem := item("watch", "dive watch", "", "watch", possessions.Box{Left: 0.6, Top: 0.6, Right: 0.65, Bottom: 0.65}, review.FieldValue{Name: "movement", Value: "automatic"})
	shoesItem := item("shoes", "white sneakers", "", "garment", possessions.Box{Left: 0.4, Top: 0.9, Right: 0.6, Bottom: 1.0}, review.FieldValue{Name: "category", Value: "footwear"})
	lampItem := item("lamp", "lamp", "", "art_and_decor", possessions.Box{Left: 0.0, Top: 0.0, Right: 0.1, Bottom: 0.1}, review.FieldValue{Name: "decor_type", Value: "lamp"})
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{
		"first 1": review.FinalReading{Items: []review.FinalItem{
			{ReaderItem: hoodieItem},
			{ReaderItem: watchItem, ExistingID: watchID},
			{ReaderItem: shoesItem, Doubt: "Are these the Adidas or another pair?"},
			{ReaderItem: lampItem},
		}},
	}}
	reviewer := NewReviewer(&review.Reviewer{Store: store.Store, Models: scripted, Photos: possessionstest.FakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}, ReviewerSettings{OwnerLocation: possessionstest.MustLocation(t, "America/Los_Angeles")})
	if _, _, err := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", ""); !errors.Is(err, possessions.ErrInvalid) {
		t.Fatalf("an outfit reading with no wearer = %v, want invalid", err)
	}
	first, _, err := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", "person_000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.Review(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	photoReview, _ := store.GetPhotoReview(first.ID)
	if photoReview.Status != "waiting_for_owner" || photoReview.OutfitID == nil || photoReview.WornOn != "2026-11-06" || len(photoReview.Sightings) != 3 {
		t.Fatalf("review = %s, outfit %v, worn %q, %d sightings (the lamp is not worn)", photoReview.Status, photoReview.OutfitID, photoReview.WornOn, len(photoReview.Sightings))
	}
	outfit, _ := store.GetOutfit(*photoReview.OutfitID, false)
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
	if _, err := reviewer.AnswerQuestion(ctx, details[0].ID, review.Answer{Choice: &yes, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := store.GetPossession(hoodieID, false); confirmed["status"] != "owned" {
		t.Fatalf("after 'yes, I still have it': %v", confirmed["status"])
	}
	if _, err := reviewer.AnswerQuestion(ctx, decisions[0].ID, review.Answer{Choice: &yes, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	outfit, _ = store.GetOutfit(*photoReview.OutfitID, false)
	if len(outfit.PossessionIDs) != 3 {
		t.Fatalf("the answered sneakers should join the outfit: %+v", outfit.PossessionIDs)
	}
	shoesID := outfit.PossessionIDs[2]

	scripted.Answers["first 1"] = review.FinalReading{Items: []review.FinalItem{
		{ReaderItem: hoodieItem, ExistingID: hoodieID}, {ReaderItem: watchItem, ExistingID: watchID}, {ReaderItem: shoesItem, ExistingID: shoesID},
	}}
	scripted.Respond = matchByLabel(map[string]string{"hoodie": hoodieID, "watch": watchID, "shoes": shoesID})
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
	store := storeOver(possessionstest.OpenTestStore(t))
	ctx := context.Background()
	necklace := item("necklace", "brown bead necklace", "", "jewelry", possessions.Box{Left: 0.4, Top: 0.4, Right: 0.6, Bottom: 0.5}, review.FieldValue{Name: "jewelry_type", Value: "necklace"})
	belt := item("belt", "black belt", "", "garment", possessions.Box{Left: 0.4, Top: 0.6, Right: 0.6, Bottom: 0.65}, review.FieldValue{Name: "category", Value: "accessory"}, review.FieldValue{Name: "garment_type", Value: "belt"})
	ring := item("ring", "silver ring", "", "jewelry", possessions.Box{Left: 0.7, Top: 0.5, Right: 0.72, Bottom: 0.52}, review.FieldValue{Name: "jewelry_type", Value: "ring"})
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{
		"first 1": review.FinalReading{Items: []review.FinalItem{
			{ReaderItem: necklace, Doubt: "Is the necklace wood or another material?"},
			{ReaderItem: belt, Doubt: "Is this your black belt or another one?"},
			{ReaderItem: ring, Doubt: "Is the ring sterling silver?", DoubtKind: "detail"},
		}},
	}}
	reviewer := NewReviewer(&review.Reviewer{Store: store.Store, Models: scripted, Photos: possessionstest.FakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}, ReviewerSettings{OwnerLocation: possessionstest.MustLocation(t, "America/Los_Angeles")})
	photoReview, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "outfit", "person_000001")
	if err := reviewer.Review(ctx, photoReview.ID); err != nil {
		t.Fatal(err)
	}
	decisions, _ := store.ListQuestions(false, photoReview.ID, "decision", 0, 0)
	if len(decisions) != 2 {
		t.Fatalf("an unlabeled doubt is a decision and a labeled detail is not: %d decisions", len(decisions))
	}
	recorded, _ := store.GetPhotoReview(photoReview.ID)
	if recorded.OutfitID == nil {
		t.Fatalf("the ring, a labeled detail, should already be in an outfit")
	}
	necklaceQuestion, beltQuestion := decisions[0].ID, decisions[1].ID
	scripted.Answers["first sort"] = map[string]any{"items": []map[string]string{
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

// storeOver is the outfits' store over a test store.
func storeOver(records *possessions.Store) *Store {
	return &Store{Store: review.NewStore(records)}
}

func item(label, name, brand, kind string, box possessions.Box, kindFields ...review.FieldValue) review.ReaderItem {
	return review.ReaderItem{Label: label, Name: name, Brand: brand, Kind: kind, Box: box, Quantity: 1, KindFields: kindFields, Confidence: 0.9}
}

func mustSighting(t *testing.T, store *Store, id string) review.Sighting {
	t.Helper()
	sighting, err := store.GetSighting(id)
	if err != nil {
		t.Fatal(err)
	}
	return sighting
}
