package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestKanbanTracker_Live runs end-to-end against the host kanban-store
// (default :8305). Skips if the service isn't reachable so CI/non-host envs
// don't fail. Cleans up its synthetic card+links on exit.
func TestKanbanTracker_Live(t *testing.T) {
	baseURL := os.Getenv("KANBAN_STORE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8305"
	}
	if resp, err := http.Get(baseURL + "/health"); err != nil {
		t.Skipf("kanban-store not reachable at %s: %v", baseURL, err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Skipf("kanban-store /health returned %s", resp.Status)
		}
	}

	c := newKanbanClient(baseURL)
	if err := c.EnsureBoard(kanbanBoardName, kanbanDefaultColumns); err != nil {
		t.Fatalf("EnsureBoard: %v", err)
	}
	if c.boardID == "" {
		t.Fatalf("boardID empty after EnsureBoard")
	}
	for _, want := range kanbanDefaultColumns {
		if c.columnsByName[want] == "" {
			t.Errorf("column %q missing after EnsureBoard", want)
		}
	}

	cardID, err := c.CreateCard(kanbanColumnTarget,
		"autoworker-test card (delete me)",
		"This card was created by `go test` in scheduler/cmd/autoworker. Safe to delete.",
		[]string{kanbanCardOrigin, "test"},
		"autoworker-test-bus-session",
		"test-provider",
	)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if cardID == "" {
		t.Fatalf("CreateCard: empty card id")
	}

	// Cleanup: hard-delete the synthetic noteboard item + its placement.
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", strings.TrimRight(baseURL, "/")+"/api/cards/"+cardID+"?hard=true", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	})
}
