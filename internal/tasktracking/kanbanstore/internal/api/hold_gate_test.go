package api_test

import (
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// The hold gate is the stop/play button, and `holdCard`'s `hold bool` is the
// only thing that tells its two directions apart: POST /cards/{id}/hold and
// POST /cards/{id}/unhold reach one function and differ by that argument alone.
// No test called it, so a version that ignored the argument — or inverted it —
// passed the whole suite.
//
// The cost is specific rather than theoretical. The hold lives on the noteboard
// item, and noteboard withholds held items from the agent read paths, so a hold
// is how work gets parked where an unattended agent will not pick it up. An
// inverted gate fails silently in the dangerous direction: pressing hold clears
// the hold, and the parked card is handed to the next unattended pass as
// ordinary open work.

func TestHoldParksTheCardAndUnholdReleasesIt(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards",
		model.CreateCardRequest{Title: "parked work", ColumnID: colID})
	if w.Code != 201 {
		t.Fatalf("create card: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID

	if nb.heldAt(cardID) != "" {
		t.Fatalf("a freshly created card is already held")
	}

	// Hold: the item must come back held, carrying the stated reason.
	if w := do(t, h, "POST", "/api/cards/"+cardID+"/hold",
		map[string]string{"reason": "waiting on the user"}); w.Code != 200 {
		t.Fatalf("hold: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if nb.heldAt(cardID) == "" {
		t.Errorf("after POST /hold the noteboard item is not held")
	}
	if got := nb.holdReason(cardID); got != "waiting on the user" {
		t.Errorf("hold reason = %q, want %q", got, "waiting on the user")
	}

	// Unhold: the gate must actually open again. This is the assertion that a
	// hold-only implementation cannot pass, and the reason both directions are
	// driven in one test rather than two.
	if w := do(t, h, "POST", "/api/cards/"+cardID+"/unhold", nil); w.Code != 200 {
		t.Fatalf("unhold: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := nb.heldAt(cardID); got != "" {
		t.Errorf("after POST /unhold the item is still held (held_at=%q)", got)
	}
}

func TestHoldGateRejectsNonPostAndSurfacesUpstreamFailure(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards",
		model.CreateCardRequest{Title: "a card", ColumnID: colID})
	var cv model.CardView
	decodeSuccessfulResponse(t, w, &cv)
	cardID := cv.Placement.CardID

	// A gate a GET can close is not a gate: a link-follower or a prefetch would
	// park work nobody asked to park.
	for _, method := range []string{"GET", "DELETE", "PATCH"} {
		if w := do(t, h, method, "/api/cards/"+cardID+"/hold", nil); w.Code != 405 {
			t.Errorf("%s /hold: expected 405, got %d", method, w.Code)
		}
		if w := do(t, h, method, "/api/cards/"+cardID+"/unhold", nil); w.Code != 405 {
			t.Errorf("%s /unhold: expected 405, got %d", method, w.Code)
		}
	}
	if nb.heldAt(cardID) != "" {
		t.Errorf("a rejected non-POST request still held the card")
	}

	// The gate is remote. A hold that noteboard refused must not answer 200 —
	// that reads to the caller as parked work that was never parked. An item
	// noteboard does not have is a 404; noteboard failing is a 502, which
	// TestCardWritesSayMissingAs404AndFailureAs502 covers.
	if w := do(t, h, "POST", "/api/cards/nb-does-not-exist/hold", nil); w.Code != 404 {
		t.Errorf("hold on a missing item: expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "POST", "/api/cards/nb-does-not-exist/unhold", nil); w.Code != 404 {
		t.Errorf("unhold on a missing item: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
