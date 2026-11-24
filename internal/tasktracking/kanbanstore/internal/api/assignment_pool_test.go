package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// setupWithPrincipals is the harness with the fake principal-store in the
// test's hands, so a test can say who in a pool is available.
func setupWithPrincipals(t *testing.T) (http.Handler, *fakeNoteboard, *fakePrincipalStore) {
	t.Helper()
	principals := newFakePrincipalStore()
	principalServer := httptest.NewServer(principals.handler())
	t.Cleanup(principalServer.Close)
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	t.Cleanup(bridge.Close)
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	t.Cleanup(bundles.Close)
	h, nb, _, cleanup := setupWithOwners(t, principalServer.URL, bridge.URL, bundles.URL)
	t.Cleanup(cleanup)
	return h, nb, principals
}

func pool(strategy model.AssignmentStrategy) *model.AssignmentPool {
	return &model.AssignmentPool{PrincipalID: poolGroup, Strategy: strategy}
}

// createCard creates a card in the column and returns its view.
func createCard(t *testing.T, h http.Handler, boardID, columnID, title string, tags ...string) model.CardView {
	t.Helper()
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: title, ColumnID: columnID, Tags: tags})
	if w.Code != 201 {
		t.Fatalf("create %q: %d %s", title, w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	return cv
}

// assignByHand puts a principal on a fresh card in the column.
func assignByHand(t *testing.T, h http.Handler, boardID, columnID, principalID string) {
	t.Helper()
	cv := createCard(t, h, boardID, columnID, "already "+principalID+"'s")
	if w := do(t, h, "PUT", "/api/cards/"+cv.Placement.CardID+"/assignments/"+principalID, nil); w.Code != 201 {
		t.Fatalf("assign by hand: %d %s", w.Code, w.Body.String())
	}
}

func onlyAssignee(t *testing.T, cv model.CardView) string {
	t.Helper()
	if len(cv.Assignments) != 1 {
		t.Fatalf("want exactly one assignee, got %+v", cv.Assignments)
	}
	return cv.Assignments[0].PrincipalID
}

func assignedDetail(t *testing.T, h http.Handler, cardID string) map[string]any {
	t.Helper()
	events := eventsOfKind(t, h, cardID, model.EventAssigned)
	if len(events) != 1 {
		t.Fatalf("want one assigned event, got %d", len(events))
	}
	var detail map[string]any
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestTheAssignmentStrategiesAreServed(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	w := do(t, h, "GET", "/api/assignment-strategies", nil)
	var strategies []model.AssignmentStrategy
	decodeSuccessfulResponse(t, w, &strategies)
	if len(strategies) != 2 || strategies[0] != model.AssignmentStrategyLeastOpenCards || strategies[1] != model.AssignmentStrategyRoundRobin {
		t.Fatalf("strategies: %v", strategies)
	}
}

func TestAnAssignmentPoolRoundTripsClearsAndMustBeAnActiveGroup(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	boardID := mkBoard(t, h, "Pool")

	for _, refused := range []struct {
		pool model.AssignmentPool
		says string
	}{
		{model.AssignmentPool{PrincipalID: activePrincipal, Strategy: model.AssignmentStrategyRoundRobin}, "is a human"},
		{model.AssignmentPool{PrincipalID: unknownPrincipal, Strategy: model.AssignmentStrategyRoundRobin}, "does not exist"},
		{model.AssignmentPool{PrincipalID: poolGroup, Strategy: "busiest_first"}, "is not one of"},
		{model.AssignmentPool{PrincipalID: poolGroup}, "is not one of"},
		{model.AssignmentPool{Strategy: model.AssignmentStrategyRoundRobin}, "principal_id is required"},
	} {
		w := patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: &refused.pool})
		if w.Code != 400 || !strings.Contains(w.Body.String(), refused.says) || !strings.Contains(w.Body.String(), "assignment_pool") {
			t.Errorf("%+v: %d %s", refused.pool, w.Code, w.Body.String())
		}
	}
	if b := getBoard(t, h, boardID); b.AssignmentPool != nil {
		t.Fatalf("a refused pool was written: %+v", b.AssignmentPool)
	}

	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyLeastOpenCards)}); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if b := getBoard(t, h, boardID); b.AssignmentPool == nil || *b.AssignmentPool != *pool(model.AssignmentStrategyLeastOpenCards) {
		t.Fatalf("read back %+v", b.AssignmentPool)
	}
	// A PATCH that does not mention the pool leaves it alone.
	patchBoard(t, h, boardID, model.UpdateBoardRequest{Name: str("Pool, renamed")})
	if b := getBoard(t, h, boardID); b.AssignmentPool == nil {
		t.Fatal("an unrelated PATCH cleared the pool")
	}
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: &model.AssignmentPool{}}); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if b := getBoard(t, h, boardID); b.AssignmentPool != nil {
		t.Fatalf("an empty object should clear the pool: %+v", b.AssignmentPool)
	}

	principals.disabledAtByID[poolGroup] = 1_757_000_000
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin)}); w.Code != 400 || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("a disabled group: %d %s", w.Code, w.Body.String())
	}
}

