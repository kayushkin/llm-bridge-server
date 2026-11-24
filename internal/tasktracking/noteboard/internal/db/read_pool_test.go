package db

import (
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

// TestOpenListReadDoesNotHoldUpOtherRequests stands in for the spend guard
// paging the whole table: a read cursor stays open on the read-only pool while
// a write, a single-item read and a second list read run. With one connection
// for everything, each of them waited for the cursor to close.
func TestOpenListReadDoesNotHoldUpOtherRequests(t *testing.T) {
	store := newTestStore(t)
	first, err := store.CreateItem(&model.CreateItemRequest{Type: "note", Title: "first"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	openCursor, err := store.readOnlyDB.Query("SELECT id FROM items")
	if err != nil {
		t.Fatalf("open cursor: %v", err)
	}
	defer openCursor.Close()
	if !openCursor.Next() {
		t.Fatalf("open cursor returned no row: %v", openCursor.Err())
	}

	finished := make(chan error, 1)
	go func() {
		if _, err := store.CreateItem(&model.CreateItemRequest{Type: "note", Title: "second"}); err != nil {
			finished <- err
			return
		}
		if _, err := store.GetItem(first.ID); err != nil {
			finished <- err
			return
		}
		items, err := store.ListItems(ListParams{Type: "note"})
		if err != nil {
			finished <- err
			return
		}
		if len(items) != 2 {
			t.Errorf("ListItems returned %d items, want 2: a read on the read-only pool must see the write that just committed", len(items))
		}
		finished <- nil
	}()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write, a single-item read and a list read waited on an open list read")
	}
}

// TestReadOnlyPoolRefusesWrites pins query_only, so a write routed to the
// read-only pool by mistake fails at once instead of racing the writer.
func TestReadOnlyPoolRefusesWrites(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.readOnlyDB.Exec("DELETE FROM items"); err == nil {
		t.Fatal("the read-only pool accepted a write")
	}
}
