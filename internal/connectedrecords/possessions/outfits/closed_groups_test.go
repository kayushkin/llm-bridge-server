package outfits

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/possessionstest"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions/review"
)

func watchPiece(label string) review.FinalItem {
	return review.FinalItem{ReaderItem: item(label, label, "", "watch", possessions.Box{Left: 0.6, Top: 0.6, Right: 0.66, Bottom: 0.66}, review.FieldValue{Name: "movement", Value: "quartz"})}
}

// ownedWatch records a watch the owner has, with these photos.
func ownedWatch(t *testing.T, store *Store, name string, photoIDs ...string) string {
	t.Helper()
	photos := []string{}
	for _, photoID := range photoIDs {
		photos = append(photos, fmt.Sprintf(`{"photo_id":%q}`, photoID))
	}
	made, err := store.CreatePossession(context.Background(), possessionstest.Fields(t, `{"name":"`+name+`","watch":{"movement":"quartz"},"photos":[`+strings.Join(photos, ",")+`]}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	return made["id"].(string)
}

var shownPossession = regexp.MustCompile(`— (possession_\d+):`)

// pickWith answers every picture match with pick(role, the ids shown,
// prompt), and falls through for anything else.
func pickWith(pick func(role string, shown []string, prompt string) string) func(role, step, prompt string, pictures int) (any, bool) {
	return func(role, step, prompt string, pictures int) (any, bool) {
		if step != "match" {
			return nil, false
		}
		shown := []string{}
		for _, found := range shownPossession.FindAllStringSubmatch(prompt, -1) {
			shown = append(shown, found[1])
		}
		return map[string]string{"match": pick(role, shown, prompt), "reason": "the dial"}, true
	}
}

func closeGroup(t *testing.T, store *Store, group string) {
	t.Helper()
	if _, err := store.SetWearableGroupClosed(context.Background(), group, true, "principal_000001"); err != nil {
		t.Fatal(err)
	}
}

func TestAClosedGroupNeverRecordsANewPiece(t *testing.T) {
	store := storeOver(possessionstest.OpenAuditStore(t))
	ctx := context.Background()
	first := ownedWatch(t, store, "steel watch", "photo_000010", "photo_000011")
	second := ownedWatch(t, store, "gold watch", "photo_000012")
	third := ownedWatch(t, store, "black watch", "photo_000013")
	if _, err := store.SetWearableGroupClosed(ctx, "watch:steel", true, "principal_000001"); err == nil {
		t.Fatalf("a group that cannot exist should be refused")
	}
	closeGroup(t, store, "watch")
	groups, _ := store.ListWearableGroups()
	if len(groups) != 1 || !groups[0].Closed || groups[0].Owned != 3 || groups[0].ID == nil {
		t.Fatalf("groups = %+v", groups)
	}

	var picturesMutex sync.Mutex
	pictures := []int{}
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{}}
	scripted.Respond = func(role, step, prompt string, count int) (any, bool) {
		if step == "match" {
			picturesMutex.Lock()
			pictures = append(pictures, count)
			picturesMutex.Unlock()
		}
		return pickWith(func(role string, shown []string, prompt string) string {
			if role == "first" {
				return first
			}
			return "cannot_tell"
		})(role, step, prompt, count)
	}
	reviewer := auditReviewer(t, store, scripted, nil)
	photoReview := readOutfit(t, reviewer, scripted, "photo_000001", watchPiece("watch"))
	if pictures[0] != 1+2+1+1 {
		t.Fatalf("the readers should see the piece and every photo of every member: %d pictures", pictures[0])
	}
	decisions, _ := store.ListQuestions(false, photoReview.ID, "decision", 0, 0)
	if len(decisions) != 1 || len(decisions[0].Choices) != 4 || decisions[0].AskedBy != "picture_match" {
		t.Fatalf("decisions = %+v", decisions)
	}
	for index, want := range []string{first, second, third} {
		if decisions[0].Choices[index].Resolution.PossessionID != want {
			t.Fatalf("choice %d = %+v", index, decisions[0].Choices[index])
		}
	}
	for _, choice := range decisions[0].Choices {
		if choice.Resolution.Action == "accept" {
			t.Fatalf("a closed group offered a new piece: %+v", choice)
		}
	}
	if watches, _ := store.ListPossessions(possessions.PossessionFilter{Kind: "watch"}); len(watches) != 3 {
		t.Fatalf("no new watch should be recorded: %d", len(watches))
	}

	scripted.Respond = pickWith(func(role string, shown []string, prompt string) string { return second })
	again := readOutfit(t, reviewer, scripted, "photo_000002", watchPiece("watch"))
	if pieceOf(t, store, again, "watch") != second {
		t.Fatalf("both readers picked the gold watch")
	}
}

func TestAClosedGroupTooBigForOneCallIsShownInBatches(t *testing.T) {
	store := storeOver(possessionstest.OpenAuditStore(t))
	members := []string{}
	for number := 1; number <= 6; number++ {
		members = append(members, ownedWatch(t, store, fmt.Sprintf("watch %d", number), "photo_000011", "photo_000012", "photo_000013", "photo_000014", "photo_000015", "photo_000016"))
	}
	closeGroup(t, store, "watch")
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{}}
	scripted.Respond = pickWith(func(role string, shown []string, prompt string) string {
		if slices.Contains(shown, members[4]) {
			return members[4]
		}
		return "not_here"
	})
	reviewer := auditReviewer(t, store, scripted, nil)
	photoReview := readOutfit(t, reviewer, scripted, "photo_000001", watchPiece("watch"))
	if scripted.Calls["first match"] != 2 || pieceOf(t, store, photoReview, "watch") != members[4] {
		t.Fatalf("calls %v; the fifth watch should be found across two batches", scripted.Calls)
	}
}

func TestRematchSettlesStuckPiecesAndFoldsUnconfirmedIntoAClosedGroup(t *testing.T) {
	store := storeOver(possessionstest.OpenAuditStore(t))
	ctx := context.Background()
	steel := ownedWatch(t, store, "steel watch", "photo_000010")
	gold := ownedWatch(t, store, "gold watch", "photo_000011")
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{}, Respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	// Before the group is closed, two selfies make two unconfirmed watches.
	firstUnconfirmed := pieceOf(t, store, readOutfit(t, reviewer, scripted, "photo_000001", watchPiece("wrist watch")), "wrist watch")
	secondUnconfirmed := pieceOf(t, store, readOutfit(t, reviewer, scripted, "photo_000002", watchPiece("other wrist watch")), "other wrist watch")
	// A jacket recorded, then a selfie where the readers differ on it.
	jacketReview := readOutfit(t, reviewer, scripted, "photo_000003", jacket("jacket"))
	jacketID := pieceOf(t, store, jacketReview, "jacket")
	scripted.Respond = pickWith(func(role string, shown []string, prompt string) string {
		if role == "first" && strings.Contains(prompt, "new photo: jacket") {
			return jacketID
		}
		return "none"
	})
	stuck := readOutfit(t, reviewer, scripted, "photo_000004", jacket("jacket"), bottoms("jeans"))
	if stuck.Status != "waiting_for_owner" {
		t.Fatalf("the jacket should wait on a question: %s", stuck.Status)
	}
	stuckJacket := stuck.Sightings[0]
	stillStuck := readOutfit(t, reviewer, scripted, "photo_000005", jacket("jacket again"))

	closeGroup(t, store, "watch")
	// Both readers now agree on the first stuck jacket and still differ on
	// the second; of the unconfirmed watches, they agree on the first.
	scripted.Respond = pickWith(func(role string, shown []string, prompt string) string {
		switch {
		case strings.Contains(prompt, "new photo: jacket ("):
			return jacketID
		case strings.Contains(prompt, "new photo: jacket again ("):
			if role == "first" {
				return jacketID
			}
			return "none"
		case strings.Contains(prompt, "recorded as "+firstUnconfirmed+":"):
			return steel
		case strings.Contains(prompt, "recorded as "+secondUnconfirmed+":") && role == "first":
			return steel
		case strings.Contains(prompt, "recorded as "+secondUnconfirmed+":"):
			return gold
		}
		return "unexpected"
	})
	run, err := reviewer.RematchOutfitPieces(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["matched"] != 1 || run.Counts["disagreed"] != 1 || run.Counts["merged"] != 1 || run.Counts["asked"] != 1 || len(run.Failures) != 0 || run.CostUSD == 0 {
		t.Fatalf("run = %+v", run)
	}
	settled := mustSighting(t, store, stuckJacket.ID)
	if settled.Status != "accepted" || *settled.PossessionID != jacketID {
		t.Fatalf("the stuck jacket should be the recorded jacket: %+v", settled)
	}
	answered, _ := store.GetQuestion(settled.QuestionIDs[0])
	if answered.AnsweredBy != "picture_match" || answered.AnswerChoice == nil || *answered.AnswerChoice != 0 || len(answered.Resolution) == 0 {
		t.Fatalf("its question should be answered by the match: %+v", answered)
	}
	photoReview, _ := store.GetPhotoReview(stuck.ID)
	outfit, _ := store.GetOutfit(*photoReview.OutfitID, false)
	if !slices.Contains(outfit.PossessionIDs, jacketID) || photoReview.Status != "done" {
		t.Fatalf("the jacket should join its outfit (%v) and the review be done (%s)", outfit.PossessionIDs, photoReview.Status)
	}
	if open := mustSighting(t, store, stillStuck.Sightings[0].ID); open.Status != "proposed" {
		t.Fatalf("a piece the readers still differ on stays proposed: %s", open.Status)
	}

	if gone, _ := store.GetPossession(firstUnconfirmed, true); gone["merged_into_id"] != steel {
		t.Fatalf("the first unconfirmed watch should be merged into the steel one: %v", gone["merged_into_id"])
	}
	var question review.OwnerQuestion
	for _, action := range run.Actions {
		if action.Action == "asked" {
			question, _ = store.GetQuestion(action.QuestionID)
		}
	}
	if question.AskedBy != "closed_group" || len(question.Choices) != 3 || question.Choices[1].Resolution.IntoPossessionID != gold || question.Choices[2].Resolution.Action != "keep_apart" {
		t.Fatalf("question = %+v", question)
	}
	again, err := reviewer.RematchOutfitPieces(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts["merged"] != 0 || again.Counts["asked"] != 0 {
		t.Fatalf("a second pass should leave the asked watch to the owner: %+v", again)
	}
	choose(t, reviewer, question.ID, 1)
	if gone, _ := store.GetPossession(secondUnconfirmed, true); gone["merged_into_id"] != gold {
		t.Fatalf("after the answer the second watch is the gold one: %v", gone["merged_into_id"])
	}
	if groups, _ := store.ListWearableGroups(); groups[len(groups)-1].Group != "watch" || groups[len(groups)-1].Unconfirmed != 0 || groups[len(groups)-1].Owned != 2 {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestTheDuplicateAndLinkChecksKeepAClosedGroupClosed(t *testing.T) {
	store := storeOver(possessionstest.OpenAuditStore(t))
	ctx := context.Background()
	steel := ownedWatch(t, store, "steel watch", "photo_000010")
	gold := ownedWatch(t, store, "gold watch", "photo_000011")
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{}, Respond: matchByLabel(nil)}
	reviewer := auditReviewer(t, store, scripted, nil)
	unconfirmed := pieceOf(t, store, readOutfit(t, reviewer, scripted, "photo_000001", watchPiece("wrist watch")), "wrist watch")
	scripted.Respond = matchByLabel(map[string]string{"steel": steel})
	linked := readOutfit(t, reviewer, scripted, "photo_000002", watchPiece("steel"))
	if pieceOf(t, store, linked, "steel") != steel {
		t.Fatalf("the steel watch should be linked")
	}
	closeGroup(t, store, "watch")

	scripted.Respond = func(role, step, prompt string, pictures int) (any, bool) {
		switch step {
		case "duplicates":
			return map[string]any{"clusters": []any{}}, true
		case "match":
			return map[string]string{"match": gold, "reason": "gold case"}, true
		}
		return nil, false
	}
	run, err := reviewer.CheckWearableDuplicates(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["merged"] != 1 || len(run.Failures) != 0 {
		t.Fatalf("run = %+v", run)
	}
	if gone, _ := store.GetPossession(unconfirmed, true); gone["merged_into_id"] != gold {
		t.Fatalf("the unconfirmed watch should be merged into the gold one: %v", gone["merged_into_id"])
	}

	scripted.Respond = linkAnswers(steel, map[string][]map[string]any{
		"first":  {{"crop": "C1", "same_piece": false, "reason": "other dial", "other_possession_ids": []string{}}},
		"second": {{"crop": "C1", "same_piece": true, "reason": "same dial", "other_possession_ids": []string{}}},
	})
	links, err := reviewer.CheckWearableLinks(ctx, "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	if links.Counts["asked"] != 1 {
		t.Fatalf("links = %+v", links)
	}
	question, _ := store.GetQuestion(links.Actions[0].QuestionID)
	actions := []string{}
	for _, choice := range question.Choices {
		actions = append(actions, choice.Resolution.Action+" "+choice.Resolution.PossessionID)
	}
	if strings.Join(actions, ", ") != "keep_link "+steel+", same_as "+gold+", reject " {
		t.Fatalf("a closed group's link question offers the other members and never a new piece: %v", actions)
	}
}

func TestANewPieceIsRefusedOnceItsGroupIsClosed(t *testing.T) {
	store := storeOver(possessionstest.OpenAuditStore(t))
	ctx := context.Background()
	steel := ownedWatch(t, store, "steel watch", "photo_000010")
	scripted := &possessionstest.ScriptedModels{Answers: map[string]any{}}
	scripted.Respond = pickWith(func(role string, shown []string, prompt string) string {
		if role == "first" {
			return steel
		}
		return "none"
	})
	reviewer := auditReviewer(t, store, scripted, nil)
	photoReview := readOutfit(t, reviewer, scripted, "photo_000001", watchPiece("watch"))
	decisions, _ := store.ListQuestions(false, photoReview.ID, "decision", 0, 0)
	if len(decisions) != 1 || decisions[0].Choices[1].Resolution.Action != "accept" {
		t.Fatalf("decisions = %+v", decisions)
	}
	closeGroup(t, store, "watch")
	asNew := 1
	if _, err := reviewer.AnswerQuestion(ctx, decisions[0].ID, review.Answer{Choice: &asNew, AnsweredBy: "principal_000001"}); !errors.Is(err, possessions.ErrConflict) {
		t.Fatalf("a new watch after the group is closed = %v, want a conflict", err)
	}
	if watches, _ := store.ListPossessions(possessions.PossessionFilter{Kind: "watch"}); len(watches) != 1 {
		t.Fatalf("watches = %d", len(watches))
	}
}
