package possessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// fakeReferences knows a fixed set of other stores' ids.
type fakeReferences struct{ known map[string]bool }

func (references fakeReferences) Check(ctx context.Context, ownerPrincipalID string, kind ReferenceKind, id string) error {
	if references.known[id] {
		return nil
	}
	return fmt.Errorf("%w: no %s %s", ErrInvalid, kind, id)
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStore(t, fakeReferences{known: map[string]bool{
		"photo_000001": true, "photo_000002": true, "m_test_machine": true, "place_000001": true,
	}})
}

func fields(t *testing.T, text string) map[string]json.RawMessage {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestAGarmentIsMadeReadAndChangedWithItsKind(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	created, err := store.CreatePossession(ctx, fields(t, `{"name":"black hoodie","brand":"Carhartt","place_id":"place_000001",
		"garment":{"category":"top","garment_type":"hoodie","primary_color":"black","seasons":["fall","winter"],"warmth":4},
		"photos":[{"photo_id":"photo_000001","box_left":0.1,"box_top":0.1,"box_right":0.5,"box_bottom":0.6}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if created["id"] != "possession_000001" || created["kind"] != "garment" || created["status"] != "owned" {
		t.Fatalf("created = %v", created)
	}
	garment := created["garment"].(map[string]any)
	if garment["id"] != "garment_000001" || garment["warmth"] != int64(4) || len(garment["seasons"].([]string)) != 2 {
		t.Fatalf("garment = %v", garment)
	}
	if created["cover_photo_id"] != "photo_000001" {
		t.Fatalf("the first photo should be the cover, got %v", created["cover_photo_id"])
	}
	patched, err := store.PatchPossession(ctx, "possession_000001", fields(t, `{"spot":"closet","garment":{"fit":"relaxed"}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if patched["spot"] != "closet" || patched["garment"].(map[string]any)["fit"] != "relaxed" || patched["garment"].(map[string]any)["primary_color"] != "black" {
		t.Fatalf("patched = %v", patched)
	}
	revisions, err := store.Revisions("possession_000001")
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions = %d %v, want the possession row and the garment row", len(revisions), err)
	}
	if _, err := store.PatchPossession(ctx, "possession_000001", fields(t, `{"computer":{"form_factor":"laptop"}}`), "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("giving a garment a second kind = %v, want a conflict", err)
	}
}

func TestAWriteRefusesWhatItCannotStore(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, body := range []string{
		`{"brand":"no name"}`,
		`{"name":"x","colour":"red"}`,
		`{"name":"x","garment":{"category":"top","primary_color":"maroon"}}`,
		`{"name":"x","garment":{"garment_type":"hoodie"}}`,
		`{"name":"x","garment":{"category":"top"},"computer":{"form_factor":"laptop"}}`,
		`{"name":"x","place_id":"place_999999"}`,
		`{"name":"x","price_cents":1000}`,
		`{"name":"x","computer":{"form_factor":"laptop","harness_machine_id":"m_unknown"}}`,
		`{"name":"x","photos":[{"photo_id":"photo_000001","box_left":0.5}]}`,
	} {
		if _, err := store.CreatePossession(ctx, fields(t, body), "principal_000001"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want invalid", body, err)
		}
	}
	if _, err := store.CreatePossession(ctx, fields(t, `{"name":"x"}`), ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("a write with no actor = %v, want invalid", err)
	}
}

func TestAComputerIsFoundByItsHarnessMachineAndHoldsItsParts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	computer, err := store.CreatePossession(ctx, fields(t, `{"name":"desktop","computer":{"form_factor":"desktop","harness_machine_id":"m_test_machine","memory_gigabytes":64}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	computerID := computer["computer"].(map[string]any)["id"].(string)
	if _, err := store.CreatePossession(ctx, fields(t, `{"name":"2 TB drive","computer_part":{"part_type":"storage_drive","installed_in_computer_id":"`+computerID+`","capacity_gigabytes":2000}}`), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	found, err := store.ComputerForHarnessMachine("m_test_machine")
	if err != nil {
		t.Fatal(err)
	}
	if found["id"] != computer["id"] || len(found["installed_part_ids"].([]string)) != 1 {
		t.Fatalf("found = %v", found)
	}
	if _, err := store.CreatePossession(ctx, fields(t, `{"name":"another","computer":{"form_factor":"laptop","harness_machine_id":"m_test_machine"}}`), "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second computer on one machine = %v, want a conflict", err)
	}
}

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

// scriptedModels answers each reader by role and round from fixed readings.
type scriptedModels struct {
	mutex   sync.Mutex
	answers map[string]any // "<role> 1", "<role> 2", "<role> interpret"
	// respond, when set, answers first: by role, step, the prompt and how
	// many pictures came with it; false falls through to answers.
	respond func(role, step, prompt string, pictures int) (any, bool)
	prompts []string
	calls   map[string]int // "<role> <step>" → how many times asked
}

func (models *scriptedModels) Ask(ctx context.Context, role, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) (ModelAnswer, error) {
	step := "1"
	switch {
	case strings.Contains(prompt, "This photo will be cut to show the owner's outfit"):
		step = "wearer_box"
	case strings.Contains(prompt, "Reader A, made by another company, listed the looks"):
		step = "check_looks"
	case strings.Contains(prompt, "List the looks:"):
		step = "looks"
	case strings.Contains(prompt, "Sort these pictures into the individual physical pieces"):
		step = "pieces"
	case strings.Contains(prompt, "inventory says show this same piece"):
		step = "links"
	case strings.Contains(prompt, "one physical piece recorded twice"):
		step = "duplicates"
	case strings.Contains(prompt, "may be one outfit recorded twice"):
		step = "outfits"
	case strings.Contains(prompt, "a piece the owner has on in "):
		step = "match"
	case strings.Contains(prompt, "Which choice do they mean?"):
		step = "choice"
	case strings.Contains(prompt, "round two"):
		step = "2"
	case strings.Contains(prompt, "answered in their own words"):
		step = "interpret"
	case strings.Contains(prompt, "Write the item as it should now read"):
		step = "detail"
	case strings.Contains(prompt, "have no kind yet"):
		step = "classify"
	case strings.Contains(prompt, "Say for each whether it is about identity"):
		step = "sort"
	}
	models.mutex.Lock()
	models.prompts = append(models.prompts, prompt)
	if models.calls == nil {
		models.calls = map[string]int{}
	}
	models.calls[role+" "+step]++
	models.mutex.Unlock()
	if models.respond != nil {
		if answer, handled := models.respond(role, step, prompt, len(images)); handled {
			encoded, _ := json.Marshal(answer)
			return ModelAnswer{Model: role + "-model", Parsed: encoded, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.01}, nil
		}
	}
	answer, known := models.answers[role+" "+step]
	if !known {
		return ModelAnswer{}, fmt.Errorf("no scripted answer for %s %s", role, step)
	}
	encoded, _ := json.Marshal(answer)
	return ModelAnswer{Model: role + "-model", Parsed: encoded, InputTokens: 1000, OutputTokens: 100, CostUSD: 0.01}, nil
}

type fakePhotos struct{}

func testJPEG() []byte {
	picture := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		for y := 0; y < 20; y++ {
			picture.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 12), 0, 255})
		}
	}
	var out bytes.Buffer
	_ = jpeg.Encode(&out, picture, nil)
	return out.Bytes()
}

func (fakePhotos) ReviewImage(ctx context.Context, photoID string) ([]byte, string, error) {
	return testJPEG(), "thumbnail", nil
}
func (fakePhotos) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	return testJPEG(), nil
}
func (fakePhotos) WearerFace(ctx context.Context, photoID, personID string) (Box, error) {
	return Box{0.4, 0.1, 0.6, 0.3}, nil
}
func (fakePhotos) DetailImage(ctx context.Context, photoID string) (image.Image, string, error) {
	decoded, _, err := image.Decode(bytes.NewReader(testJPEG()))
	return decoded, "thumbnail", err
}
func (fakePhotos) WhyACopy(ctx context.Context, photoID string) (string, error) {
	if photoID == "photo_000039" {
		return whyACopy(`C:\\photos\\Screenshot_20260913_093002_Gallery.jpg`, nil), nil
	}
	return "", nil
}

func (fakePhotos) TakenAt(ctx context.Context, photoID string) (int64, error) {
	return 1794010000, nil // 2026-11-07 00:06 UTC, still 2026-11-06 in Los Angeles
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

func TestUprightTurnsASidewaysPhoto(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 2, 1))
	picture.Set(0, 0, color.RGBA{255, 0, 0, 255})
	picture.Set(1, 0, color.RGBA{0, 0, 255, 255})
	turned := Upright(picture, 6)
	if turned.Bounds().Dx() != 1 || turned.Bounds().Dy() != 2 {
		t.Fatalf("bounds = %v", turned.Bounds())
	}
	if red, _, _, _ := turned.At(0, 0).RGBA(); red == 0 {
		t.Fatalf("orientation 6 should put the left pixel on top")
	}
}

func TestReadExifFindsTheDateOrientationAndPlace(t *testing.T) {
	file := jpegWithExif(t)
	facts, err := ReadExif(file)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Orientation != 6 || facts.TakenLocal != "2026:11:06 12:30:00" || facts.TakenOffset != "-08:00" {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.Latitude == nil || *facts.Latitude != 12.5 || *facts.Longitude != -45.25 {
		t.Fatalf("place = %v %v", facts.Latitude, facts.Longitude)
	}
	taken, ok, err := facts.TakenAt(nil)
	if err != nil || !ok || taken.UTC().Hour() != 20 {
		t.Fatalf("taken = %v %v %v", taken, ok, err)
	}
}

// jpegWithExif builds a little-endian EXIF block by hand: IFD0 with the
// orientation and pointers to the EXIF and GPS IFDs.
func jpegWithExif(t *testing.T) []byte {
	t.Helper()
	var tiff bytes.Buffer
	le := func(values ...any) {
		for _, value := range values {
			switch typed := value.(type) {
			case uint16:
				tiff.Write([]byte{byte(typed), byte(typed >> 8)})
			case uint32:
				tiff.Write([]byte{byte(typed), byte(typed >> 8), byte(typed >> 16), byte(typed >> 24)})
			}
		}
	}
	tiff.WriteString("II")
	le(uint16(42), uint32(8))
	// IFD0 at 8: three entries, then next-IFD 0. Ends at 8+2+36+4 = 50.
	le(uint16(3))
	le(uint16(0x0112), uint16(3), uint32(1), uint16(6), uint16(0))
	le(uint16(0x8769), uint16(4), uint32(1), uint32(50))
	le(uint16(0x8825), uint16(4), uint32(1), uint32(80))
	le(uint32(0))
	// EXIF IFD at 50: two entries; ends at 50+2+24+4 = 80; values after the GPS IFD.
	le(uint16(2))
	le(uint16(0x9003), uint16(2), uint32(20), uint32(200))
	le(uint16(0x9011), uint16(2), uint32(7), uint32(220))
	le(uint32(0))
	// GPS IFD at 80: four entries; ends at 80+2+48+4 = 134.
	le(uint16(4))
	le(uint16(1), uint16(2), uint32(2), uint32('N'))
	le(uint16(2), uint16(5), uint32(3), uint32(140))
	le(uint16(3), uint16(2), uint32(2), uint32('W'))
	le(uint16(4), uint16(5), uint32(3), uint32(164))
	le(uint32(0))
	for tiff.Len() < 140 {
		tiff.WriteByte(0)
	}
	le(uint32(12), uint32(1), uint32(30), uint32(1), uint32(0), uint32(1)) // 12°30'0" N
	le(uint32(45), uint32(1), uint32(15), uint32(1), uint32(0), uint32(1)) // 45°15'0" W
	for tiff.Len() < 200 {
		tiff.WriteByte(0)
	}
	tiff.WriteString("2026:11:06 12:30:00\x00")
	tiff.WriteString("-08:00\x00")
	segment := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	file := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(segment) + 2) >> 8), byte(len(segment) + 2)}
	file = append(file, segment...)
	return append(file, testJPEG()[2:]...)
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

func TestAMergeMovesPhotosKindAndLinksOntoTheKeptPossession(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	kept, err := store.CreatePossession(ctx, fields(t, `{"name":"GoXLR Mini","photos":[{"photo_id":"photo_000001"}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.CreatePossession(ctx, fields(t, `{"name":"streaming mixer","brand":"TC-Helicon","audio_device":{"device_type":"mixer"},
		"photos":[{"photo_id":"photo_000002","box_left":0.1,"box_top":0.1,"box_right":0.3,"box_bottom":0.3}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	keptID, duplicateID := kept["id"].(string), duplicate["id"].(string)
	merged, err := store.MergePossession(ctx, duplicateID, keptID, "principal_000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if merged["name"] != "GoXLR Mini" || merged["brand"] != "TC-Helicon" || merged["kind"] != "audio_device" || len(merged["photos"].([]PhotoLink)) != 2 || merged["cover_photo_id"] != "photo_000001" {
		t.Fatalf("merged = %v", merged)
	}
	if _, err := store.GetPossession(duplicateID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the duplicate should be gone from reads, got %v", err)
	}
	gone, _ := store.GetPossession(duplicateID, true)
	if gone["merged_into_id"] != keptID {
		t.Fatalf("the duplicate should say where it went: %v", gone["merged_into_id"])
	}
	if _, err := store.MergePossession(ctx, keptID, keptID, "principal_000001", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("merging into itself = %v", err)
	}
	laptop, _ := store.CreatePossession(ctx, fields(t, `{"name":"laptop","computer":{"form_factor":"laptop"}}`), "principal_000001")
	if _, err := store.MergePossession(ctx, laptop["id"].(string), keptID, "principal_000001", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("merging a computer into an audio device = %v, want a conflict", err)
	}
}

func TestEveryKindCanBeMade(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, kind := range KindTypes {
		required := ""
		for _, column := range kind.Columns {
			if !column.Required {
				continue
			}
			if column.Required {
				required = fmt.Sprintf(`"%s":%q`, column.Name, column.Vocabulary[0])
			}
		}
		made, err := store.CreatePossession(ctx, fields(t, `{"name":"x","room":"office","`+kind.Name+`":{`+required+`}}`), "principal_000001")
		if err != nil {
			t.Fatalf("%s: %v", kind.Name, err)
		}
		if made["kind"] != kind.Name || made["room"] != "office" {
			t.Fatalf("%s made as %v", kind.Name, made["kind"])
		}
	}
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

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func TestAPossessionMovesToAnotherKind(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	watch, _ := store.CreatePossession(ctx, fields(t, `{"name":"Seiko","garment":{"category":"accessory"}}`), "principal_000001")
	id := watch["id"].(string)
	moved, err := store.ChangeKind(ctx, id, "watch", fields(t, `{"movement":"quartz"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if moved["kind"] != "watch" || moved["garment"] != nil || moved["watch"].(map[string]any)["movement"] != "quartz" {
		t.Fatalf("moved = %v", moved)
	}
	if _, err := store.ChangeKind(ctx, id, "watch", nil, "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("moving to the kind it has = %v", err)
	}
	if _, err := store.ChangeKind(ctx, id, "jewelry", fields(t, `{}`), "principal_000001"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a kind with its required field missing = %v", err)
	}
	if still, _ := store.GetPossession(id, false); still["kind"] != "watch" {
		t.Fatalf("a refused move should change nothing: %v", still["kind"])
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
