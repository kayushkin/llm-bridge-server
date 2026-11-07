package possessionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"regexp"
	"strings"
	"testing"
)

// asymmetricPicture has a bright block in its top left corner and a ramp,
// so every turn of it looks different.
func asymmetricPicture(width, height int) *image.RGBA {
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			level := uint8(40 + 100*x/width)
			if x < width/3 && y < height/4 {
				level = 250
			}
			picture.Set(x, y, color.RGBA{level, level, level, 255})
		}
	}
	return picture
}

func TestAnOriginalIsTurnedToStandAsItsThumbnail(t *testing.T) {
	raw := asymmetricPicture(300, 200)
	thumbnail := resized(Upright(raw, 6), 100) // the importer stood it upright by EXIF 6
	for _, claimed := range []int{6, 1, 8} {
		orientation, err := orientationMatching(raw, thumbnail, claimed)
		if err != nil || orientation != 6 {
			t.Fatalf("EXIF says %d: matched %d, %v; want 6 whatever EXIF says", claimed, orientation, err)
		}
	}
	unturned := resized(raw, 100) // a thumbnail made without turning it
	if orientation, err := orientationMatching(raw, unturned, 6); err != nil || orientation != 1 {
		t.Fatalf("a thumbnail never turned: matched %d, %v; want 1", orientation, err)
	}
	other := image.NewRGBA(image.Rect(0, 0, 67, 100))
	for x := 0; x < 67; x++ {
		for y := 0; y < 100; y++ {
			other.Set(x, y, color.RGBA{uint8(255 * ((x / 8) % 2)), 0, 0, 255})
		}
	}
	if _, err := orientationMatching(raw, other, 6); err == nil {
		t.Fatalf("a different picture should not match")
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, raw, nil); err != nil {
		t.Fatal(err)
	}
	aligned, orientation, err := alignOriginal(encoded.Bytes(), thumbnail)
	if err != nil || orientation != 6 || aligned.Bounds().Dx() != 200 || aligned.Bounds().Dy() != 300 {
		t.Fatalf("aligned %v, orientation %d, %v", aligned.Bounds(), orientation, err)
	}
	// The bright block was top left of the raw picture; turned by 6 it is
	// top right, so a box read on the thumbnail's top right cuts it.
	crop, err := detailCrop(aligned, Box{0.75, 0, 1, 0.33}, true)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, _ := image.Decode(bytes.NewReader(crop))
	if longest := max(decoded.Bounds().Dx(), decoded.Bounds().Dy()); longest < smallPieceLongestSide {
		t.Fatalf("a small piece should be scaled up to %d, got %d", smallPieceLongestSide, longest)
	}
	red, _, _, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/3).RGBA()
	if red>>8 < 200 {
		t.Fatalf("the crop should show the bright block, got level %d", red>>8)
	}
}

func TestGroupsAreCountedAsTheOwnerCountsThem(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	for fieldsText, want := range map[string]string{
		`{"name":"a","jewelry":{"jewelry_type":"pendant"}}`:                      "jewelry:necklace",
		`{"name":"b","jewelry":{"jewelry_type":"chain"}}`:                        "jewelry:necklace",
		`{"name":"c","garment":{"category":"accessory","garment_type":"scarf"}}`: "garment:scarf",
		`{"name":"d","garment":{"category":"accessory","garment_type":"socks"}}`: "",
		`{"name":"e","garment":{"category":"bottom","garment_type":"jeans"}}`:    "garment:bottom",
	} {
		made, err := store.CreatePossession(ctx, fields(t, fieldsText), "principal_000001")
		if err != nil {
			t.Fatal(err)
		}
		if got := possessionGroup(made); got != want {
			t.Fatalf("%s is in %q, want %q", fieldsText, got, want)
		}
	}
	low, high := 3, 9
	group, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", WearableGroupChange{ExpectedLow: &low, ExpectedHigh: &high, Actor: "principal_000001"})
	if err != nil || group.ExpectedLow == nil || *group.ExpectedLow != 3 || *group.ExpectedHigh != 9 || group.Members != 2 || group.Closed {
		t.Fatalf("group = %+v, %v", group, err)
	}
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:pendant", WearableGroupChange{ExpectedHigh: &high, Actor: "principal_000001"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pendants are necklaces, not a group: %v", err)
	}
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", WearableGroupChange{ExpectedLow: &high, ExpectedHigh: &low, Actor: "principal_000001"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a range upside down: %v", err)
	}
	groups, _ := store.ListWearableGroups()
	for _, listed := range groups {
		if listed.Group == "" {
			t.Fatalf("socks should be in no group: %+v", groups)
		}
	}
}

