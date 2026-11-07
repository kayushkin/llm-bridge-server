package possessionstore

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestTwoCropsOfOneShirtInDifferentBatchesEndAsOnePiece(t *testing.T) {
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
			byPiece[found[1]] = append(byPiece[found[1]], match[1])
		}
		pieces := []map[string]any{}
		for _, members := range byPiece {
			pieces = append(pieces, map[string]any{"members": members, "reason": "print"})
		}
		return map[string]any{"pieces": pieces, "uncertain": []any{}}, true
	}
	reviewer := auditReviewer(t, store, scripted, nil)
	// Forty shirts, every one different but the first and the last, which
	// sort into different batches by their words.
	items := []clusterItem{}
	for index := 0; index < 40; index++ {
		shirt, key := index, fmt.Sprintf("m shirt %02d", index)
		switch index {
		case 0:
			key = "a striped shirt"
		case 39:
			shirt, key = 0, "z striped shirt"
		}
		items = append(items, clusterItem{Label: fmt.Sprintf("S%d", index+1), SlotID: fmt.Sprintf("outfit_slot_%06d", index+1),
			Text: fmt.Sprintf("worn as \"shirt\": shirt %d", shirt), SortKey: key, Crop: testJPEG()})
	}
	run := newAuditRun()
	facts := WearableGroup{Group: "garment:top"}
	sorted, err := reviewer.sortItems(context.Background(), "first", facts, identificationBrief(facts), items, &run)
	if err != nil {
		t.Fatal(err)
	}
	if sorted.piece[0] != sorted.piece[39] {
		t.Fatalf("the two crops of one shirt are pieces %d and %d", sorted.piece[0], sorted.piece[39])
	}
	distinct := map[int]bool{}
	for _, piece := range sorted.piece {
		distinct[piece] = true
	}
	// Two batches find nothing alike; the 40 pieces are then seen in three
	// chunks of 15, every two together: three calls more.
	if len(distinct) != 39 || scripted.calls["first pieces"] != 5 || len(run.Left) != 0 {
		t.Fatalf("%d pieces in %d calls, left %v; want 39 pieces in 5 calls", len(distinct), scripted.calls["first pieces"], run.Left)
	}
}

