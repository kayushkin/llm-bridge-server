package db

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

// Both tests below pin the same thing on the two write paths that put a
// column id into `placements`: the column is looked up in Go, and a column
// that is not there is answered by Go.
//
// The schema also guards it — `placements.column_id REFERENCES columns(id)`,
// and kanban-store opens SQLite with `_foreign_keys=on`, so the constraint is
// live rather than decorative. That is exactly why these tests assert the
// SENTINEL and not merely that some error came back. The two layers do not
// answer alike:
//
//	Go:     ErrNotFound            -> the API's mapDBErr turns it into 404
//	SQLite: FOREIGN KEY constraint failed -> mapDBErr turns it into 500
//
// So if the Go lookup is ever dropped, the endpoint degrades from a
// client-correctable 404 to a server fault, and the message stops naming the
// column at all. Measured before these tests existed: deleting the Go check in
// either function left the whole suite green.
//
// The move test attaches the card for real first. Without a live placement the
// UPDATE matches no row and MoveCard returns ErrNotFound for that reason
// instead, which would satisfy the assertion with the Go check gone — a test
// that passes for the wrong reason and pins nothing.

func newPlacementTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestAttachCardToAColumnThatDoesNotExistIsAnsweredByGo(t *testing.T) {
	store := newPlacementTestStore(t)
	board, err := store.CreateBoard(&model.CreateBoardRequest{Name: "Board"})
	if err != nil {
		t.Fatalf("create board: %v", err)
	}

	_, err = store.AttachCard(board.ID, "card-1", &model.AttachCardRequest{ColumnID: "no-such-column"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach to a missing column = %v, want ErrNotFound — the Go lookup is what makes this a 404 rather than a foreign-key 500", err)
	}
}

func TestMoveCardToAColumnThatDoesNotExistIsAnsweredByGo(t *testing.T) {
	store := newPlacementTestStore(t)
	board, err := store.CreateBoard(&model.CreateBoardRequest{Name: "Board"})
	if err != nil {
		t.Fatalf("create board: %v", err)
	}
	column, err := store.CreateColumn(board.ID, &model.CreateColumnRequest{Name: "Todo"})
	if err != nil {
		t.Fatalf("create column: %v", err)
	}
	if _, err := store.AttachCard(board.ID, "card-1", &model.AttachCardRequest{ColumnID: column.ID}); err != nil {
		t.Fatalf("attach card: %v", err)
	}

	_, err = store.MoveCard("card-1", &model.MoveCardRequest{BoardID: board.ID, ColumnID: "no-such-column"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("move to a missing column = %v, want ErrNotFound — the Go lookup is what makes this a 404 rather than a foreign-key 500", err)
	}
}
