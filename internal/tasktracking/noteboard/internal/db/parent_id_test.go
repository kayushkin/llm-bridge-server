package db

import (
	"errors"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

// The hold gate and the spend ceiling both roll up over parent_id, and they do
// it by joining a child to its parent. A parent_id that matches no row joins to
// nothing, so the child inherits neither — it reads to every consumer as
// ordinary open work with no hold and no ceiling. Nothing downstream can tell
// that apart from a child whose parent really is unheld, which is why the write
// has to refuse.

func str(s string) *string { return &s }

// mustCreateWith is mustCreate's sibling for the cases that need more of the
// request than a title.
func mustCreateWith(t *testing.T, s *Store, req *model.CreateItemRequest) *model.Item {
	t.Helper()
	item, err := s.CreateItem(req)
	if err != nil {
		t.Fatalf("CreateItem(%q): %v", req.Title, err)
	}
	return item
}

func TestAParentIDThatNamesNoItemIsRefused(t *testing.T) {
	s := newTestStore(t)

	_, err := s.CreateItem(&model.CreateItemRequest{
		Type:     "todo",
		Title:    "child of nothing",
		ParentID: str("a88bca06-3c94-4b7a-9a2d-59c2d1a3e9d1"),
	})
	if err == nil {
		t.Fatal("a child naming a parent that does not exist was created, and it escapes every hold and ceiling above it")
	}
	if !errors.Is(err, ErrUnknownParent) {
		t.Errorf("error is %v, and does not identify itself as an unknown parent", err)
	}
}

func TestAParentIDThatNamesADeletedItemIsRefused(t *testing.T) {
	s := newTestStore(t)

	parent := mustCreate(t, s, "parent")
	if err := s.DeleteItem(parent.ID, false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	// A tombstone cannot seed the held subtree, so a child hung off one is the
	// same escape as a child hung off nothing.
	if _, err := s.CreateItem(&model.CreateItemRequest{
		Type: "todo", Title: "child of a tombstone", ParentID: str(parent.ID),
	}); !errors.Is(err, ErrUnknownParent) {
		t.Errorf("a child of a deleted parent was created (err %v)", err)
	}
}

func TestARealParentIsAccepted(t *testing.T) {
	s := newTestStore(t)

	parent := mustCreate(t, s, "parent")
	child := mustCreateWith(t, s, &model.CreateItemRequest{
		Type: "todo", Title: "child", ParentID: str(parent.ID),
	})

	if child.ParentID == nil || *child.ParentID != parent.ID {
		t.Fatalf("child parent_id = %v, want %s", child.ParentID, parent.ID)
	}
}

// TestTheCheckedParentIDIsWhatMakesTheHoldReachTheChild ties the refusal to the
// thing it protects: a held parent withholds its children from the discovery
// paths, and it can only do that when the child names it.
func TestTheCheckedParentIDIsWhatMakesTheHoldReachTheChild(t *testing.T) {
	s := newTestStore(t)

	parent := mustCreate(t, s, "parked parent")
	child := mustCreateWith(t, s, &model.CreateItemRequest{
		Type: "todo", Title: "child of the parked parent", ParentID: str(parent.ID),
	})
	if _, err := s.HoldItem(parent.ID, "user parked this"); err != nil {
		t.Fatalf("HoldItem: %v", err)
	}

	items, err := s.ListItems(ListParams{Type: "todo", Status: "open"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.ID == child.ID {
			t.Fatal("a held parent's child was offered as open work")
		}
		if item.ID == parent.ID {
			t.Fatal("a held parent was offered as open work")
		}
	}
}

func TestAnItemCannotBeMovedInsideItsOwnAncestry(t *testing.T) {
	s := newTestStore(t)

	top := mustCreate(t, s, "top")
	middle := mustCreateWith(t, s, &model.CreateItemRequest{Type: "todo", Title: "middle", ParentID: str(top.ID)})
	bottom := mustCreateWith(t, s, &model.CreateItemRequest{Type: "todo", Title: "bottom", ParentID: str(middle.ID)})

	// Its own parent: a one-item cycle, reachable from nothing above it.
	if _, err := s.UpdateItem(top.ID, &model.UpdateItemRequest{ParentID: str(top.ID)}); !errors.Is(err, ErrUnknownParent) {
		t.Errorf("an item became its own parent (err %v)", err)
	}

	// A two-step cycle: top under bottom, which is already under top.
	if _, err := s.UpdateItem(top.ID, &model.UpdateItemRequest{ParentID: str(bottom.ID)}); !errors.Is(err, ErrUnknownParent) {
		t.Errorf("a parent was moved under its own descendant (err %v)", err)
	}

	// The legal move in the same shape still works: bottom straight under top.
	if _, err := s.UpdateItem(bottom.ID, &model.UpdateItemRequest{ParentID: str(top.ID)}); err != nil {
		t.Errorf("reparenting a child onto a real ancestor was refused: %v", err)
	}
}

func TestAnUpdateToAParentIDThatNamesNoItemIsRefused(t *testing.T) {
	s := newTestStore(t)

	child := mustCreate(t, s, "child")

	if _, err := s.UpdateItem(child.ID, &model.UpdateItemRequest{
		ParentID: str("00000000-0000-0000-0000-000000000000"),
	}); !errors.Is(err, ErrUnknownParent) {
		t.Errorf("a PATCH moved an item under a parent that does not exist (err %v)", err)
	}
}
