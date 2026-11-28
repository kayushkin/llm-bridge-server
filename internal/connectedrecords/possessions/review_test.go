package possessions

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAProposalKeepsOnlyFieldsThatFitAndSaysWhatItDropped(t *testing.T) {
	proposal := normalizeProposal(readerItem{Label: "hoodie on chair", Name: "hoodie", Kind: "garment", Quantity: 0, KindFields: []fieldValue{
		{Name: "category", Value: "top"}, {Name: "primary_color", Value: "maroon"}, {Name: "seasons", Value: "fall, winter"}, {Name: "warmth", Value: "warm"}, {Name: "sleeves", Value: "long"},
	}})
	if proposal.Kind != "garment" || proposal.Quantity != 1 {
		t.Fatalf("proposal = %+v", proposal)
	}
	if string(proposal.KindFields["seasons"]) != `["fall","winter"]` || proposal.KindFields["primary_color"] != nil {
		t.Fatalf("kind fields = %v", proposal.KindFields)
	}
	if len(proposal.DroppedFields) != 3 {
		t.Fatalf("dropped = %v, want maroon, warm and sleeves", proposal.DroppedFields)
	}
	noCategory := normalizeProposal(readerItem{Name: "hat", Kind: "garment", KindFields: []fieldValue{{Name: "primary_color", Value: "red"}}})
	if noCategory.Kind != "" || len(noCategory.KindFields) != 0 {
		t.Fatalf("a garment with no category = %+v, want no kind", noCategory)
	}
}

func item(label, name, brand, kind string, box Box, kindFields ...fieldValue) readerItem {
	return readerItem{Label: label, Name: name, Brand: brand, Kind: kind, Box: box, Quantity: 1, KindFields: kindFields, Confidence: 0.9}
}