func necklace(t *testing.T, store *Store, name, status, photoID string) string {
	t.Helper()
	made, err := store.CreatePossession(context.Background(), fields(t, `{"name":"`+name+`","status":"`+status+`","jewelry":{"jewelry_type":"necklace"},"photos":[{"photo_id":"`+photoID+`"}]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	return made["id"].(string)
}

func TestAGroupOverItsRangeIsPulledIntoItByWhatBothReadersAgreeOn(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	ids := map[string]string{
		"A": necklace(t, store, "gold chain", "owned", "photo_000010"),
		"B": necklace(t, store, "gold chain again", "owned", "photo_000011"),
		"C": necklace(t, store, "chain in a selfie", "unconfirmed", "photo_000012"),
		"D": necklace(t, store, "pendant in a selfie", "unconfirmed", "photo_000013"),
		"E": necklace(t, store, "silver chain", "owned", "photo_000014"),
		"F": necklace(t, store, "silver chain again", "owned", "photo_000015"),
	}
	letterOf := map[string]string{}
	for letter, id := range ids {
		letterOf[id] = letter
	}
	// The first reader sees A, B and C as one; the second only A and C, and
	// puts B with D. Both see E and F as one.
	sortings := map[string][][]string{
		"first":  {{"A", "B", "C"}, {"D"}, {"E", "F"}},
		"second": {{"A", "C"}, {"B", "D"}, {"E", "F"}},
	}
	scripted := &scriptedModels{answers: map[string]any{}}
	scripted.respond = func(role, step, prompt string, pictures int) (any, bool) {
		if step != "pieces" {
			return nil, false
		}
		if !strings.Contains(prompt, "Sorting so far made 6 separate pieces") || !strings.Contains(prompt, "between 1 and 4") {
			t.Errorf("the readers should be told the range and that the sorting split too finely: %s", prompt)
		}
		labelOf := map[string]string{}
		for _, match := range sortLine.FindAllStringSubmatch(prompt, -1) {
			labelOf[letterOf[strings.SplitN(match[2], ":", 2)[0]]] = match[1]
		}
		pieces := []map[string]any{}
		for _, letters := range sortings[role] {
			members := []string{}
			for _, letter := range letters {
				members = append(members, labelOf[letter])
			}
			pieces = append(pieces, map[string]any{"members": members, "reason": "clasp"})
		}
		return map[string]any{"pieces": pieces, "uncertain": []any{}}, true
	}
	reviewer := auditReviewer(t, store, scripted, nil)
	low, high := 1, 4
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", WearableGroupChange{ExpectedLow: &low, ExpectedHigh: &high, Actor: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	run, err := reviewer.IdentifySlots(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Groups) != 1 || run.Groups[0].PullIn == nil {
		t.Fatalf("groups = %+v", run.Groups)
	}
	count := run.Groups[0]
	if count.PullIn.Before != 6 || count.PullIn.Shown != 6 || count.PullIn.Merged != 2 || count.PullIn.After != 4 || count.Identified != 4 || count.WithinRange == nil || !*count.WithinRange {
		t.Fatalf("necklaces = %+v, pull-in %+v", count, *count.PullIn)
	}
	if gone, _ := store.GetPossession(ids["C"], true); gone["merged_into_id"] != ids["A"] {
		t.Fatalf("the unconfirmed chain both readers put with A should be merged into A: %v", gone["merged_into_id"])
	}
	if kept, _ := store.GetPossession(ids["B"], false); kept["status"] != "owned" || kept["merged_into_id"] != nil {
		t.Fatalf("two owned chains only one reader put together stay apart: %v", kept)
	}
	if kept, _ := store.GetPossession(ids["D"], false); kept["merged_into_id"] != nil {
		t.Fatalf("D stays: %v", kept)
	}
	merged, _ := store.GetPossession(ids["F"], true)
	other, _ := store.GetPossession(ids["E"], true)
	if (merged["merged_into_id"] == nil) == (other["merged_into_id"] == nil) {
		t.Fatalf("the two owned silver chains both readers put together should be one: %v %v", merged["merged_into_id"], other["merged_into_id"])
	}
	// Every merge can be undone.
	for _, action := range run.Actions {
		if action.Action != "merged" {
			continue
		}
		if _, err := store.UndoPossessionMerge(ctx, action.MergeID, "principal_000001"); err != nil {
			t.Fatal(err)
		}
	}
	if members, _ := store.groupMembersToCount("jewelry:necklace"); len(members) != 6 {
		t.Fatalf("after the undos the group holds %d", len(members))
	}
	// Within its range, a group gets no further round and no model call.
	scripted.respond = nil
	high = 6
	if _, err := store.ChangeWearableGroup(ctx, "jewelry:necklace", WearableGroupChange{ExpectedHigh: &high, Actor: "principal_000001"}); err != nil {
		t.Fatal(err)
	}
	if within, err := reviewer.IdentifySlots(ctx, "principal_000001"); err != nil || len(within.Groups) != 0 || len(within.Failures) != 0 {
		t.Fatalf("a group within its range: %+v, %v", within, err)
	}
}

// addLook writes a look on a day with a slot per piece, as reading and
// identification would leave it.
func addLook(t *testing.T, store *Store, day string, pieces map[string]string) string {
	t.Helper()
	var lookCount, slotCount int
	store.db.QueryRow(`SELECT COUNT(*) FROM outfit_looks`).Scan(&lookCount)
	lookID := fmt.Sprintf("outfit_look_%06d", lookCount+1)
	if _, err := store.db.Exec(`INSERT INTO outfit_looks (id, seq, worn_on, created_by, created_at, updated_at) VALUES (?, ?, ?, 'test', 1, 1)`, lookID, lookCount+1, day); err != nil {
		t.Fatal(err)
	}
	for piece, group := range pieces {
		store.db.QueryRow(`SELECT COUNT(*) FROM outfit_slots`).Scan(&slotCount)
		if _, err := store.db.Exec(`INSERT INTO outfit_slots (id, seq, look_id, slot, group_key, proposed, agreement, possession_id, decided_by, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, '{}', 'both_agree', ?, 'best_guess', 'test', 1, 1)`, fmt.Sprintf("outfit_slot_%06d", slotCount+1), slotCount+1, lookID, group, group, piece); err != nil {
			t.Fatal(err)
		}
	}
	return lookID
}

func TestLooksAreOneOutfitByTheirMainPiecesWhateverTheAccessories(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	piece := func(text string) string {
		made, err := store.CreatePossession(ctx, fields(t, text), "principal_000001")
		if err != nil {
			t.Fatal(err)
		}
		return made["id"].(string)
	}
	garment := func(name, category, garmentType string) string {
		return piece(`{"name":"` + name + `","garment":{"category":"` + category + `","garment_type":"` + garmentType + `"}}`)
	}
	whiteShirt, blueShirt, greenShirt := garment("white shirt", "top", "t_shirt"), garment("blue shirt", "top", "t_shirt"), garment("green shirt", "top", "t_shirt")
	jeans, chinos := garment("jeans", "bottom", "jeans"), garment("chinos", "bottom", "chinos")
	jacket := garment("denim jacket", "outerwear", "jacket")
	steelWatch, goldWatch := piece(`{"name":"steel watch","watch":{}}`), piece(`{"name":"gold watch","watch":{}}`)
	ring := piece(`{"name":"ring","jewelry":{"jewelry_type":"ring"}}`)

	first := addLook(t, store, "2026-11-01", map[string]string{whiteShirt: "garment:top", jeans: "garment:bottom", steelWatch: "watch"})
	otherWatch := addLook(t, store, "2026-11-02", map[string]string{whiteShirt: "garment:top", jeans: "garment:bottom", goldWatch: "watch"})
	withJacket := addLook(t, store, "2026-11-03", map[string]string{blueShirt: "garment:top", chinos: "garment:bottom", jacket: "garment:outerwear"})
	noBottomInView := addLook(t, store, "2026-11-04", map[string]string{blueShirt: "garment:top", jacket: "garment:outerwear", ring: "jewelry:ring"})
	otherTop := addLook(t, store, "2026-11-05", map[string]string{greenShirt: "garment:top", jeans: "garment:bottom", steelWatch: "watch"})

	rebuilt, err := store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Counts["outfits_made"] != 3 || rebuilt.Counts["looks_joined"] != 2 {
		t.Fatalf("rebuilt = %+v", rebuilt.Counts)
	}
	outfitOf := func(lookID string) Outfit {
		look, err := store.GetLook(lookID)
		if err != nil || look.OutfitID == nil {
			t.Fatalf("%s has no outfit: %v", lookID, err)
		}
		outfit, err := store.GetOutfit(*look.OutfitID, false)
		if err != nil {
			t.Fatal(err)
		}
		return outfit
	}
	watches := outfitOf(first)
	if outfitOf(otherWatch).ID != watches.ID || strings.Join(watches.WornOn, ",") != "2026-11-01,2026-11-02" || len(watches.PossessionIDs) != 4 ||
		!contains(watches.PossessionIDs, steelWatch) || !contains(watches.PossessionIDs, goldWatch) {
		t.Fatalf("the same shirt and jeans with another watch is one outfit worn twice, holding both watches: %+v", watches)
	}
	jacketOutfit := outfitOf(withJacket)
	if outfitOf(noBottomInView).ID != jacketOutfit.ID || strings.Join(jacketOutfit.WornOn, ",") != "2026-11-03,2026-11-04" || len(jacketOutfit.PossessionIDs) != 4 {
		t.Fatalf("a look with no bottom in view joins on its top and jacket: %+v", jacketOutfit)
	}
	if green := outfitOf(otherTop); green.ID == watches.ID || strings.Join(green.WornOn, ",") != "2026-11-05" {
		t.Fatalf("another top is another outfit: %+v", green)
	}
	// One look read later joins the outfit an earlier rebuild made, and the
	// undo takes back only the piece it added.
	later := addLook(t, store, "2026-11-06", map[string]string{whiteShirt: "garment:top", jeans: "garment:bottom", ring: "jewelry:ring"})
	again, err := store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["looks_joined_earlier_outfits"] != 1 || outfitOf(later).ID != watches.ID || len(outfitOf(later).PossessionIDs) != 5 {
		t.Fatalf("again = %+v, outfit %+v", again.Counts, outfitOf(later))
	}
	if _, err := store.UndoOutfitRebuild(ctx, again.Actions[len(again.Actions)-1].Records[0], "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if back := outfitOf(first); len(back.PossessionIDs) != 4 || strings.Join(back.WornOn, ",") != "2026-11-01,2026-11-02" {
		t.Fatalf("after the undo: %+v", back)
	}
}

func TestALookWithNoTopOnIsNeverAnOutfit(t *testing.T) {
	store := openAuditStore(t)
	ctx := context.Background()
	scripted := &scriptedModels{answers: map[string]any{}, respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	piece := func(text string) string {
		made, err := store.CreatePossession(ctx, fields(t, text), "principal_000001")
		if err != nil {
			t.Fatal(err)
		}
		return made["id"].(string)
	}
	shirt := piece(`{"name":"white shirt","garment":{"category":"top","garment_type":"t_shirt"}}`)
	jeans := piece(`{"name":"jeans","garment":{"category":"bottom","garment_type":"jeans"}}`)
	watch := piece(`{"name":"steel watch","watch":{}}`)

	// An old outfit reading of a body check photo made an outfit.
	bodyCheck := readOutfit(t, reviewer, scripted, "photo_000001", cap("cap"))
	oldOutfit := *bodyCheck.OutfitID
	noTopSlot := addLook(t, store, "2026-11-01", map[string]string{jeans: "garment:bottom", watch: "watch"})
	if _, err := store.db.Exec(`INSERT INTO outfit_look_photos (look_id, photo_id, photo_review_id) VALUES (?, 'photo_000001', ?)`, noTopSlot, bodyCheck.ID); err != nil {
		t.Fatal(err)
	}
	// The readers said no top, though a slot reads as one.
	saidNoTop := addLook(t, store, "2026-11-02", map[string]string{shirt: "garment:top", jeans: "garment:bottom"})
	store.db.Exec(`UPDATE outfit_looks SET wearer_has_no_top = 1 WHERE id = ?`, saidNoTop)
	dressed := addLook(t, store, "2026-11-03", map[string]string{shirt: "garment:top", jeans: "garment:bottom"})

	rebuilt, err := store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Counts["looks_not_outfits"] != 2 || rebuilt.Counts["outfits_made"] != 1 || rebuilt.Counts["outfits_replaced"] != 1 {
		t.Fatalf("rebuilt = %+v", rebuilt.Counts)
	}
	for _, lookID := range []string{noTopSlot, saidNoTop} {
		look, _ := store.GetLook(lookID)
		if look.OutfitID != nil || !strings.Contains(look.NotAnOutfit, "body check") {
			t.Fatalf("%s should be kept, marked not an outfit: %+v", lookID, look)
		}
	}
	if gone, err := store.GetOutfit(oldOutfit, true); err != nil || gone.DeletedAt == 0 || gone.MergedIntoID != nil {
		t.Fatalf("the outfit made only from a body check should be removed: %+v, %v", gone, err)
	}
	if review, _ := store.GetPhotoReview(bodyCheck.ID); review.OutfitID != nil {
		t.Fatalf("the body check's reading should point at no outfit: %v", *review.OutfitID)
	}
	if look, _ := store.GetLook(dressed); look.OutfitID == nil || look.NotAnOutfit != "" {
		t.Fatalf("the dressed look is an outfit: %+v", look)
	}
	if _, err := store.UndoOutfitRebuild(ctx, rebuilt.Actions[len(rebuilt.Actions)-1].Records[0], "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if back, _ := store.GetOutfit(oldOutfit, false); back.ID != oldOutfit {
		t.Fatalf("the undo should bring the old outfit back")
	}
	if review, _ := store.GetPhotoReview(bodyCheck.ID); review.OutfitID == nil || *review.OutfitID != oldOutfit {
		t.Fatalf("the undo should point the reading back")
	}
	if look, _ := store.GetLook(noTopSlot); look.NotAnOutfit != "" {
		t.Fatalf("the undo should clear the mark: %+v", look)
	}

	// A look an earlier rebuild placed is taken out when it turns out to
	// have no top on, and its outfit, worn only as that look, goes.
	rebuilt, err = store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	placed, _ := store.GetLook(dressed)
	store.db.Exec(`UPDATE outfit_looks SET wearer_has_no_top = 1 WHERE id = ?`, dressed)
	again, err := store.RebuildOutfitsFromLooks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["looks_not_outfits"] != 1 || again.Counts["outfits_replaced"] != 1 {
		t.Fatalf("again = %+v", again.Counts)
	}
	if gone, _ := store.GetOutfit(*placed.OutfitID, true); gone.DeletedAt == 0 {
		t.Fatalf("an outfit left with no look should be removed: %+v", gone)
	}
	if _, err := store.UndoOutfitRebuild(ctx, again.Actions[len(again.Actions)-1].Records[0], "principal_000001"); err != nil {
		t.Fatal(err)
	}
	if back, _ := store.GetLook(dressed); back.OutfitID == nil || *back.OutfitID != *placed.OutfitID || back.NotAnOutfit != "" {
		t.Fatalf("after the undo the look is back in its outfit: %+v", back)
	}
	if outfit, _ := store.GetOutfit(*placed.OutfitID, false); len(outfit.PossessionIDs) != 2 || strings.Join(outfit.WornOn, ",") != "2026-11-03" {
		t.Fatalf("after the undo the outfit is whole: %+v", outfit)
	}
}
