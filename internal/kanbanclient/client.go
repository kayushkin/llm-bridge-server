// Package kanbanclient is a small HTTP client for the local kanban-store
// (default :8305). Used by autoworker (creates cards) and kanban-curator
// (closes them). Intentionally narrow — only the calls the scheduler binaries
// need; not a general-purpose SDK.
package kanbanclient

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

type Client struct {
	BaseURL string
	HTTP    *http.Client

	// Set by EnsureBoard for callers that want to reuse the resolved IDs.
	BoardID       string
	ColumnsByName map[string]string
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ============================ Boards & columns ============================

type Board struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Column struct {
	ID         string  `json:"id"`
	BoardID    string  `json:"board_id"`
	Name       string  `json:"name"`
	Position   float64 `json:"position"`
	AutoStatus string  `json:"auto_status,omitempty"`
}

// EnsureBoard finds (or creates) a board by name and ensures the named columns
// exist. Caches the resolved ids on the client. Idempotent.
func (c *Client) EnsureBoard(name string, wantColumns []string) error {
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
	c.BoardID = board.ID

	cols, err := c.ListColumns(board.ID)
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
	c.ColumnsByName = have
	return nil
}

// FindBoard returns the board with the given name, or (nil, nil) if not found.
func (c *Client) FindBoard(name string) (*Board, error) {
	return c.findBoardByName(name)
}

func (c *Client) findBoardByName(name string) (*Board, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/boards")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list boards", resp)
	}
	var boards []Board
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

func (c *Client) createBoard(name string) (*Board, error) {
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"description": "Auto-tracked agent dispatches (autoworker, scheduler agent jobs).",
	})
	resp, err := c.HTTP.Post(c.BaseURL+"/api/boards", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, statusErr("create board", resp)
	}
	var b Board
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) ListColumns(boardID string) ([]Column, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s/columns", c.BaseURL, boardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list columns", resp)
	}
	var cols []Column
	if err := json.NewDecoder(resp.Body).Decode(&cols); err != nil {
		return nil, err
	}
	return cols, nil
}

func (c *Client) createColumn(boardID, name string, autoStatus *string) (*Column, error) {
	payload := map[string]any{"name": name}
	if autoStatus != nil {
		payload["auto_status"] = *autoStatus
	}
	body, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(fmt.Sprintf("%s/api/boards/%s/columns", c.BaseURL, boardID),
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, statusErr("create column "+name, resp)
	}
	var col Column
	if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
		return nil, err
	}
	return &col, nil
}

// ============================ Cards ============================

// CardView mirrors kanban-store's BoardView entry: a placement plus the raw
// noteboard item. We treat the item as opaque map[string]any.
type CardView struct {
	Placement struct {
		CardID   string  `json:"card_id"`
		BoardID  string  `json:"board_id"`
		ColumnID string  `json:"column_id"`
		Position float64 `json:"position"`
	} `json:"placement"`
	Item map[string]any `json:"item"`
}

// ColumnView holds a column and its cards (in placement order).
type ColumnView struct {
	Column Column     `json:"column"`
	Cards  []CardView `json:"cards"`
}

// BoardView is the assembled response from /api/boards/:id/cards.
type BoardView struct {
	Board   Board        `json:"board"`
	Columns []ColumnView `json:"columns"`
}

// GetBoardView returns the full board+columns+cards tree.
func (c *Client) GetBoardView(boardID string) (*BoardView, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s/cards", c.BaseURL, boardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("board view", resp)
	}
	var bv BoardView
	if err := json.NewDecoder(resp.Body).Decode(&bv); err != nil {
		return nil, err
	}
	return &bv, nil
}

// CreateCardArgs is the input to CreateCard.
type CreateCardArgs struct {
	ColumnName   string
	Title        string
	Body         string
	Tags         []string
	BusSessionID string // becomes a card_link entity_type=bus_session
	Provider     string // becomes a card_link entity_type=autoworker_provider
}

// CreateCard creates a new card on the bound board+column and attaches the
// standard autoworker links. EnsureBoard must have been called first.
func (c *Client) CreateCard(a CreateCardArgs) (cardID string, err error) {
	if c.BoardID == "" {
		return "", errors.New("kanbanclient: EnsureBoard not called")
	}
	colID, ok := c.ColumnsByName[a.ColumnName]
	if !ok {
		return "", fmt.Errorf("column %q missing on board", a.ColumnName)
	}

	payload := map[string]any{
		"title":     a.Title,
		"body":      a.Body,
		"tags":      a.Tags,
		"column_id": colID,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/boards/%s/cards", c.BaseURL, c.BoardID)
	resp, err := c.HTTP.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", statusErr("create card", resp)
	}
	var created struct {
		Item map[string]any `json:"item"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	id, _ := created.Item["id"].(string)
	if id == "" {
		return "", errors.New("create card: empty item id in response")
	}

	type linkSpec struct{ entityType, entityRef, label string }
	links := []linkSpec{
		{"bus_session", a.BusSessionID, "autoworker"},
		{"autoworker_provider", a.Provider, ""},
	}
	for _, l := range links {
		if l.entityRef == "" {
			continue
		}
		if err := c.AddCardLink(id, l.entityType, l.entityRef, l.label); err != nil {
			return id, fmt.Errorf("link %s/%s: %w", l.entityType, l.entityRef, err)
		}
	}
	return id, nil
}

// CardLink is one (card_id, entity_type, entity_ref) row.
type CardLink struct {
	ID         string `json:"id"`
	CardID     string `json:"card_id"`
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`
}

// AddCardLink attaches a typed pointer to an external entity.
func (c *Client) AddCardLink(cardID, entityType, entityRef, label string) error {
	payload := map[string]any{"entity_type": entityType, "entity_ref": entityRef}
	if label != "" {
		payload["label"] = label
	}
	body, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(
		fmt.Sprintf("%s/api/cards/%s/links", c.BaseURL, cardID),
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return statusErr("create card link", resp)
	}
	return nil
}

// ListCardLinks returns all entity-link rows for a card.
func (c *Client) ListCardLinks(cardID string) ([]CardLink, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/cards/%s/links", c.BaseURL, cardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list card links", resp)
	}
	var links []CardLink
	if err := json.NewDecoder(resp.Body).Decode(&links); err != nil {
		return nil, err
	}
	return links, nil
}

// MoveCard moves a card to a different column on the given board. position
// defaults to 0 (top); pass 0 if you don't care about relative ordering.
// If the destination column has auto_status set, the response will indicate
// whether the noteboard status patch succeeded.
func (c *Client) MoveCard(cardID, boardID, columnID string, position float64) error {
	payload := map[string]any{
		"board_id":  boardID,
		"column_id": columnID,
		"position":  position,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/cards/%s/move", c.BaseURL, cardID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("move card", resp)
	}
	return nil
}

// PatchCardBody updates the noteboard item's body via the kanban-store proxy.
// Used by closure curator to append final-state notes.
func (c *Client) PatchCardBody(cardID, body string) error {
	payload, _ := json.Marshal(map[string]any{"body": body})
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/api/cards/%s", c.BaseURL, cardID), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("patch card", resp)
	}
	return nil
}

// ============================ helpers ============================

func statusErr(op string, resp *http.Response) error {
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("kanban %s: %s — %s", op, resp.Status, strings.TrimSpace(string(raw)))
}
