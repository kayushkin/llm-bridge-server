package api_test

import (
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// A write to a card noteboard does not have is a 404: the caller asked about
// something that is not there, and retrying will not help. noteboard failing
// is a 502: the caller should try again. Until fix/missing-card-404 every one
// of these routes answered 502 for both, so a caller deleting an already-purged
// card was told the store was down (card 66c85b68-a051-4e86-9ac4-b0ff7b39b109).
func TestCardWritesSayMissingAs404AndFailureAs502(t *testing.T) {
	h, nb, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	colID := mkColumn(t, h, boardID, "Todo", "")

	writes := []struct {
		name, method, path string
		body               any
	}{
		{"patch", "PATCH", "/api/cards/nb-missing", map[string]any{"title": "x"}},
		{"patch priority", "PATCH", "/api/cards/nb-missing", map[string]any{"priority": 2}},
		{"delete", "DELETE", "/api/cards/nb-missing", nil},
		{"hard delete", "DELETE", "/api/cards/nb-missing?hard=true", nil},
		{"hold", "POST", "/api/cards/nb-missing/hold", map[string]string{"reason": "r"}},
		{"unhold", "POST", "/api/cards/nb-missing/unhold", nil},
		{"attach", "PUT", "/api/boards/" + boardID + "/cards/nb-missing", model.AttachCardRequest{ColumnID: colID}},
	}

	for _, write := range writes {
		if w := do(t, h, write.method, write.path, write.body); w.Code != 404 {
			t.Errorf("%s of a missing card: want 404, got %d %s", write.name, w.Code, w.Body.String())
		}
	}

	nb.mu.Lock()
	nb.failStatus = 500
	nb.mu.Unlock()
	for _, write := range writes {
		if w := do(t, h, write.method, write.path, write.body); w.Code != 502 {
			t.Errorf("%s while noteboard fails: want 502, got %d %s", write.name, w.Code, w.Body.String())
		}
	}
}
