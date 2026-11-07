package possessionstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const testDay = int64(1794010000) // 2026-11-06 in Los Angeles

// datedPhotos is fakePhotos with a day of its own for each photo.
type datedPhotos struct {
	fakePhotos
	takenAt map[string]int64
}

func (photos datedPhotos) TakenAt(ctx context.Context, photoID string) (int64, error) {
	if taken, known := photos.takenAt[photoID]; known {
		return taken, nil
	}
	return testDay, nil
}

func openAuditStore(t *testing.T) *Store {
	t.Helper()
	known := map[string]bool{}
	for number := 1; number <= 40; number++ {
		known[fmt.Sprintf("photo_%06d", number)] = true
	}
	store, err := Open(t.TempDir(), fakeReferences{known: known})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// auditReviewer reads photo_00000N on day N-1 after testDay unless days
// says otherwise.
func auditReviewer(t *testing.T, store *Store, scripted *scriptedModels, days map[string]int) *Reviewer {
	takenAt := map[string]int64{}
	for number := 1; number <= 40; number++ {
		takenAt[fmt.Sprintf("photo_%06d", number)] = testDay + int64(number-1)*86400
	}
	for photoID, day := range days {
		takenAt[photoID] = testDay + int64(day)*86400
	}
	return &Reviewer{Store: store, Models: scripted, Photos: datedPhotos{takenAt: takenAt}, FirstReaderRole: "first", SecondReaderRole: "second",
		OwnerLocation: mustLocation(t, "America/Los_Angeles")}
}

// readOutfit runs an outfit reading of a photo in which the reader lists
// these pieces.
func readOutfit(t *testing.T, reviewer *Reviewer, scripted *scriptedModels, photoID string, pieces ...finalItem) PhotoReview {
	t.Helper()
	ctx := context.Background()
	scripted.answers["first 1"] = finalReading{Items: pieces}
	review, _, err := reviewer.Store.QueuePhotoReview(ctx, photoID, "principal_000001", "outfit", "person_000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewer.Review(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	read, err := reviewer.Store.GetPhotoReview(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func top(label, garmentType string) finalItem {
	return finalItem{readerItem: item(label, label, "", "garment", Box{0.3, 0.3, 0.7, 0.7}, fieldValue{Name: "category", Value: "top"}, fieldValue{Name: "garment_type", Value: garmentType})}
}

func jacket(label string) finalItem {
	return finalItem{readerItem: item(label, label, "", "garment", Box{0.2, 0.3, 0.8, 0.8}, fieldValue{Name: "category", Value: "outerwear"}, fieldValue{Name: "garment_type", Value: "jacket"})}
}

func bottoms(label string) finalItem {
	return finalItem{readerItem: item(label, label, "", "garment", Box{0.3, 0.6, 0.7, 0.95}, fieldValue{Name: "category", Value: "bottom"}, fieldValue{Name: "garment_type", Value: "jeans"})}
}

func cap(label string) finalItem {
	return finalItem{readerItem: item(label, label, "", "garment", Box{0.4, 0.0, 0.6, 0.1}, fieldValue{Name: "category", Value: "headwear"}, fieldValue{Name: "garment_type", Value: "baseball_cap"})}
}

func pieceOf(t *testing.T, store *Store, review PhotoReview, label string) string {
	t.Helper()
	for _, sighting := range review.Sightings {
		if sighting.Label == label && sighting.PossessionID != nil {
			return *sighting.PossessionID
		}
	}
	t.Fatalf("%s has no recorded %q: %+v", review.ID, label, review.Sightings)
	return ""
}

func choose(t *testing.T, reviewer *Reviewer, questionID string, index int) {
	t.Helper()
	if _, err := reviewer.AnswerQuestion(context.Background(), questionID, Answer{Choice: &index, AnsweredBy: "principal_000001"}); err != nil {
		t.Fatalf("answer %s with %d: %v", questionID, index, err)
	}
}

func possessionPhotoIDs(t *testing.T, store *Store, id string) []string {
	t.Helper()
	possession, err := store.GetPossession(id, false)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, link := range possession["photos"].([]PhotoLink) {
		ids = append(ids, link.PhotoID)
	}
	sort.Strings(ids)
	return ids
}

func TestAMergeIsUndoneRowByRow(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	first := readOutfit(t, reviewer, scripted, "photo_000001", top("gray hoodie", "hoodie"), cap("red cap"))
	keptID := pieceOf(t, store, first, "gray hoodie")
	second := readOutfit(t, reviewer, scripted, "photo_000002", top("grey hoodie", "hoodie"))
	duplicateID := pieceOf(t, store, second, "grey hoodie")
	if keptID == duplicateID {
		t.Fatalf("the match said none, so the second hoodie should be its own possession")
	}
	if _, err := store.PatchPossession(ctx, duplicateID, fields(t, `{"brand":"Uniqlo"}`), "principal_000001"); err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetPossession(keptID, false)

	merged, err := store.MergePossession(ctx, duplicateID, keptID, "principal_000001", "one hoodie")
	if err != nil {
		t.Fatal(err)
	}
	mergeID, _ := merged["merge_id"].(string)
	if !strings.HasPrefix(mergeID, "possession_merge_") || merged["brand"] != "Uniqlo" || len(merged["photos"].([]PhotoLink)) != 2 {
		t.Fatalf("merged = %v", merged)
	}
	secondOutfit, _ := store.GetOutfit(*second.OutfitID, false)
	if len(secondOutfit.PossessionIDs) != 1 || secondOutfit.PossessionIDs[0] != keptID {
		t.Fatalf("the second outfit should hold the kept hoodie: %v", secondOutfit.PossessionIDs)
	}
	if sighting := mustSighting(t, store, second.Sightings[0].ID); *sighting.PossessionID != keptID {
		t.Fatalf("the sighting should point at the kept hoodie: %v", *sighting.PossessionID)
	}

	undone, err := store.UndoPossessionMerge(ctx, mergeID, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if undone.UndoneAt == 0 || undone.UndoneBy != "principal_000001" {
		t.Fatalf("undone = %+v", undone)
	}
	back, err := store.GetPossession(duplicateID, false)
	if err != nil || back["merged_into_id"] != nil {
		t.Fatalf("the duplicate should be live again: %v %v", back, err)
	}
	kept, _ := store.GetPossession(keptID, false)
	if kept["brand"] != before["brand"] || len(kept["photos"].([]PhotoLink)) != 1 || kept["cover_photo_id"] != before["cover_photo_id"] {
		t.Fatalf("the kept hoodie should be as before: brand %q, photos %v", kept["brand"], kept["photos"])
	}
	if ids := possessionPhotoIDs(t, store, duplicateID); len(ids) != 1 || ids[0] != "photo_000002" {
		t.Fatalf("the duplicate's photos = %v", ids)
	}
	secondOutfit, _ = store.GetOutfit(*second.OutfitID, false)
	if len(secondOutfit.PossessionIDs) != 1 || secondOutfit.PossessionIDs[0] != duplicateID {
		t.Fatalf("the second outfit should hold the duplicate again: %v", secondOutfit.PossessionIDs)
	}
	if sighting := mustSighting(t, store, second.Sightings[0].ID); *sighting.PossessionID != duplicateID {
		t.Fatalf("the sighting should point back at the duplicate")
	}
	if _, err := store.UndoPossessionMerge(ctx, mergeID, "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second undo = %v, want a conflict", err)
	}

	// Undo goes in reverse order.
	third := readOutfit(t, reviewer, scripted, "photo_000003", top("charcoal hoodie", "hoodie"))
	thirdID := pieceOf(t, store, third, "charcoal hoodie")
	firstMerge, err := store.MergePossession(ctx, duplicateID, keptID, "principal_000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MergePossession(ctx, thirdID, keptID, "principal_000001", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UndoPossessionMerge(ctx, firstMerge["merge_id"].(string), "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undoing a merge before a later one on the same possession = %v, want a conflict", err)
	}
}

// linkAnswers answers a link check for one reference possession from
// per-crop verdicts, and "same piece" for every other.
func linkAnswers(referenceID string, verdicts map[string][]map[string]any) func(role, step, prompt string, pictures int) (any, bool) {
	cropLabel := regexp.MustCompile(`C\d+ is picture`)
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "links" {
			return nil, false
		}
		crops := []map[string]any{}
		for _, found := range cropLabel.FindAllString(prompt, -1) {
			crops = append(crops, map[string]any{"crop": strings.Fields(found)[0], "same_piece": true, "reason": "same print", "other_possession_ids": []string{}})
		}
		if strings.Contains(prompt, "Picture 1 is the reference: "+referenceID+":") {
			return map[string]any{"crops": verdicts[role]}, true
		}
		return map[string]any{"crops": crops}, true
	}
}

func TestTheLinkCheckUnlinksWhatBothDoubtAndAsksWhatOneDoubts(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	blue, err := store.CreatePossession(ctx, fields(t, `{"name":"blue hoodie","garment":{"category":"top","garment_type":"hoodie"},"photos":[{"photo_id":"photo_000010"}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	blueID := blue["id"].(string)
	scripted := &scriptedModels{answers: map[string]any{}}
	reviewer := auditReviewer(t, store, scripted, nil)
	scripted.respond = matchByLabel(nil)
	first := readOutfit(t, reviewer, scripted, "photo_000001", top("hoodie", "hoodie"))
	hoodieID := pieceOf(t, store, first, "hoodie")
	scripted.respond = matchByLabel(map[string]string{"hoodie": hoodieID})
	readOutfit(t, reviewer, scripted, "photo_000002", top("hoodie", "hoodie"))
	third := readOutfit(t, reviewer, scripted, "photo_000003", top("hoodie", "hoodie"), cap("cap"))
	fourth := readOutfit(t, reviewer, scripted, "photo_000004", top("hoodie", "hoodie"))
	if ids := possessionPhotoIDs(t, store, hoodieID); len(ids) != 4 {
		t.Fatalf("the hoodie should be in four photos: %v", ids)
	}

	no := func(others ...string) map[string]any {
		return map[string]any{"crop": "", "same_piece": false, "reason": "different print", "other_possession_ids": append([]string{}, others...)}
	}
	yes := map[string]any{"crop": "", "same_piece": true, "reason": "same print", "other_possession_ids": []string{}}
	label := func(name string, verdict map[string]any) map[string]any {
		copied := map[string]any{}
		for key, value := range verdict {
			copied[key] = value
		}
		copied["crop"] = name
		return copied
	}
	scripted.respond = linkAnswers(hoodieID, map[string][]map[string]any{
		"first":  {label("C1", yes), label("C2", no(blueID)), label("C3", yes)},
		"second": {label("C1", yes), label("C2", no()), label("C3", no())},
	})
	run, err := reviewer.CheckWearableLinks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["confirmed"] != 1 || run.Counts["unlinked"] != 1 || run.Counts["asked"] != 1 || len(run.Failures) != 0 || run.CostUSD == 0 {
		t.Fatalf("run = %+v", run)
	}
	if ids := possessionPhotoIDs(t, store, hoodieID); strings.Join(ids, ",") != "photo_000001,photo_000002,photo_000004" {
		t.Fatalf("photo 3 should be unlinked: %v", ids)
	}
	thirdSighting := mustSighting(t, store, third.Sightings[0].ID)
	if thirdSighting.Status != "proposed" || thirdSighting.PossessionID != nil {
		t.Fatalf("the unlinked sighting should wait again: %+v", thirdSighting)
	}
	thirdOutfit, _ := store.GetOutfit(*third.OutfitID, false)
	if contains(thirdOutfit.PossessionIDs, hoodieID) || len(thirdOutfit.PossessionIDs) != 1 {
		t.Fatalf("the hoodie should leave the third outfit: %v", thirdOutfit.PossessionIDs)
	}
	var unlinkedQuestion, linkQuestion OwnerQuestion
	for _, action := range run.Actions {
		question, _ := store.GetQuestion(action.QuestionID)
		switch action.Action {
		case "unlinked":
			unlinkedQuestion = question
		case "asked":
			linkQuestion = question
		}
	}
	if len(unlinkedQuestion.Choices) != 3 || unlinkedQuestion.Choices[1].Resolution.PossessionID != blueID || unlinkedQuestion.Purpose != "decision" || unlinkedQuestion.AskedBy != "link_check" {
		t.Fatalf("the unlinked sighting's question = %+v", unlinkedQuestion)
	}
	if review, _ := store.GetPhotoReview(third.ID); review.Status != "waiting_for_owner" {
		t.Fatalf("a review with a new decision question should wait: %s", review.Status)
	}
	choose(t, reviewer, unlinkedQuestion.ID, 1)
	thirdOutfit, _ = store.GetOutfit(*third.OutfitID, false)
	if !contains(thirdOutfit.PossessionIDs, blueID) {
		t.Fatalf("the blue hoodie should join the third outfit: %v", thirdOutfit.PossessionIDs)
	}

	if linkQuestion.Choices[0].Resolution.Action != "keep_link" || linkQuestion.Choices[1].Resolution.UnlinkPossessionID != hoodieID {
		t.Fatalf("the doubted link's question = %+v", linkQuestion)
	}
	if ids := possessionPhotoIDs(t, store, hoodieID); len(ids) != 3 {
		t.Fatalf("a link one reader doubts stays until answered: %v", ids)
	}
	scripted.answers["first choice"] = map[string]any{"choice": 1, "explanation": "a hoodie never recorded"}
	if _, err := reviewer.AnswerQuestion(ctx, linkQuestion.ID, Answer{Text: "that's another one I never added", AnsweredBy: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	if ids := possessionPhotoIDs(t, store, hoodieID); strings.Join(ids, ",") != "photo_000001,photo_000002" {
		t.Fatalf("after 'a different piece' photo 4 should be unlinked: %v", ids)
	}
	fourthSighting := mustSighting(t, store, fourth.Sightings[0].ID)
	if fourthSighting.Status != "accepted" || *fourthSighting.PossessionID == hoodieID {
		t.Fatalf("photo 4 should be a new piece: %+v", fourthSighting)
	}

	again, err := reviewer.CheckWearableLinks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["unlinked"] != 0 || again.Counts["asked"] != 0 {
		t.Fatalf("a second pass changed things: %+v", again)
	}
}

// clusterAnswers clusters, in a duplicate check, the labels of the given
// possession ids, per role.
func clusterAnswers(clusters map[string][][]string, maximumPictures *int) func(role, step, prompt string, pictures int) (any, bool) {
	line := regexp.MustCompile(`(I\d+) \(picture \d+\) — (possession_\d+):`)
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "duplicates" {
			return nil, false
		}
		if maximumPictures != nil && pictures > *maximumPictures {
			*maximumPictures = pictures
		}
		labels := map[string]string{}
		for _, match := range line.FindAllStringSubmatch(prompt, -1) {
			labels[match[2]] = match[1]
		}
		answer := []map[string]any{}
		for _, cluster := range clusters[role] {
			members := []string{}
			for _, id := range cluster {
				if label, shown := labels[id]; shown {
					members = append(members, label)
				}
			}
			if len(members) >= 2 {
				answer = append(answer, map[string]any{"members": members, "reason": "same patch"})
			}
		}
		return map[string]any{"clusters": answer}, true
	}
}

func TestTheDuplicateCheckMergesWhatBothClusterAndAsksTheRest(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	ids := []string{}
	for number := 1; number <= 3; number++ {
		review := readOutfit(t, reviewer, scripted, fmt.Sprintf("photo_%06d", number), jacket("black jacket"))
		ids = append(ids, pieceOf(t, store, review, "black jacket"))
	}
	scripted.respond = clusterAnswers(map[string][][]string{
		"first":  {{ids[0], ids[1], ids[2]}},
		"second": {{ids[0], ids[1]}},
	}, nil)
	run, err := reviewer.CheckWearableDuplicates(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["merged"] != 1 || run.Counts["asked"] != 1 || len(run.Failures) != 0 {
		t.Fatalf("run = %+v", run)
	}
	if gone, _ := store.GetPossession(ids[1], true); gone["merged_into_id"] != ids[0] {
		t.Fatalf("the second jacket should be merged into the first: %v", gone["merged_into_id"])
	}
	var questionID string
	for _, action := range run.Actions {
		if action.Action == "asked" {
			questionID = action.QuestionID
		}
	}
	question, _ := store.GetQuestion(questionID)
	if question.Choices[0].Resolution.Action != "merge_possessions" || question.Choices[0].Resolution.PossessionID != ids[2] || question.Choices[0].Resolution.IntoPossessionID != ids[0] {
		t.Fatalf("question = %+v", question)
	}
	again, err := reviewer.CheckWearableDuplicates(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["asked"] != 0 || again.Counts["merged"] != 0 {
		t.Fatalf("a second pass should leave the asked pair to the owner: %+v", again)
	}
	choose(t, reviewer, questionID, 0)
	if photos := possessionPhotoIDs(t, store, ids[0]); len(photos) != 3 {
		t.Fatalf("the kept jacket should hold all three photos: %v", photos)
	}
	merges, _ := store.ListPossessionMerges(ids[0], 0, 0)
	if len(merges) != 2 {
		t.Fatalf("merges = %+v", merges)
	}
	if _, err := store.UndoPossessionMerge(ctx, merges[1].ID, "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undoing the older merge first = %v, want a conflict", err)
	}
	for _, merge := range merges {
		if _, err := store.UndoPossessionMerge(ctx, merge.ID, "principal_000001"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		if photos := possessionPhotoIDs(t, store, id); len(photos) != 1 {
			t.Fatalf("%s after both undos has photos %v", id, photos)
		}
	}
}

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

// outfitAnswers answers an outfit comparison per role.
func outfitAnswers(answers map[string]outfitComparison) func(role, step, prompt string, pictures int) (any, bool) {
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "outfits" {
			return nil, false
		}
		return answers[role], true
	}
}

// twoOutfitsOneDay reads a hoodie and jeans on day 0, the same seen as new
// pieces on day 1, and records the second outfit as also worn on day 0.
func twoOutfitsOneDay(t *testing.T) (*Store, *Reviewer, *scriptedModels, [2]PhotoReview, [4]string) {
	store := openAuditStore(t)
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	first := readOutfit(t, reviewer, scripted, "photo_000001", top("hoodie", "hoodie"), bottoms("jeans"))
	second := readOutfit(t, reviewer, scripted, "photo_000002", top("hoodie", "hoodie"), bottoms("jeans"))
	if *first.OutfitID == *second.OutfitID {
		t.Fatalf("the second reading saw new pieces on another day and should make a second outfit")
	}
	if _, err := store.RecordWearing(*second.OutfitID, first.WornOn); err != nil {
		t.Fatal(err)
	}
	pieces := [4]string{pieceOf(t, store, first, "hoodie"), pieceOf(t, store, first, "jeans"), pieceOf(t, store, second, "hoodie"), pieceOf(t, store, second, "jeans")}
	return store, reviewer, scripted, [2]PhotoReview{first, second}, pieces
}

func TestConsolidationCombinesOneDaysOutfitsAndMergesTheirPieces(t *testing.T) {
	store, reviewer, scripted, reviews, pieces := twoOutfitsOneDay(t)
	ctx := context.Background()
	same := outfitComparison{SameOutfit: true, Reason: "same clothes", Pieces: []pieceAnswer{{Piece: pieces[0], Match: pieces[2]}, {Piece: pieces[1], Match: pieces[3]}}}
	scripted.respond = outfitAnswers(map[string]outfitComparison{"first": same, "second": same})
	run, err := reviewer.ConsolidateOutfits(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["merged"] != 2 || run.Counts["combined"] != 1 || len(run.Failures) != 0 {
		t.Fatalf("run = %+v", run)
	}
	outfits, _ := store.ListOutfits("", "", 0, 0)
	if len(outfits) != 1 || len(outfits[0].PossessionIDs) != 2 || len(outfits[0].WornOn) != 2 || len(outfits[0].PhotoIDs) != 2 {
		t.Fatalf("outfits = %+v", outfits)
	}
	if hoodies, _ := store.ListPossessions(PossessionFilter{Kind: "garment"}); len(hoodies) != 2 {
		t.Fatalf("two pieces should be left, got %d", len(hoodies))
	}
	var combineID string
	for _, action := range run.Actions {
		if action.Action == "combined" {
			combineID = action.MergeID
		}
	}
	if _, err := store.UndoOutfitMerge(ctx, combineID, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if outfits, _ := store.ListOutfits("", "", 0, 0); len(outfits) != 2 {
		t.Fatalf("undoing the combine should bring both outfits back: %d", len(outfits))
	}
	for _, review := range reviews {
		if read, _ := store.GetPhotoReview(review.ID); *read.OutfitID != *review.OutfitID {
			t.Fatalf("%s should answer its own outfit again", review.ID)
		}
	}
}

func TestConsolidationAsksWhenAPieceIsMappedDifferently(t *testing.T) {
	store, reviewer, scripted, _, pieces := twoOutfitsOneDay(t)
	ctx := context.Background()
	first := outfitComparison{SameOutfit: true, Reason: "same clothes", Pieces: []pieceAnswer{{Piece: pieces[0], Match: pieces[2]}, {Piece: pieces[1], Match: pieces[3]}}}
	second := outfitComparison{SameOutfit: true, Reason: "same top", Pieces: []pieceAnswer{{Piece: pieces[0], Match: pieces[2]}, {Piece: pieces[1], Match: "not_visible"}}}
	scripted.respond = outfitAnswers(map[string]outfitComparison{"first": first, "second": second})
	run, err := reviewer.ConsolidateOutfits(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["asked"] != 1 || run.Counts["merged"] != 0 || run.Counts["combined"] != 0 {
		t.Fatalf("run = %+v", run)
	}
	if outfits, _ := store.ListOutfits("", "", 0, 0); len(outfits) != 2 {
		t.Fatalf("a refused pair stays two outfits: %d", len(outfits))
	}
	question, _ := store.GetQuestion(run.Actions[0].QuestionID)
	combine := question.Choices[0].Resolution
	if combine.Action != "combine_outfits" || len(combine.PieceMerges) != 1 || !strings.Contains(question.Question, "not_visible") {
		t.Fatalf("question = %+v", question)
	}
	again, _ := reviewer.ConsolidateOutfits(ctx, "principal_000001")
	if again.Counts["asked"] != 0 {
		t.Fatalf("a second pass should not ask again: %+v", again)
	}
	choose(t, reviewer, question.ID, 0)
	outfits, _ := store.ListOutfits("", "", 0, 0)
	if len(outfits) != 1 || len(outfits[0].PossessionIDs) != 3 {
		t.Fatalf("after 'the same outfit': %+v (the hoodies merged, both jeans kept)", outfits)
	}
}

func TestOutfitsWithTheSamePiecesAreCombinedWithoutAsking(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	shirt, _ := store.CreatePossession(ctx, fields(t, `{"name":"shirt","garment":{"category":"top"}}`), "principal_000001")
	shoes, _ := store.CreatePossession(ctx, fields(t, `{"name":"shoes","garment":{"category":"footwear"}}`), "principal_000001")
	kept := "kept"
	pieces := []string{shirt["id"].(string), shoes["id"].(string)}
	first, err := store.CreateOutfit(ctx, OutfitInput{Status: &kept, PossessionIDs: pieces}, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := store.CreateOutfit(ctx, OutfitInput{Status: &kept, PossessionIDs: []string{pieces[1], pieces[0]}}, "principal_000001")
	if _, err := store.RecordWearing(second.ID, "2026-11-01"); err != nil {
		t.Fatal(err)
	}
	reviewer := auditReviewer(t, store, &scriptedModels{answers: map[string]any{}}, nil)
	run, err := reviewer.ConsolidateOutfits(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["combined_identical"] != 1 || run.CostUSD != 0 {
		t.Fatalf("run = %+v", run)
	}
	gone, _ := store.GetOutfit(first.ID, true)
	if gone.MergedIntoID == nil || *gone.MergedIntoID != second.ID {
		t.Fatalf("the outfit worn on fewer days should go into the other: %+v", gone)
	}
}

func TestANewOutfitReadingMatchesPiecesByPictureWithBothReaders(t *testing.T) {
	store := openAuditStore(t)
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, map[string]int{"photo_000003": 1})
	first := readOutfit(t, reviewer, scripted, "photo_000001", jacket("jacket"), cap("cap"))
	jacketID, capID := pieceOf(t, store, first, "jacket"), pieceOf(t, store, first, "cap")

	scripted.respond = func(role, step, prompt string, pictures int) (any, bool) {
		if step != "match" {
			return nil, false
		}
		if strings.Contains(prompt, "new photo: jacket (") && role == "first" {
			return map[string]string{"match": jacketID, "reason": "same zip"}, true
		}
		return map[string]string{"match": "none", "reason": "different"}, true
	}
	second := readOutfit(t, reviewer, scripted, "photo_000002", jacket("jacket"), cap("cap"))
	decisions, _ := store.ListQuestions(false, second.ID, "decision", 0, 0)
	if len(decisions) != 1 || decisions[0].Choices[0].Resolution.PossessionID != jacketID || decisions[0].Choices[1].Resolution.Action != "accept" || decisions[0].AskedBy != "picture_match" {
		t.Fatalf("the jacket the readers differ on should be a decision: %+v", decisions)
	}
	newCap := pieceOf(t, store, second, "cap")
	if newCap == capID {
		t.Fatalf("both readers said the cap is none of the recorded ones, so it is new")
	}
	if second.Status != "waiting_for_owner" || len(second.Readings) != 5 {
		t.Fatalf("review = %s with %d readings (one reading, two matches by two readers)", second.Status, len(second.Readings))
	}

	scripted.respond = matchByLabel(map[string]string{"cap": newCap})
	third := readOutfit(t, reviewer, scripted, "photo_000003", cap("cap"), finalItem{readerItem: item("scarf", "scarf", "", "garment", Box{0.4, 0.3, 0.6, 0.4}, fieldValue{Name: "category", Value: "accessory"}, fieldValue{Name: "garment_type", Value: "scarf"})})
	if third.WornOn != second.WornOn || *third.OutfitID != *second.OutfitID {
		t.Fatalf("a reading on the same day sharing a piece should find that day's outfit: %v vs %v", third.OutfitID, second.OutfitID)
	}
	outfit, _ := store.GetOutfit(*second.OutfitID, false)
	if len(outfit.PossessionIDs) != 2 {
		t.Fatalf("the day's outfit should now hold the cap and the scarf: %v", outfit.PossessionIDs)
	}
}
