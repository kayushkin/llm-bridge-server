// Kanban tracking for autoworker fires.
//
// On a successful NATS publish, the autoworker creates a card on the
// "Agent runs" board so the user can see what was dispatched and when.
// Cards land in the "In Progress" column with a card_link to bus_session_id
// (which the adapter uses as the bridge session key) and tags identifying
// the origin. The board self-bootstraps on first run.
//
// Done/Failed transitions are NOT handled here — autoworker is fire-and-
// forget and does not see the session's result. A separate curator will
// pick up state changes by querying llm-bridge for autoworker-tagged cards.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	kanbanBoardName    = "Agent runs"
	kanbanColumnTarget = "In Progress"
	kanbanCardOrigin   = "autoworker"
)

var kanbanDefaultColumns = []string{"Queued", "In Progress", "Blocked", "Done", "Failed"}

// kanbanClient is a minimal HTTP client for kanban-store. Holds resolved
// board + column UUIDs after EnsureBoard so each fire just does one POST.
type kanbanClient struct {
	baseURL string
	http    *http.Client

	boardID    string
	columnsByName map[string]string
}

func newKanbanClient(baseURL string) *kanbanClient {
	return &kanbanClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// EnsureBoard finds or creates the named board and ensures the requested
// columns exist on it. Subsequent calls are idempotent. Caches board+column
// IDs on the client for reuse.
func (c *kanbanClient) EnsureBoard(name string, wantColumns []string) error {
	board, err := c.findBoardByName(name)
	if err != nil {
		return err
	}
	if board == nil {
		board, err = c.createBoard(name)
		if err != nil {
			return fmt.Errorf("create board %q: %w", name, err)
		}
	}
	c.boardID = board.ID

	cols, err := c.listColumns(board.ID)
	if err != nil {
		return fmt.Errorf("list columns: %w", err)
	}
	have := map[string]string{}
	for _, col := range cols {
		have[col.Name] = col.ID
	}
	for _, want := range wantColumns {
		if _, ok := have[want]; ok {
			continue
		}
		var autoStatus *string
		switch want {
		case "Done":
			s := "done"
			autoStatus = &s
		case "Failed":
			s := "archived"
			autoStatus = &s
		}
		col, err := c.createColumn(board.ID, want, autoStatus)
		if err != nil {
			return fmt.Errorf("create column %q: %w", want, err)
		}
		have[want] = col.ID
	}
	c.columnsByName = have
	return nil
}

// CreateCard posts a new card to the bound board in the named column. Returns
// the created noteboard item id (== card_id in kanban-store).
//
// busSessionID is wired as a typed link so reverse lookups via
// /api/kanban/entities/bus_session/<id>/cards work.
func (c *kanbanClient) CreateCard(columnName, title, body string, tags []string, busSessionID, providerName string) (cardID string, err error) {
	if c.boardID == "" {
		return "", errors.New("kanbanClient: EnsureBoard not called")
	}
	colID, ok := c.columnsByName[columnName]
	if !ok {
		return "", fmt.Errorf("column %q missing on board", columnName)
	}

	cardPayload := map[string]any{
		"title":     title,
		"body":      body,
		"tags":      tags,
		"column_id": colID,
	}
	body1, _ := json.Marshal(cardPayload)
	url := fmt.Sprintf("%s/api/boards/%s/cards", c.baseURL, c.boardID)
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body1))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("kanban create card: %s — %s", resp.Status, string(raw))
	}
	var created struct {
		Item map[string]any `json:"item"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	id, _ := created.Item["id"].(string)
	if id == "" {
		return "", errors.New("kanban create card: empty item id in response")
	}

	// Add typed links. Best-effort; card already exists, so a link failure is
	// not fatal but is logged by the caller via the returned error chain.
	type linkSpec struct{ entityType, entityRef, label string }
	links := []linkSpec{
		{"bus_session", busSessionID, "autoworker"},
		{"autoworker_provider", providerName, ""},
	}
	for _, l := range links {
		if l.entityRef == "" {
			continue
		}
		payload := map[string]any{"entity_type": l.entityType, "entity_ref": l.entityRef}
		if l.label != "" {
			payload["label"] = l.label
		}
		body2, _ := json.Marshal(payload)
		linkURL := fmt.Sprintf("%s/api/cards/%s/links", c.baseURL, id)
		linkResp, err := c.http.Post(linkURL, "application/json", bytes.NewReader(body2))
		if err != nil {
			return id, fmt.Errorf("link card %s → %s/%s: %w", id, l.entityType, l.entityRef, err)
		}
		linkResp.Body.Close()
		if linkResp.StatusCode != http.StatusCreated {
			return id, fmt.Errorf("link card %s → %s/%s: HTTP %d", id, l.entityType, l.entityRef, linkResp.StatusCode)
		}
	}
	return id, nil
}

// --- low-level helpers ---

type kanbanBoard struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type kanbanColumn struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *kanbanClient) findBoardByName(name string) (*kanbanBoard, error) {
	resp, err := c.http.Get(c.baseURL + "/api/boards")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list boards: %s — %s", resp.Status, string(raw))
	}
	var boards []kanbanBoard
	if err := json.NewDecoder(resp.Body).Decode(&boards); err != nil {
		return nil, err
	}
	for i := range boards {
		if boards[i].Name == name {
			return &boards[i], nil
		}
	}
	return nil, nil
}

func (c *kanbanClient) createBoard(name string) (*kanbanBoard, error) {
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"description": "Auto-tracked agent dispatches (autoworker, scheduler agent jobs).",
	})
	resp, err := c.http.Post(c.baseURL+"/api/boards", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create board: %s — %s", resp.Status, string(raw))
	}
	var b kanbanBoard
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *kanbanClient) listColumns(boardID string) ([]kanbanColumn, error) {
	url := fmt.Sprintf("%s/api/boards/%s/columns", c.baseURL, boardID)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list columns: %s — %s", resp.Status, string(raw))
	}
	var cols []kanbanColumn
	if err := json.NewDecoder(resp.Body).Decode(&cols); err != nil {
		return nil, err
	}
	return cols, nil
}

func (c *kanbanClient) createColumn(boardID, name string, autoStatus *string) (*kanbanColumn, error) {
	payload := map[string]any{"name": name}
	if autoStatus != nil {
		payload["auto_status"] = *autoStatus
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/boards/%s/columns", c.baseURL, boardID)
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create column %q: %s — %s", name, resp.Status, string(raw))
	}
	var col kanbanColumn
	if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
		return nil, err
	}
	return &col, nil
}