func TestAReviewRecordsWhatBothReadersAgreeOnAndAsksAboutTheRest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	lamp, err := store.CreatePossession(ctx, fields(t, `{"name":"desk lamp"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	hoodieBox := Box{0.1, 0.1, 0.4, 0.5}
	lampBox := Box{0.6, 0.1, 0.8, 0.4}
	guitarBox := Box{0.5, 0.5, 0.9, 0.95}
	mugBox := Box{0.0, 0.8, 0.1, 0.95}
	category := fieldValue{Name: "category", Value: "top"}
	final := func(base readerItem, from []string, existing, doubt string) finalItem {
		return finalItem{readerItem: base, FromReadings: from, ExistingID: existing, Doubt: doubt}
	}
	models := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{item("hoodie", "black hoodie", "", "garment", hoodieBox, category), item("lamp", "lamp", "", "none", lampBox), item("guitar", "guitar", "Fender", "musical_instrument", guitarBox)}},
		"second 1": firstReading{Items: []readerItem{item("hoodie", "hoodie", "", "garment", hoodieBox, category), item("lamp", "lamp", "", "none", lampBox), item("guitar", "guitar", "Squier", "musical_instrument", guitarBox), item("mug", "mug", "", "none", mugBox)}},
		"first 2": finalReading{Items: []finalItem{
			final(item("hoodie", "black hoodie", "", "garment", hoodieBox, category, fieldValue{Name: "primary_color", Value: "black"}), []string{"A1", "B1"}, "", ""),
			final(item("lamp", "desk lamp", "", "none", lampBox), []string{"A2", "B2"}, lamp["id"].(string), ""),
			final(item("guitar", "electric guitar", "Fender", "musical_instrument", guitarBox), []string{"A3", "B3"}, "", ""),
		}},
		"second 2": finalReading{Items: []finalItem{
			final(item("hoodie", "hoodie", "", "garment", hoodieBox, category), []string{"A1", "B1"}, "", ""),
			final(item("lamp", "lamp", "", "none", lampBox), []string{"A2", "B2"}, lamp["id"].(string), ""),
			final(item("guitar", "guitar", "Squier", "musical_instrument", guitarBox), []string{"A3", "B3"}, "", ""),
			final(item("mug", "mug", "", "none", mugBox), []string{"B4"}, "", ""),
		}},
	}}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	review, created, err := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err != nil || !created {
		t.Fatalf("queue = %v %v", created, err)
	}
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	done, err := store.GetPhotoReview(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "waiting_for_owner" || len(done.Readings) != 4 || len(done.Sightings) != 4 {
		t.Fatalf("review = %s, %d readings, %d sightings", done.Status, len(done.Readings), len(done.Sightings))
	}
	if done.CostUSD < 0.039 || done.CostUSD > 0.041 {
		t.Fatalf("cost = %v, want the four readings' 0.04", done.CostUSD)
	}
	hoodies, _ := store.ListPossessions(PossessionFilter{Kind: "garment"})
	if len(hoodies) != 1 || hoodies[0]["name"] != "black hoodie" || hoodies[0]["created_by"] != review.ID {
		t.Fatalf("the agreed hoodie = %v", hoodies)
	}
	if hoodies[0]["garment"].(map[string]any)["primary_color"] != "black" {
		t.Fatalf("the hoodie should take the color one reader gave and the other left out: %v", hoodies[0]["garment"])
	}
	lampView, _ := store.GetPossession(lamp["id"].(string), false)
	if photos := lampView["photos"].([]PhotoLink); len(photos) != 1 || photos[0].PhotoID != "photo_000001" {
		t.Fatalf("the lamp both readers matched should gain the photo: %v", photos)
	}
	questions, err := store.ListQuestions(false, review.ID, "decision", 0, 0)
	if err != nil || len(questions) != 2 {
		t.Fatalf("questions = %d %v, want the guitar's brand and the mug only one reader saw", len(questions), err)
	}
	if !strings.Contains(questions[0].Question, "Fender") || !strings.Contains(questions[0].Question, "Squier") {
		t.Fatalf("the guitar question = %q", questions[0].Question)
	}
	second := 1
	if _, err := reviewer.AnswerQuestion(ctx, questions[0].ID, Answer{Choice: &second, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	guitars, _ := store.ListPossessions(PossessionFilter{Kind: "musical_instrument"})
	if len(guitars) != 1 || guitars[0]["brand"] != "Squier" {
		t.Fatalf("after choosing the second reader's version: %v", guitars)
	}
	if _, err := reviewer.AnswerQuestion(ctx, questions[0].ID, Answer{Choice: &second, AnsweredBy: "principal_000001"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("answering twice = %v, want a conflict", err)
	}
	reject := 1
	if _, err := reviewer.AnswerQuestion(ctx, questions[1].ID, Answer{Choice: &reject, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	finished, _ := store.GetPhotoReview(review.ID)
	if finished.Status != "done" {
		t.Fatalf("with every question answered the review = %s, want done", finished.Status)
	}
}

func TestASightingWaitingOnAnEarlierOneIsSettledWithIt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	box := Box{0.2, 0.2, 0.6, 0.6}
	jacket := func(brand string) readerItem { return item("jacket", "jacket", brand, "none", box) }
	firstModels := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{jacket("")}},
		"second 1": firstReading{Items: []readerItem{jacket("")}},
		"first 2":  finalReading{Items: []finalItem{{readerItem: jacket("Patagonia"), FromReadings: []string{"A1", "B1"}}}},
		"second 2": finalReading{Items: []finalItem{{readerItem: jacket("North Face"), FromReadings: []string{"A1", "B1"}}}},
	}}
	reviewer := &Reviewer{Store: store, Models: firstModels, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	firstReview, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, firstReview.ID); err != nil {
		t.Fatal(err)
	}
	firstSighting := "sighting_000001"
	reviewer.Models = &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{jacket("")}},
		"second 1": firstReading{Items: []readerItem{jacket("")}},
		"first 2":  finalReading{Items: []finalItem{{readerItem: jacket(""), FromReadings: []string{"A1", "B1"}, ExistingID: firstSighting}}},
		"second 2": finalReading{Items: []finalItem{{readerItem: jacket(""), FromReadings: []string{"A1", "B1"}, ExistingID: firstSighting}}},
	}}
	secondReview, _, _ := store.QueuePhotoReview(ctx, "photo_000002", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, secondReview.ID); err != nil {
		t.Fatal(err)
	}
	waiting, _ := store.GetSighting("sighting_000002")
	if waiting.Status != "same_as_other_sighting" || waiting.SameAsSightingID == nil || *waiting.SameAsSightingID != firstSighting {
		t.Fatalf("the second photo's jacket = %+v", waiting)
	}
	if len(firstModels.prompts) < 3 || !strings.Contains(firstModels.prompts[2], "(nothing yet)") {
		t.Fatalf("the first review's round two should see an empty inventory")
	}
	questions, _ := store.ListQuestions(false, firstReview.ID, "", 0, 0)
	choice := 0
	if _, err := reviewer.AnswerQuestion(ctx, questions[0].ID, Answer{Choice: &choice, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	settled, _ := store.GetSighting("sighting_000002")
	if settled.Status != "accepted" || settled.PossessionID == nil {
		t.Fatalf("the waiting jacket after the owner answered = %+v", settled)
	}
	jacketView, _ := store.GetPossession(*settled.PossessionID, false)
	if photos := jacketView["photos"].([]PhotoLink); len(photos) != 2 {
		t.Fatalf("the jacket should have both photos, has %d", len(photos))
	}
}

func TestAnAnswerInWordsIsReadByTheFirstReader(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	box := Box{0.2, 0.2, 0.6, 0.6}
	models := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{item("amp", "amplifier", "", "audio_device", box, fieldValue{Name: "device_type", Value: "amplifier"})}},
		"second 1": firstReading{},
		"first 2":  finalReading{Items: []finalItem{{readerItem: item("amp", "amplifier", "", "audio_device", box, fieldValue{Name: "device_type", Value: "amplifier"}), FromReadings: []string{"A1"}}}},
		"second 2": finalReading{},
		"first interpret": map[string]any{"action": "accept", "possession_id": "", "sighting_id": "", "explanation": "a Boss Katana amp",
			"item": item("amp", "Boss Katana amp", "Boss", "audio_device", box, fieldValue{Name: "device_type", Value: "amplifier"})},
	}}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	review, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	questions, _ := store.ListQuestions(false, "", "", 0, 0)
	if len(questions) != 1 || !strings.HasPrefix(questions[0].Question, "Only first-model saw this") {
		t.Fatalf("questions = %+v", questions)
	}
	answered, err := reviewer.AnswerQuestion(ctx, questions[0].ID, Answer{Text: "yes, it's my Boss Katana", AnsweredBy: "principal_000001"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answered.Resolution), "Boss Katana") {
		t.Fatalf("resolution = %s", answered.Resolution)
	}
	amps, _ := store.ListPossessions(PossessionFilter{Kind: "audio_device"})
	if len(amps) != 1 || amps[0]["brand"] != "Boss" || amps[0]["created_by"] != "principal_000001" {
		t.Fatalf("amps = %v", amps)
	}
}

func TestAnAgreedThingWithADoubtIsRecordedAndItsAnswerAmendsIt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	box := Box{0.2, 0.2, 0.6, 0.6}
	mouse := item("mouse", "black mouse", "", "computer_part", box, fieldValue{Name: "part_type", Value: "mouse"})
	models := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{mouse}},
		"second 1": firstReading{Items: []readerItem{mouse}},
		"first 2":  finalReading{Items: []finalItem{{readerItem: mouse, FromReadings: []string{"A1", "B1"}, Doubt: "Which model of mouse is it?"}}},
		"second 2": finalReading{Items: []finalItem{{readerItem: mouse, FromReadings: []string{"A1", "B1"}}}},
		"first detail": map[string]any{"explanation": "a Logitech G Pro",
			"item": item("mouse", "Logitech G Pro mouse", "Logitech", "computer_part", box, fieldValue{Name: "part_type", Value: "mouse"})},
	}}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	review, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	done, _ := store.GetPhotoReview(review.ID)
	if done.Status != "done" || done.Sightings[0].Status != "accepted" {
		t.Fatalf("an agreed thing with only a detail doubt: review %s, sighting %s; want done and accepted", done.Status, done.Sightings[0].Status)
	}
	details, _ := store.ListQuestions(false, review.ID, "detail", 0, 0)
	if len(details) != 1 || !strings.HasPrefix(details[0].Question, "Recorded as") {
		t.Fatalf("detail questions = %+v", details)
	}
	if _, err := reviewer.AnswerQuestion(ctx, details[0].ID, Answer{Text: "it's a Logitech G Pro", AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	amended, _ := store.GetPossession(*done.Sightings[0].PossessionID, false)
	if amended["brand"] != "Logitech" || amended["name"] != "Logitech G Pro mouse" || amended["kind"] != "computer_part" {
		t.Fatalf("after the answer the mouse = %v", amended)
	}
}

func TestAgreedSightingsHeldBackBeforeAreRecordedOnce(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	box := Box{0.2, 0.2, 0.6, 0.6}
	models := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{item("lamp", "lamp", "", "none", box)}},
		"second 1": firstReading{Items: []readerItem{item("lamp", "lamp", "", "none", box)}},
		"first 2":  finalReading{Items: []finalItem{{readerItem: item("lamp", "lamp", "", "none", box), FromReadings: []string{"A1", "B1"}}}},
		"second 2": finalReading{Items: []finalItem{{readerItem: item("lamp", "lamp", "", "none", box), FromReadings: []string{"A1", "B1"}}}},
	}}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	review, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	// Put the store back as the old rule left it: agreed, unrecorded, asked.
	possessionID := *mustSighting(t, store, "sighting_000001").PossessionID
	for _, statement := range []string{
		`DELETE FROM possession_photos`, `UPDATE sightings SET status = 'proposed', possession_id = NULL, decided_by = '', decided_at = 0`,
		`DELETE FROM possessions WHERE id = '` + possessionID + `'`, `UPDATE photo_reviews SET status = 'waiting_for_owner'`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := store.db.Begin()
	if err := insertQuestion(tx, "sighting_000001", "Both readers agree this is lamp, but: which lamp?", []Choice{rejectChoice}, "decision", 1); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	result, err := store.RecordAgreedSightings(ctx, "principal_000001")
	if err != nil || result.Recorded != 1 || len(result.Failures) != 0 {
		t.Fatalf("record agreed = %+v %v", result, err)
	}
	again, _ := store.RecordAgreedSightings(ctx, "principal_000001")
	if again.Recorded != 0 {
		t.Fatalf("a second run recorded %d more", again.Recorded)
	}
	question, _ := store.GetQuestion("possession_question_000001")
	if question.Purpose != "detail" || question.Question != "Recorded as lamp. To add: which lamp?" {
		t.Fatalf("question = %+v", question)
	}
	finished, _ := store.GetPhotoReview(review.ID)
	if finished.Status != "done" || finished.Sightings[0].Status != "accepted" {
		t.Fatalf("review %s, sighting %s", finished.Status, finished.Sightings[0].Status)
	}
}

func mustSighting(t *testing.T, store *Store, id string) Sighting {
	t.Helper()
	sighting, err := store.GetSighting(id)
	if err != nil {
		t.Fatal(err)
	}
	return sighting
}

func TestClassifyingGivesKindsWhenSureAndAsksWhenNot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	box := Box{0.2, 0.2, 0.6, 0.6}
	scripted := &scriptedModels{answers: map[string]any{
		"first 1":  firstReading{Items: []readerItem{item("sofa", "yellow sofa", "", "none", box), item("vase", "blue vase", "", "none", Box{0.7, 0.7, 0.9, 0.9})}},
		"second 1": firstReading{Items: []readerItem{item("sofa", "yellow sofa", "", "none", box), item("vase", "blue vase", "", "none", Box{0.7, 0.7, 0.9, 0.9})}},
		"first 2": finalReading{Items: []finalItem{{readerItem: item("sofa", "yellow sofa", "", "none", box), FromReadings: []string{"A1", "B1"}},
			{readerItem: item("vase", "blue vase", "", "none", Box{0.7, 0.7, 0.9, 0.9}), FromReadings: []string{"A2", "B2"}}}},
		"second 2": finalReading{Items: []finalItem{{readerItem: item("sofa", "yellow sofa", "", "none", box), FromReadings: []string{"A1", "B1"}},
			{readerItem: item("vase", "blue vase", "", "none", Box{0.7, 0.7, 0.9, 0.9}), FromReadings: []string{"A2", "B2"}}}},
	}}
	reviewer := &Reviewer{Store: store, Models: scripted, Photos: fakePhotos{}, FirstReaderRole: "first", SecondReaderRole: "second"}
	review, _, _ := store.QueuePhotoReview(ctx, "photo_000001", "principal_000001", "inventory", "")
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	loose, _ := store.CreatePossession(ctx, fields(t, `{"name":"black box"}`), "principal_000001")
	scripted.answers["first classify"] = map[string]any{"items": []map[string]any{
		{"possession_id": "possession_000001", "kind": "furniture", "kind_fields": []fieldValue{{Name: "furniture_type", Value: "sofa"}, {Name: "primary_color", Value: "yellow"}}, "room": "living_room", "sure": true, "reason": "a sofa"},
		{"possession_id": "possession_000002", "kind": "art_and_decor", "kind_fields": []fieldValue{{Name: "decor_type", Value: "vase"}}, "room": "", "sure": false, "reason": "a vase or a lamp base"},
		{"possession_id": loose["id"].(string), "kind": "none", "kind_fields": []fieldValue{}, "room": "", "sure": false, "reason": "cannot tell"},
	}}
	run, err := reviewer.ClassifyKindless(ctx, "principal_000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Classified) != 1 || len(run.Asked) != 1 || len(run.Left) != 1 {
		t.Fatalf("run = %+v", run)
	}
	sofa, _ := store.GetPossession("possession_000001", false)
	if sofa["kind"] != "furniture" || sofa["room"] != "living_room" || sofa["name"] != "yellow sofa" {
		t.Fatalf("sofa = %v", sofa)
	}
	questions, _ := store.ListQuestions(false, "", "detail", 0, 0)
	if len(questions) != 1 || !strings.HasPrefix(questions[0].Choices[0].Label, "Yes: blue vase · art_and_decor") {
		t.Fatalf("detail questions = %+v", questions)
	}
	yes := 0
	if _, err := reviewer.AnswerQuestion(ctx, questions[0].ID, Answer{Choice: &yes, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	vase, _ := store.GetPossession("possession_000002", false)
	if vase["kind"] != "art_and_decor" || vase["quantity"] != int64(1) || vase["name"] != "blue vase" {
		t.Fatalf("vase after yes = %v", vase)
	}
}
