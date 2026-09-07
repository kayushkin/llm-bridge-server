package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// recordingKanban is a fake kanban-store that remembers every card move it was
// asked for, keyed by card id, and answers 200 to everything else.
func recordingKanban(t *testing.T) (*kanbanclient.Client, map[string]string, func()) {
	t.Helper()
	moves := map[string]string{} // card id -> column id
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/move") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/cards/"), "/")
			var body struct {
				ColumnID string `json:"column_id"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("move body is not JSON: %v", err)
			}
			moves[parts[0]] = body.ColumnID
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	kc := kanbanclient.New(srv.URL)
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{
		columnNoAction:        "col-none",
		columnActionNeeded:    "col-needed",
		columnActionCompleted: "col-completed",
	}
	return kc, moves, srv.Close
}

// TestDoneCardLeavesTheActionColumn — the bug this reconciler exists for: a
// todo ticked done in noteboard stayed in Action needed, still an action item.
func TestDoneCardLeavesTheActionColumn(t *testing.T) {
	kc, moves, done := recordingKanban(t)
	defer done()

	state := &boardState{Cards: map[string]*cardState{
		"done-in-needed":  {ID: "done-in-needed", Title: "NNBA Adult Ballet enrollment", Status: statusDone, ColumnName: columnActionNeeded, Action: "file"},
		"done-in-none":    {ID: "done-in-none", Title: "AMP Up Your Business", Status: statusDone, ColumnName: columnNoAction, Action: "none"},
		"open-in-needed":  {ID: "open-in-needed", Title: "Rotate staging Postgres credentials", Status: statusOpen, ColumnName: columnActionNeeded, Action: "fix"},
		"done-in-done":    {ID: "done-in-done", Title: "Already settled", Status: statusDone, ColumnName: columnActionCompleted, Action: "pay"},
		"archived-anyway": {ID: "archived-anyway", Title: "Merged away", Status: "archived", ColumnName: columnActionNeeded, Action: "reply"},
	}}

	got := reconcileStatusWithColumn(kc, state, nil, false)

	if got.Completed != 2 {
		t.Errorf("Completed = %d, want 2 (both done cards outside Action completed)", got.Completed)
	}
	if got.Reopened != 0 || got.Skipped != 0 {
		t.Errorf("Reopened = %d, Skipped = %d, want 0 and 0", got.Reopened, got.Skipped)
	}
	for _, id := range []string{"done-in-needed", "done-in-none"} {
		if moves[id] != "col-completed" {
			t.Errorf("card %s moved to %q, want col-completed", id, moves[id])
		}
		if state.Cards[id].ColumnName != columnActionCompleted {
			t.Errorf("card %s in-memory column = %q; the sweep that follows must see it as completed", id, state.Cards[id].ColumnName)
		}
	}
	for _, id := range []string{"open-in-needed", "done-in-done", "archived-anyway"} {
		if _, moved := moves[id]; moved {
			t.Errorf("card %s was moved; an open card in its action column, a done card already completed, and an archived card must all be left alone", id)
		}
	}
}

// TestReopenedCardReturnsToItsActionColumn — the same defect mirrored: a todo
// reopened in noteboard stayed in Action completed, invisible as work.
func TestReopenedCardReturnsToItsActionColumn(t *testing.T) {
	kc, moves, done := recordingKanban(t)
	defer done()

	state := &boardState{Cards: map[string]*cardState{
		"reopened-task":   {ID: "reopened-task", Title: "Pay the invoice after all", Status: statusOpen, ColumnName: columnActionCompleted, Action: "pay"},
		"reopened-bucket": {ID: "reopened-bucket", Title: "Medium digests", Status: statusOpen, ColumnName: columnActionCompleted, Action: "none"},
	}}

	got := reconcileStatusWithColumn(kc, state, nil, false)

	if got.Reopened != 2 || got.Completed != 0 {
		t.Errorf("Reopened = %d, Completed = %d, want 2 and 0", got.Reopened, got.Completed)
	}
	if moves["reopened-task"] != "col-needed" {
		t.Errorf("reopened task moved to %q, want col-needed — action 'pay' is work", moves["reopened-task"])
	}
	if moves["reopened-bucket"] != "col-none" {
		t.Errorf("reopened bucket moved to %q, want col-none — action 'none' is not work", moves["reopened-bucket"])
	}
}

// TestDryRunReconcileMovesNothingButCountsEverything — the summary a dry run
// prints must match what the real run would do, or the dry run is not a preview.
func TestDryRunReconcileMovesNothingButCountsEverything(t *testing.T) {
	kc, moves, done := recordingKanban(t)
	defer done()

	state := &boardState{Cards: map[string]*cardState{
		"a": {ID: "a", Status: statusDone, ColumnName: columnActionNeeded, Action: "verify"},
		"b": {ID: "b", Status: statusOpen, ColumnName: columnActionCompleted, Action: "reply"},
	}}

	got := reconcileStatusWithColumn(kc, state, nil, true)

	if len(moves) != 0 {
		t.Errorf("dry run issued %d moves, want 0", len(moves))
	}
	if got.Completed != 1 || got.Reopened != 1 {
		t.Errorf("dry run counted Completed=%d Reopened=%d, want 1 and 1", got.Completed, got.Reopened)
	}
	if state.Cards["a"].ColumnName != columnActionNeeded {
		t.Error("dry run changed in-memory state")
	}
}