var sortLine = regexp.MustCompile(`([SR]\d+) \(picture \d+\): (.*)`)

// sortAnswers sorts pictures for the identification: the first reader puts
// every label of a group in one piece; the second the same, but keeps
// apart any recorded picture whose words hold apartID.
func sortAnswers(apartID string) func(role, step, prompt string, pictures int) (any, bool) {
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "pieces" {
			return nil, false
		}
		together, apart := []string{}, []map[string]any{}
		for _, match := range sortLine.FindAllStringSubmatch(prompt, -1) {
			if role == "second" && apartID != "" && strings.Contains(match[2], apartID) {
				apart = append(apart, map[string]any{"members": []string{match[1]}, "reason": "other case"})
				continue
			}
			together = append(together, match[1])
		}
		pieces := append([]map[string]any{{"members": together, "reason": "same piece"}}, apart...)
		return map[string]any{"pieces": pieces, "uncertain": []any{}}, true
	}
}

func box(left, top, right, bottom float64) map[string]float64 {
	return map[string]float64{"left": left, "top": top, "right": right, "bottom": bottom}
}

func lookSlot(slot, group, name, status, from string, boxes ...map[string]any) map[string]any {
	return map[string]any{"slot": slot, "group": group, "name": name, "description": name, "kind_fields": []any{}, "boxes": boxes, "status": status, "from_slot": from}
}

func onPhoto(label string, where map[string]float64) map[string]any {
	return map[string]any{"photo": label, "box": where}
}