// The count is of open cards: a card in a column that stops the clock is
// finished, so member B with two finished cards is less loaded than member A
// with one open one.
func TestThePoolHandsANewCardToTheAvailableMemberWithFewestOpenCards(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	principals.availableMembersByGroup[poolGroup] = []string{activePrincipal, otherActivePrincipal}
	boardID := mkBoard(t, h, "Desk")
	open := mkColumn(t, h, boardID, "Open", "")
	stopped := model.ClockStopped
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "Done", BudgetClockState: &stopped})
	var done model.Column
	decodeSuccessfulResponse(t, w, &done)
	assignByHand(t, h, boardID, open, activePrincipal)
	assignByHand(t, h, boardID, done.ID, otherActivePrincipal)
	assignByHand(t, h, boardID, done.ID, otherActivePrincipal)
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyLeastOpenCards), DefaultPrincipalID: str("principal_000004")})

	cv := createCard(t, h, boardID, open, "new mail")
	if got := onlyAssignee(t, cv); got != otherActivePrincipal {
		t.Fatalf("want %s (0 open cards) over %s (1), got %s", otherActivePrincipal, activePrincipal, got)
	}
	detail := assignedDetail(t, h, cv.Placement.CardID)
	if detail["source"] != "pool" || detail["strategy"] != "least_open_cards" || detail["candidates"] != float64(2) || detail["pool_principal_id"] != poolGroup {
		t.Fatalf("the event should say the pool chose, how, and among how many: %v", detail)
	}
}

// Availability is principal-store's answer. A member it leaves out — on time
// off, outside their hours — is never chosen, however idle.
func TestThePoolNeverChoosesAMemberPrincipalStoreCallsUnavailable(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	principals.availableMembersByGroup[poolGroup] = []string{activePrincipal}
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	assignByHand(t, h, boardID, columnID, activePrincipal)
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyLeastOpenCards)})

	if got := onlyAssignee(t, createCard(t, h, boardID, columnID, "new mail")); got != activePrincipal {
		t.Fatalf("the only available member should get it, got %s", got)
	}
}

func TestRoundRobinChoosesTheMemberWhoseNewestAssignmentIsOldest(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	principals.availableMembersByGroup[poolGroup] = []string{activePrincipal, otherActivePrincipal, "principal_000004"}
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	// principal_000004 has never been assigned here, so it goes first.
	assignByHand(t, h, boardID, columnID, activePrincipal)
	assignByHand(t, h, boardID, columnID, otherActivePrincipal)
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin)})

	var got []string
	for range 4 {
		got = append(got, onlyAssignee(t, createCard(t, h, boardID, columnID, "next")))
	}
	want := []string{"principal_000004", activePrincipal, otherActivePrincipal, "principal_000004"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("round robin order: got %v, want %v", got, want)
	}
}

func TestAnEmptyPoolFallsToTheBoardDefaultAndSaysSo(t *testing.T) {
	h, _, _ := setupWithPrincipals(t)
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin), DefaultPrincipalID: str(activePrincipal)})

	cv := createCard(t, h, boardID, columnID, "3am mail")
	if got := onlyAssignee(t, cv); got != activePrincipal {
		t.Fatalf("want the board default, got %s", got)
	}
	detail := assignedDetail(t, h, cv.Placement.CardID)
	if detail["source"] != "board_default" || detail["pool_empty"] != true {
		t.Fatalf("the event should say the pool was empty: %v", detail)
	}
}

