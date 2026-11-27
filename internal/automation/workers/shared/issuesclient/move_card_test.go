package issuesclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// moveCardAgainst runs MoveCard against a stand-in kanban-store that answers
// the move route with the given status and body, and fails the test on any
// other route.
func moveCardAgainst(t *testing.T, status int, body string) (MoveResult, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != "/api/cards/card-1/move" {
			t.Errorf("unexpected request %s %s", r.Method, r.RequestURI)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	return New(srv.URL, "").MoveCard("card-1", "board-1", "col-1", 0)
}

// kanban-store treats a failed auto-status patch as non-fatal: the move
// succeeded on the board, so it answers 200 and names the failure in the body.
// A client that reads only the status code cannot see it, and the card then
// sits in Done on the board while its noteboard item is still open.
func TestMoveCardReportsAFailedAutoStatusPatch(t *testing.T) {
	result, err := moveCardAgainst(t, 200, `{"placement":{},"auto_status_error":"noteboard: 502 bad gateway"}`)
	if err != nil {
		t.Fatalf("the move itself succeeded (HTTP 200); MoveCard must not report it as failed: %v", err)
	}
	if !strings.Contains(result.AutoStatusError, "502") {
		t.Errorf("AutoStatusError = %q, want the server's reason verbatim", result.AutoStatusError)
	}
}

// Reporting only the failure would leave a caller unable to tell "the patch
// worked" from "this column has no auto_status".
func TestMoveCardReportsTheAutoStatusItApplied(t *testing.T) {
	result, err := moveCardAgainst(t, 200, `{"placement":{},"auto_status_applied":"done"}`)
	if err != nil {
		t.Fatalf("MoveCard: %v", err)
	}
	if result.AutoStatusApplied != "done" || result.AutoStatusError != "" {
		t.Errorf("result = %+v, want AutoStatusApplied=done and no error", result)
	}
}

// The control that stops the fix crying wolf: most moves go to Backlog or In
// Progress, which carry no auto_status, so the response has neither field.
func TestMoveCardToAColumnWithoutAutoStatusReportsNeither(t *testing.T) {
	result, err := moveCardAgainst(t, 200, `{"placement":{"card_id":"card-1"}}`)
	if err != nil {
		t.Fatalf("MoveCard: %v", err)
	}
	if result != (MoveResult{}) {
		t.Errorf("a column with no auto_status reported %+v; both fields must stay empty", result)
	}
}

// A column at its WIP limit refuses the move with 409, and the card stays
// where it was. A client that swallowed it would report a move that never
// happened.
func TestMoveCardSurfacesTheWIPRejection(t *testing.T) {
	_, err := moveCardAgainst(t, 409, `{"error":"column at WIP limit"}`)
	if err == nil {
		t.Fatal("a 409 WIP rejection was read as a successful move")
	}
	if !strings.Contains(err.Error(), "WIP") {
		t.Errorf("error %q drops the server's reason", err)
	}
}

// A 200 whose body cannot be read says nothing about the auto-status patch, so
// MoveCard must not report it as clean.
func TestMoveCardDoesNotCallAnUnreadableResponseClean(t *testing.T) {
	if _, err := moveCardAgainst(t, 200, `not json`); err == nil {
		t.Fatal("an unreadable move response was reported as a clean move")
	}
}
