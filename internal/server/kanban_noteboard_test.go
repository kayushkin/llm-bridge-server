package server

import (
	"errors"
	"strings"
	"testing"

	kanbannoteboard "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/noteboard"
	noteboardapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

func newKanbanNoteboardOnTestDatabase(t *testing.T) kanbanNoteboard {
	t.Helper()
	module, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	return kanbanNoteboard{notes: noteboardapi.New(items.New(postgrestest.NewDatabase(t, module).Open(module)))}
}

// kanban-store reads noteboard in Go with the answers its HTTP client read:
// an item as opaque JSON, a missing one as not found, a stale patch as the
// item as it is now, and a refused query or item as noteboard's own words.
func TestKanbanStoreCallsNoteboardInGoWithItsRefusals(t *testing.T) {
	notes := newKanbanNoteboardOnTestDatabase(t)
	body := "the printer on floor two"
	created, err := notes.CreateItem(kanbannoteboard.CreateItemPayload{Type: "todo", Title: "printer jammed", Body: &body, Tags: []string{"support"}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" || created["title"] != "printer jammed" || created["body"] != body {
		t.Fatalf("created = %v", created)
	}
	if _, err := notes.CreateItem(kanbannoteboard.CreateItemPayload{Title: "no type"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("an item with no type = %v, want noteboard's 400", err)
	}
	if _, err := notes.GetItem("00000000-0000-0000-0000-000000000000"); !kanbannoteboard.IsNotFound(err) {
		t.Errorf("an unknown item = %v, want not found", err)
	}

	// A patch made against the version the read returned applies; a second
	// one against the same, now old, version is refused with the item as it
	// is.
	version := `"` + created["updated_at"].(string) + `"`
	if _, err := notes.PatchItemIfMatch(id, map[string]any{"title": "printer fixed"}, version); err != nil {
		t.Fatal(err)
	}
	_, err = notes.PatchItemIfMatch(id, map[string]any{"title": "printer jammed again"}, version)
	var changed *kanbannoteboard.ItemChangedError
	if !errors.As(err, &changed) || !strings.Contains(string(changed.Body), `"title":"printer fixed"`) || changed.ETag == "" {
		t.Fatalf("a stale patch = %v", err)
	}

	held, err := notes.HoldItem(id, "waiting on a part")
	if err != nil || held["held_at"] == nil {
		t.Fatalf("hold = %v, %v", held, err)
	}
	if _, err := notes.UnholdItem(id); err != nil {
		t.Fatal(err)
	}
	batch, err := notes.GetItems([]string{id, "00000000-0000-0000-0000-000000000000"})
	if err != nil || len(batch) != 2 || batch[0]["id"] != id || batch[1] != nil {
		t.Fatalf("batch = %v, %v", batch, err)
	}
	found, err := notes.Search("printer", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("search = %v, %v", found, err)
	}
	result, err := notes.QueryItems(kanbannoteboard.ItemsQuery{IDs: []string{id}, IncludeItems: true})
	if err != nil || result.Total != 1 || len(result.Items) != 1 {
		t.Fatalf("query = %+v, %v", result, err)
	}
	_, err = notes.QueryItems(kanbannoteboard.ItemsQuery{IDs: []string{id}, Sort: "no-such-sort"})
	var refused *kanbannoteboard.QueryRefusedError
	if !errors.As(err, &refused) || !strings.Contains(string(refused.Body), `"error"`) {
		t.Fatalf("a query with an unknown sort = %v", err)
	}
	if err := notes.DeleteItem(id, false); err != nil {
		t.Fatal(err)
	}
	if err := notes.DeleteItem("00000000-0000-0000-0000-000000000000", false); !kanbannoteboard.IsNotFound(err) {
		t.Errorf("deleting an unknown item = %v, want not found", err)
	}
}
