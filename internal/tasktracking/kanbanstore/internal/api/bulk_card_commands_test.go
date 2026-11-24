package api_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

func runBulk(t *testing.T, h http.Handler, who map[string]string, request model.BulkCardCommandRequest) model.BulkCardCommandResponse {
	t.Helper()
	w := requestAs(t, h, who, "POST", "/api/bulk-card-commands", request)
	mustStatus(t, w, 200, "bulk "+string(request.Command))
	var response model.BulkCardCommandResponse
	decodeSuccessfulResponse(t, w, &response)
	return response
}

func statusesOf(response model.BulkCardCommandResponse) map[string]int {
	statuses := map[string]int{}
	for _, result := range response.Results {
		statuses[result.CardID] = result.Status
	}
	return statuses
}

// alice edits Support and cannot see Finance. One bulk move names a card she
// may move, one she cannot see, and one that also sits on Finance. Each must
// get exactly the answer the single-card route would have given her, and the
// refusals must undo nothing.
func TestABulkCommandJudgesEachCardAsTheSingleCardRouteWould(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	w := requestAs(t, h, asService, "POST", "/api/boards/"+f.supportBoardID+"/columns", model.CreateColumnRequest{Name: "Resolved"})
	mustStatus(t, w, 201, "a second Support column")
	var resolved model.Column
	decodeSuccessfulResponse(t, w, &resolved)

	response := runBulk(t, h, asPrincipal(alice), model.BulkCardCommandRequest{
		CardIDs: []string{f.supportCardID, f.financeCardID, f.sharedCardID},
		Command: model.BulkCommandMove,
		Move:    &model.MoveCardRequest{BoardID: f.supportBoardID, ColumnID: resolved.ID},
	})
	want := map[string]int{f.supportCardID: 200, f.financeCardID: 404, f.sharedCardID: 403}
	if got := statusesOf(response); !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if response.Succeeded != 1 || response.Failed != 2 {
		t.Errorf("succeeded %d failed %d, want 1 and 2", response.Succeeded, response.Failed)
	}

	// The card that moved did move, and its timeline names alice, not a
	// service: the event is the single-card route's own.
	w = requestAs(t, h, asService, "GET", "/api/cards/"+f.supportCardID+"/events", nil)
	mustStatus(t, w, 200, "events")
	var events []model.CardEvent
	decodeSuccessfulResponse(t, w, &events)
	moved := false
	for _, event := range events {
		if event.Kind == model.EventCardMoved && event.ToColumnID == resolved.ID {
			moved = true
			if event.Actor != alice {
				t.Errorf("the move's actor is %q, want %s", event.Actor, alice)
			}
		}
	}
	if !moved {
		t.Errorf("no card_moved event into Resolved: %+v", events)
	}
	// The refused shared card is where it was.
	w = requestAs(t, h, asService, "GET", "/api/cards/"+f.sharedCardID+"/placements", nil)
	var placements []model.Placement
	decodeSuccessfulResponse(t, w, &placements)
	for _, placement := range placements {
		if placement.ColumnID == resolved.ID {
			t.Errorf("the shared card moved, though alice was refused")
		}
	}
}

// A column's WIP limit binds a bulk move one card at a time: the cards that
// fit go in, and the one that does not is told why.
func TestABulkMoveStopsAtTheColumnsLimit(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	limit := 1
	w := requestAs(t, h, asService, "POST", "/api/boards/"+f.supportBoardID+"/columns", model.CreateColumnRequest{Name: "In progress", WIPLimit: &limit})
	mustStatus(t, w, 201, "a column that holds one card")
	var narrow model.Column
	decodeSuccessfulResponse(t, w, &narrow)

	response := runBulk(t, h, asService, model.BulkCardCommandRequest{
		CardIDs: []string{f.supportCardID, f.sharedCardID},
		Command: model.BulkCommandMove,
		Move:    &model.MoveCardRequest{BoardID: f.supportBoardID, ColumnID: narrow.ID},
	})
	want := map[string]int{f.supportCardID: 200, f.sharedCardID: 409}
	if got := statusesOf(response); !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

// noteboard's PATCH replaces the whole tag list. Adding a tag to many cards
// must leave each card's other tags alone.
func TestBulkTagChangesLeaveACardsOtherTagsAlone(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asService, "PATCH", "/api/cards/"+f.supportCardID, map[string]any{"tags": []string{"hardware", "urgent"}}), 200, "seed tags")

	tagsOf := func(cardID string) []string {
		w := requestAs(t, h, asService, "GET", "/api/cards/"+cardID, nil)
		mustStatus(t, w, 200, "read card")
		var detail model.CardDetail
		decodeSuccessfulResponse(t, w, &detail)
		tags := []string{}
		raw, _ := detail.Item.(map[string]any)["tags"].([]any)
		for _, tag := range raw {
			tags = append(tags, tag.(string))
		}
		return tags
	}

	added := runBulk(t, h, asPrincipal(alice), model.BulkCardCommandRequest{
		CardIDs: []string{f.supportCardID}, Command: model.BulkCommandAddTags, Tags: []string{"billing", "urgent"},
	})
	if added.Succeeded != 1 {
		t.Fatalf("add_tags: %+v", added.Results)
	}
	if got, want := tagsOf(f.supportCardID), []string{"hardware", "urgent", "billing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after add_tags: %v, want %v (kept, and the tag already held not doubled)", got, want)
	}
	runBulk(t, h, asPrincipal(alice), model.BulkCardCommandRequest{
		CardIDs: []string{f.supportCardID}, Command: model.BulkCommandRemoveTags, Tags: []string{"urgent"},
	})
	if got, want := tagsOf(f.supportCardID), []string{"hardware", "billing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after remove_tags: %v, want %v", got, want)
	}
}