func TestLooksAreReadIdentifiedAndRebuiltIntoOutfits(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, map[string]int{"photo_000002": 0, "photo_000003": 1, "photo_000004": 2})
	steel := ownedWatch(t, store, "steel watch", "photo_000010")
	unconfirmed, err := store.CreatePossession(ctx, fields(t, `{"name":"wrist watch","status":"unconfirmed","watch":{},"photos":[{"photo_id":"photo_000011"}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	unconfirmedID := unconfirmed["id"].(string)
	// Readings of the old kind made an outfit for each selfie.
	readings := []PhotoReview{}
	for _, photoID := range []string{"photo_000001", "photo_000002", "photo_000003", "photo_000004"} {
		readings = append(readings, readOutfit(t, reviewer, scripted, photoID, cap("cap")))
	}
	oldOutfits := map[string]bool{}
	for _, reading := range readings {
		oldOutfits[*reading.OutfitID] = true
	}
	low, high := 1, 3
	if _, err := store.ChangeWearableGroup(ctx, "watch", WearableGroupChange{ExpectedLow: &low, ExpectedHigh: &high, Actor: "principal_000001"}); err != nil {
		t.Fatal(err)
	}

	watchBox, shirtBox, jeansBox := box(0.6, 0.6, 0.66, 0.66), box(0.3, 0.3, 0.7, 0.7), box(0.3, 0.7, 0.7, 0.98)
	scripted.respond = func(role, step, prompt string, pictures int) (any, bool) {
		switch {
		case step == "looks":
			return map[string]any{"looks": []any{}}, true // the checker's list is the one recorded
		case step == "check_looks" && strings.Contains(prompt, "on 2026-11-06."):
			return map[string]any{"looks": []any{map[string]any{"photos": []string{"P1", "P2"}, "slots": []any{
				lookSlot("watch, left wrist", "watch", "steel watch", "agreed", "S1", onPhoto("P1", watchBox), onPhoto("P2", watchBox)),
				lookSlot("t-shirt", "garment:top", "white t-shirt", "changed", "S2", onPhoto("P1", shirtBox)),
				lookSlot("jeans", "garment:bottom", "blue jeans", "agreed", "S4", onPhoto("P2", jeansBox)),
			}}}, "removed": []any{map[string]any{"slot": "S3", "reason": "a shadow, not a belt"}}}, true
		case step == "check_looks" && strings.Contains(prompt, "on 2026-11-07."):
			return map[string]any{"looks": []any{map[string]any{"photos": []string{"P1"}, "slots": []any{
				lookSlot("watch, left wrist", "watch", "steel watch", "agreed", "S1", onPhoto("P1", watchBox)),
				lookSlot("t-shirt", "garment:top", "white t-shirt", "added", "", onPhoto("P1", shirtBox)),
				lookSlot("jeans", "garment:bottom", "blue jeans", "agreed", "S2", onPhoto("P1", jeansBox)),
			}}}, "removed": []any{}}, true
		case step == "check_looks":
			return map[string]any{"looks": []any{}, "removed": []any{}}, true // leaves its photo out: refused
		}
		return sortAnswers(unconfirmedID)(role, step, prompt, pictures)
	}
	read, err := reviewer.ReadLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if read.Looked != 3 || read.Counts["looks"] != 2 || read.Counts["slots"] != 6 || len(read.Failures) != 1 || len(read.Left) != 1 {
		t.Fatalf("read = %+v", read)
	}
	looks, _ := store.ListLooks("", "", 0, 0)
	if looks[0].WearerHasNoTop == nil || *looks[0].WearerHasNoTop {
		t.Fatalf("the second reader's word on a top should be kept: %v", looks[0].WearerHasNoTop)
	}
	if len(looks) != 2 || len(looks[0].PhotoIDs) != 2 || looks[0].Slots[0].Agreement != "both_agree" || looks[0].Slots[1].Agreement != "second_reader_changed" || looks[1].Slots[1].Agreement != "second_reader_added" {
		t.Fatalf("looks = %+v", looks)
	}
	if again, _ := reviewer.ReadLooks(ctx, "principal_000001"); again.Looked != 1 {
		t.Fatalf("a second pass reads only the day that failed: %+v", again)
	}

	identified, err := reviewer.IdentifySlots(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if identified.Counts["linked"] != 6 || identified.Counts["merged"] != 1 || identified.Counts["new_pieces"] != 2 || len(identified.Failures) != 0 {
		t.Fatalf("identified = %+v", identified)
	}
	var watchCount GroupCount
	for _, group := range identified.Groups {
		if group.Group == "watch" {
			watchCount = group
		}
	}
	if watchCount.Identified != 1 || watchCount.WithinRange == nil || !*watchCount.WithinRange || watchCount.Slots != 2 {
		t.Fatalf("watch count = %+v", watchCount)
	}
	if gone, _ := store.GetPossession(unconfirmedID, true); gone["merged_into_id"] != steel {
		t.Fatalf("the unconfirmed watch should be merged into the owned one: %v", gone["merged_into_id"])
	}
	looks, _ = store.ListLooks("", "", 0, 0)
	watchSlot, shirtSlot, laterShirt := looks[0].Slots[0], looks[0].Slots[1], looks[1].Slots[1]
	if *watchSlot.PossessionID != steel || watchSlot.DecidedBy != "best_guess" || watchSlot.Candidates[0].PossessionID != steel || watchSlot.Photos[0].SightingID == nil {
		t.Fatalf("watch slot = %+v", watchSlot)
	}
	if shirtSlot.PossessionID == nil || laterShirt.PossessionID == nil || *shirtSlot.PossessionID != *laterShirt.PossessionID {
		t.Fatalf("both shirts are one new piece: %v %v", shirtSlot.PossessionID, laterShirt.PossessionID)
	}
	shirt, _ := store.GetPossession(*shirtSlot.PossessionID, false)
	if shirt["status"] != "unconfirmed" || shirt["kind"] != "garment" || len(shirt["photos"].([]PhotoLink)) != 2 {
		t.Fatalf("the new shirt = %v", shirt)
	}
	questions, _ := store.ListQuestions(false, "", "detail", 0, 0)
	var shirtQuestion OwnerQuestion
	for _, question := range questions {
		if question.AskedBy == "outfit_slots" && question.SightingID == *laterShirt.Photos[0].SightingID {
			shirtQuestion = question
		}
	}
	if len(shirtQuestion.Choices) != 3 || shirtQuestion.Choices[0].Resolution.Action != "slot_link" || shirtQuestion.Choices[1].Resolution.Action != "slot_new" {
		t.Fatalf("the slot's question = %+v", shirtQuestion)
	}

	rebuilt, err := store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Counts["outfits_made"] != 1 || rebuilt.Counts["outfits_replaced"] != 3 {
		t.Fatalf("rebuilt = %+v (the two looks hold the same pieces; the fourth selfie's outfit keeps its reading)", rebuilt)
	}
	outfits, _ := store.ListOutfits("", "", 0, 0)
	var made Outfit
	for _, outfit := range outfits {
		if !oldOutfits[outfit.ID] {
			made = outfit
		}
	}
	if len(made.PossessionIDs) != 3 || strings.Join(made.WornOn, ",") != "2026-11-06,2026-11-07" || len(made.PhotoIDs) != 3 {
		t.Fatalf("the rebuilt outfit = %+v", made)
	}
	rebuildID := rebuilt.Actions[len(rebuilt.Actions)-1].Records[0]
	if _, err := store.UndoOutfitRebuild(ctx, rebuildID, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	for _, reading := range readings {
		if back, _ := store.GetPhotoReview(reading.ID); *back.OutfitID != *reading.OutfitID {
			t.Fatalf("%s should answer its old outfit after the undo", reading.ID)
		}
	}
	if outfits, _ := store.ListOutfits("", "", 0, 0); len(outfits) != len(oldOutfits) {
		t.Fatalf("after the undo the old outfits are back alone: %d", len(outfits))
	}
	rebuilt, err = store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	rebuildID = rebuilt.Actions[len(rebuilt.Actions)-1].Records[0]

	choose(t, reviewer, shirtQuestion.ID, 1) // a different shirt, not recorded yet
	first, _ := store.GetLook(looks[0].ID)
	moved, _ := store.GetLook(looks[1].ID)
	if first.OutfitID == nil || moved.OutfitID == nil || *moved.OutfitID == *first.OutfitID {
		t.Fatalf("the look whose shirt changed should leave the shared outfit: %v %v", first.OutfitID, moved.OutfitID)
	}
	split, _ := store.GetOutfit(*moved.OutfitID, false)
	if strings.Join(split.WornOn, ",") != "2026-11-07" || len(split.PossessionIDs) != 3 || contains(split.PossessionIDs, *shirtSlot.PossessionID) || len(split.PhotoIDs) != 1 {
		t.Fatalf("split outfit = %+v", split)
	}
	if shared, _ := store.GetOutfit(*first.OutfitID, false); strings.Join(shared.WornOn, ",") != "2026-11-06" || len(shared.PhotoIDs) != 2 {
		t.Fatalf("the shared outfit keeps only the first day: %+v", shared)
	}
	if newShirt := mustSighting(t, store, *laterShirt.Photos[0].SightingID); *newShirt.PossessionID == *shirtSlot.PossessionID {
		t.Fatalf("the sighting should point at the new shirt")
	}
	if _, err := store.UndoOutfitRebuild(ctx, rebuildID, "principal_000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("undoing a rebuild after a look moved = %v, want a conflict", err)
	}
}

func TestAClosedGroupsSlotIsRankedOnlyAgainstItsMembers(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	steel := ownedWatch(t, store, "steel watch", "photo_000010")
	gold := ownedWatch(t, store, "gold watch", "photo_000011")
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	readOutfit(t, reviewer, scripted, "photo_000001")
	closeGroup(t, store, "watch")
	scripted.respond = func(role, step, prompt string, pictures int) (any, bool) {
		switch step {
		case "looks":
			return map[string]any{"looks": []any{}}, true
		case "check_looks":
			return map[string]any{"looks": []any{map[string]any{"photos": []string{"P1"}, "slots": []any{
				lookSlot("watch, left wrist", "watch", "watch", "agreed", "S1", onPhoto("P1", box(0.6, 0.6, 0.66, 0.66))),
			}}}, "removed": []any{}}, true
		case "pieces":
			// The first reader leaves the slot alone; the second puts it with the gold watch.
			labels := map[string]string{}
			for _, match := range sortLine.FindAllStringSubmatch(prompt, -1) {
				labels[match[1]] = match[2]
			}
			pieces := []map[string]any{}
			for label, text := range labels {
				if role == "second" && strings.HasPrefix(label, "S") {
					continue
				}
				members := []string{label}
				if role == "second" && strings.Contains(text, gold) {
					members = append(members, "S1")
				}
				pieces = append(pieces, map[string]any{"members": members, "reason": "dial"})
			}
			return map[string]any{"pieces": pieces, "uncertain": []any{}}, true
		}
		return nil, false
	}
	if _, err := reviewer.ReadLooks(ctx, "principal_000001"); err != nil {
		t.Fatal(err)
	}
	run, err := reviewer.IdentifySlots(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["new_pieces"] != 0 || run.Counts["linked"] != 1 {
		t.Fatalf("run = %+v", run)
	}
	looks, _ := store.ListLooks("", "", 0, 0)
	slot := looks[0].Slots[0]
	if *slot.PossessionID != gold || len(slot.Candidates) != 2 || slot.Candidates[1].PossessionID != steel {
		t.Fatalf("slot = %+v", slot)
	}
	questions, _ := store.ListQuestions(false, "", "detail", 0, 0)
	for _, question := range questions {
		if question.AskedBy != "outfit_slots" {
			continue
		}
		for _, choice := range question.Choices {
			if choice.Resolution.Action == "slot_new" {
				t.Fatalf("a closed group's slot offered a new piece: %+v", question.Choices)
			}
		}
	}
}

func TestALargeGroupIsSortedInBatchesThenAcrossThem(t *testing.T) {
	store := openAuditStore(t)
	scripted := &scriptedModels{answers: map[string]any{}}
	number := regexp.MustCompile(`shirt (\d+)`)
	scripted.respond = func(role, step, prompt string, pictures int) (any, bool) {
		if pictures > largeGroupPicturesPerCall {
			t.Errorf("a call held %d pictures", pictures)
		}
		byPiece := map[string][]string{}
		for _, match := range sortLine.FindAllStringSubmatch(prompt, -1) {
			found := number.FindStringSubmatch(match[2])
			value := 0
			fmt.Sscanf(found[1], "%d", &value)
			key := fmt.Sprint(value % 5)
			byPiece[key] = append(byPiece[key], match[1])
		}
		pieces := []map[string]any{}
		for _, members := range byPiece {
			pieces = append(pieces, map[string]any{"members": members, "reason": "print"})
		}
		return map[string]any{"pieces": pieces, "uncertain": []any{}}, true
	}
	reviewer := auditReviewer(t, store, scripted, nil)
	items := []clusterItem{}
	for index := 0; index < 65; index++ {
		items = append(items, clusterItem{Label: fmt.Sprintf("S%d", index+1), SlotID: fmt.Sprintf("outfit_slot_%06d", index+1), Text: fmt.Sprintf("worn as \"t-shirt\": shirt %d", index), Crop: testJPEG()})
	}
	run := newAuditRun()
	sorted, err := reviewer.sortItems(context.Background(), "first", WearableGroup{Group: "garment:top"}, identificationBrief(WearableGroup{Group: "garment:top"}), items, &run)
	if err != nil {
		t.Fatal(err)
	}
	pieceOfRemainder := map[int]int{}
	distinct := map[int]bool{}
	for index, piece := range sorted.piece {
		distinct[piece] = true
		if known, seen := pieceOfRemainder[index%5]; seen && known != piece {
			t.Fatalf("shirt %d is in piece %d, but its look-alikes are in %d", index, piece, known)
		}
		pieceOfRemainder[index%5] = piece
	}
	if len(distinct) != 5 || scripted.calls["first pieces"] != 4 {
		t.Fatalf("%d pieces in %d calls; want 5 pieces from three batches and one call across them", len(distinct), scripted.calls["first pieces"])
	}
}
