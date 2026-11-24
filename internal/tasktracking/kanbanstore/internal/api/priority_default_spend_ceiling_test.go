package api_test

import (
	"net/http"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

func dollars(v float64) *float64 { return &v }

// mkLadderWithDefaultCeilings gives a board two rungs: P0 (value 5) defaulting
// to $20 and P3 (value 2) defaulting to $2, plus P4 (value 1) with no default.
func mkLadderWithDefaultCeilings(t *testing.T, h http.Handler, boardID string, topDefault, lowDefault float64) {
	t.Helper()
	w := do(t, h, "PUT", "/api/boards/"+boardID+"/priority-levels", model.SetPriorityLadderRequest{
		Levels: []model.BoardPriorityLevel{
			{PriorityValue: 5, Label: "P0", DefaultAutoHoldAtUSD: dollars(topDefault)},
			{PriorityValue: 2, Label: "P3", DefaultAutoHoldAtUSD: dollars(lowDefault)},
			{PriorityValue: 1, Label: "P4"},
		},
	})
	if w.Code != 200 {
		t.Fatalf("set ladder: %d %s", w.Code, w.Body.String())
	}
}

func ceilingOf(t *testing.T, nb *fakeNoteboard, cardID string) any {
	t.Helper()
	nb.mu.Lock()
	defer nb.mu.Unlock()
	return nb.items[cardID]["auto_hold_at_usd"]
}

func TestLadderStoresDefaultSpendCeiling(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Work")
	mkLadderWithDefaultCeilings(t, h, boardID, 20, 2)

	var ladder model.PriorityLadder
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/priority-levels", nil), &ladder)
	if got := ladder.Levels[0].DefaultAutoHoldAtUSD; got == nil || *got != 20 {
		t.Errorf("P0 default = %v, want 20", got)
	}
	if got := ladder.Levels[2].DefaultAutoHoldAtUSD; got != nil {
		t.Errorf("P4 default = %v, want none", *got)
	}

	w := do(t, h, "PUT", "/api/boards/"+boardID+"/priority-levels", model.SetPriorityLadderRequest{
		Levels: []model.BoardPriorityLevel{{PriorityValue: 5, Label: "P0", DefaultAutoHoldAtUSD: dollars(-1)}},
	})
	if w.Code != 400 {
		t.Errorf("negative default: %d, want 400", w.Code)
	}
}

func TestCreatedCardTakesItsRungsDefaultSpendCeiling(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Work")
	col := mkColumn(t, h, boardID, "Todo", "")
	mkLadderWithDefaultCeilings(t, h, boardID, 20, 2)

	top := 5
	var defaulted, handSet, unranked model.CardView
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "a", ColumnID: col, Priority: &top}), &defaulted)
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "b", ColumnID: col, Priority: &top, AutoHoldAtUSD: dollars(0)}), &handSet)
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "c", ColumnID: col}), &unranked)

	if got := ceilingOf(t, nb, defaulted.Placement.CardID); got != 20.0 {
		t.Errorf("P0 card ceiling = %v, want 20", got)
	}
	if got := ceilingOf(t, nb, handSet.Placement.CardID); got != 0.0 {
		t.Errorf("card created with $0 ceiling = %v, want 0 kept", got)
	}
	if got := ceilingOf(t, nb, unranked.Placement.CardID); got != nil {
		t.Errorf("unranked card ceiling = %v, want none", got)
	}
}

func TestPriorityChangeMovesADefaultedCeilingButNotAHandSetOne(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Work")
	col := mkColumn(t, h, boardID, "Todo", "")
	mkLadderWithDefaultCeilings(t, h, boardID, 20, 2)

	low := 2
	var card model.CardView
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "a", ColumnID: col, Priority: &low}), &card)
	cardID := card.Placement.CardID
	if got := ceilingOf(t, nb, cardID); got != 2.0 {
		t.Fatalf("P3 card ceiling = %v, want 2", got)
	}

	// Still carrying P3's default, so promoting it to P0 gives it P0's.
	if w := do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"priority": 5}); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if got := ceilingOf(t, nb, cardID); got != 20.0 {
		t.Errorf("after promotion ceiling = %v, want 20", got)
	}

	// Moving to a rung with no default leaves the ceiling as it is.
	do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"priority": 1})
	if got := ceilingOf(t, nb, cardID); got != 20.0 {
		t.Errorf("after moving to a rung with no default ceiling = %v, want 20 kept", got)
	}

	// Set by hand, then re-prioritized: the hand-set number stays.
	do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"auto_hold_at_usd": 7.5})
	do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"priority": 2})
	if got := ceilingOf(t, nb, cardID); got != 7.5 {
		t.Errorf("hand-set ceiling after priority change = %v, want 7.5 kept", got)
	}

	// A patch naming both decides the ceiling itself.
	do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"priority": 5, "auto_hold_at_usd": 3})
	if got := ceilingOf(t, nb, cardID); got != 3.0 {
		t.Errorf("ceiling named in the same patch = %v, want 3", got)
	}

	if w := do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"priority": "high"}); w.Code != 400 {
		t.Errorf("non-numeric priority: %d, want 400", w.Code)
	}
}

func TestAttachedCardTakesTheLowestDefaultOfItsBoards(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	generous := mkBoard(t, h, "Generous")
	mkLadderWithDefaultCeilings(t, h, generous, 20, 2)
	strict := mkBoard(t, h, "Strict")
	strictColumn := mkColumn(t, h, strict, "Todo", "")
	mkLadderWithDefaultCeilings(t, h, strict, 4, 1)
	generousColumn := mkColumn(t, h, generous, "Todo", "")

	cardID := nb.seedItem("existing")
	nb.mu.Lock()
	nb.items[cardID]["priority"] = 5.0
	nb.mu.Unlock()

	if w := do(t, h, "PUT", "/api/boards/"+generous+"/cards/"+cardID, model.AttachCardRequest{ColumnID: generousColumn}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	if got := ceilingOf(t, nb, cardID); got != 20.0 {
		t.Fatalf("after attach to generous board ceiling = %v, want 20", got)
	}
	if w := do(t, h, "PUT", "/api/boards/"+strict+"/cards/"+cardID, model.AttachCardRequest{ColumnID: strictColumn}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	// It already has a ceiling, so a second attach does not change it.
	if got := ceilingOf(t, nb, cardID); got != 20.0 {
		t.Errorf("after second attach ceiling = %v, want 20 kept", got)
	}

	// An unranked card on both boards, given a priority, takes the lower default.
	unrankedID := nb.seedItem("unranked")
	do(t, h, "PUT", "/api/boards/"+generous+"/cards/"+unrankedID, model.AttachCardRequest{ColumnID: generousColumn})
	do(t, h, "PUT", "/api/boards/"+strict+"/cards/"+unrankedID, model.AttachCardRequest{ColumnID: strictColumn})
	if got := ceilingOf(t, nb, unrankedID); got != nil {
		t.Fatalf("unranked card got ceiling %v on attach, want none", got)
	}
	if w := do(t, h, "PATCH", "/api/cards/"+unrankedID, map[string]any{"priority": 2}); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if got := ceilingOf(t, nb, unrankedID); got != 1.0 {
		t.Errorf("priority change on two boards ceiling = %v, want 1", got)
	}
}