func TestAnEmptyPoolWithNoDefaultLeavesTheCardUnassignedAndRecordsWhy(t *testing.T) {
	h, _, _ := setupWithPrincipals(t)
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin)})

	cv := createCard(t, h, boardID, columnID, "3am mail")
	if len(cv.Assignments) != 0 {
		t.Fatalf("nobody available and no default: the card should arrive unassigned, got %+v", cv.Assignments)
	}
	skipped := eventsOfKind(t, h, cv.Placement.CardID, model.EventAssignmentSkipped)
	if len(skipped) != 1 {
		t.Fatalf("want one assignment_skipped event, got %d", len(skipped))
	}
	var detail map[string]any
	json.Unmarshal(skipped[0].Detail, &detail)
	if detail["reason"] != "pool_empty" || skipped[0].BoardID != boardID {
		t.Fatalf("the skip should name its reason and board: %+v %v", skipped[0], detail)
	}
}

// Without a pool nothing changes: a board with no default still creates
// unassigned cards and records no skip.
func TestABoardWithoutAPoolRecordsNoSkip(t *testing.T) {
	h, _, _ := setupWithPrincipals(t)
	boardID := mkBoard(t, h, "Plain")
	columnID := mkColumn(t, h, boardID, "Open", "")
	cv := createCard(t, h, boardID, columnID, "plain")
	if len(cv.Assignments) != 0 || len(eventsOfKind(t, h, cv.Placement.CardID, model.EventAssignmentSkipped)) != 0 {
		t.Fatalf("a board with no pool and no default should do nothing: %+v", cv.Assignments)
	}
}

func TestATagRulesPrincipalWinsOverThePool(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	principals.availableMembersByGroup[poolGroup] = []string{activePrincipal}
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin)})
	if w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{{Tags: []string{"billing"}, DefaultPrincipalID: otherActivePrincipal}}); w.Code != 200 {
		t.Fatalf("tag rules: %d %s", w.Code, w.Body.String())
	}
	cv := createCard(t, h, boardID, columnID, "invoice", "billing")
	if got := onlyAssignee(t, cv); got != otherActivePrincipal {
		t.Fatalf("the tag rule's principal should win, got %s", got)
	}
	if detail := assignedDetail(t, h, cv.Placement.CardID); detail["source"] != "tag_rule" {
		t.Fatalf("source: %v", detail)
	}
}

func TestAPrincipalStoreThatCannotSayWhoIsAvailableRefusesTheCard(t *testing.T) {
	h, nb, principals := setupWithPrincipals(t)
	boardID := mkBoard(t, h, "Desk")
	columnID := mkColumn(t, h, boardID, "Open", "")
	patchBoard(t, h, boardID, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyRoundRobin), DefaultPrincipalID: str(activePrincipal)})
	principals.membersStatus = 500

	itemsBefore := len(nb.items)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "lost?", ColumnID: columnID})
	if w.Code != 502 || !strings.Contains(w.Body.String(), "assignment pool") {
		t.Fatalf("want 502 naming the pool, got %d %s", w.Code, w.Body.String())
	}
	if len(nb.items) != itemsBefore {
		t.Fatalf("a refused card must not leave a noteboard item behind: %d → %d", itemsBefore, len(nb.items))
	}
}

func TestAttachingAnUnassignedCardAlsoDrawsFromThePool(t *testing.T) {
	h, _, principals := setupWithPrincipals(t)
	principals.availableMembersByGroup[poolGroup] = []string{otherActivePrincipal}
	source := mkBoard(t, h, "Source")
	sourceColumn := mkColumn(t, h, source, "Todo", "")
	cv := createCard(t, h, source, sourceColumn, "loose")
	target := mkBoard(t, h, "Target")
	targetColumn := mkColumn(t, h, target, "Inbox", "")
	patchBoard(t, h, target, model.UpdateBoardRequest{AssignmentPool: pool(model.AssignmentStrategyLeastOpenCards)})
	if w := do(t, h, "PUT", "/api/boards/"+target+"/cards/"+cv.Placement.CardID, model.AttachCardRequest{ColumnID: targetColumn}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, "GET", "/api/cards/"+cv.Placement.CardID+"/assignments", nil)
	var assignments []model.CardAssignment
	decodeSuccessfulResponse(t, w, &assignments)
	if len(assignments) != 1 || assignments[0].PrincipalID != otherActivePrincipal {
		t.Fatalf("attach should draw from the pool: %+v", assignments)
	}
}