func TestBulkAssignAndHoldReachEveryCard(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	cards := []string{f.supportCardID, f.sharedCardID}
	assigned := runBulk(t, h, asService, model.BulkCardCommandRequest{CardIDs: cards, Command: model.BulkCommandAssign, PrincipalID: carol})
	if assigned.Succeeded != 2 {
		t.Fatalf("assign: %+v", assigned.Results)
	}
	w := requestAs(t, h, asService, "GET", "/api/assignments?principal_id="+carol, nil)
	mustStatus(t, w, 200, "carol's cards")
	var mine []map[string]any
	decodeSuccessfulResponse(t, w, &mine)
	if len(mine) != 2 {
		t.Errorf("carol is on %d cards, want 2", len(mine))
	}
	held := runBulk(t, h, asService, model.BulkCardCommandRequest{CardIDs: cards, Command: model.BulkCommandHold, Reason: "waiting on the vendor"})
	if held.Succeeded != 2 {
		t.Fatalf("hold: %+v", held.Results)
	}
	// An unknown principal is refused per card, by principal-store's answer.
	unknown := runBulk(t, h, asService, model.BulkCardCommandRequest{CardIDs: cards, Command: model.BulkCommandAssign, PrincipalID: "principal_999999"})
	if unknown.Failed != 2 || unknown.Results[0].Status != 400 {
		t.Errorf("assigning a principal that does not exist: %+v", unknown.Results)
	}
}

// A request that is wrong as a whole does nothing to any card.
func TestAMalformedBulkRequestIsRefusedWhole(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	one := []string{f.supportCardID}
	tooMany := make([]string, model.MaxBulkCardCommandCards+1)
	for i := range tooMany {
		tooMany[i] = "card-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	for what, request := range map[string]model.BulkCardCommandRequest{
		"no cards":               {Command: model.BulkCommandUnhold},
		"an unknown command":     {CardIDs: one, Command: "archive"},
		"a move with no move":    {CardIDs: one, Command: model.BulkCommandMove},
		"an assign with a move":  {CardIDs: one, Command: model.BulkCommandAssign, PrincipalID: carol, Move: &model.MoveCardRequest{BoardID: f.supportBoardID, ColumnID: f.supportColumnID}},
		"a name for a principal": {CardIDs: one, Command: model.BulkCommandAssign, PrincipalID: "carol"},
		"a card named twice":     {CardIDs: []string{f.supportCardID, f.supportCardID}, Command: model.BulkCommandUnhold},
		"a path for a card id":   {CardIDs: []string{"x/../../boards"}, Command: model.BulkCommandUnhold},
		"too many cards":         {CardIDs: tooMany, Command: model.BulkCommandUnhold},
	} {
		mustStatus(t, requestAs(t, h, asService, "POST", "/api/bulk-card-commands", request), 400, what)
	}

	w := requestAs(t, h, asPrincipal(alice), "GET", "/api/bulk-card-commands", nil)
	mustStatus(t, w, 200, "the vocabulary")
	var options model.BulkCardCommandOptions
	decodeSuccessfulResponse(t, w, &options)
	if !reflect.DeepEqual(options.Commands, model.BulkCardCommands) || options.MaxCards != model.MaxBulkCardCommandCards {
		t.Errorf("options = %+v", options)
	}
}
