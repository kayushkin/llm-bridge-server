// Package kanbanclient is a minimal HTTP client for kanban-store's
// entity-link reverse lookup. bridge-server uses it to answer one question:
// which noteboard todo is this session linked to?
//
// kanban-store owns that link. A card id in kanban-store IS a noteboard item
// id — cards have no rows of their own, only placements and links against the
// noteboard item — so the card ids it returns for a session are todo ids
// directly, with no second join and no name matching.
package kanbanclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a kanban-store reverse-lookup caller. Safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// New constructs a Client. baseURL is the kanban-store root (e.g.
// "http://localhost:8305").
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Loopback HTTP against a SQLite reverse index — typically
		// sub-millisecond. The timeout is a ceiling so a wedged store fails
		// the lookup quickly instead of holding up whatever raised the
		// signal.
		http: &http.Client{Timeout: 3 * time.Second},
	}
}

// entityCard is one row of GET /api/entities/{type}/{ref}/cards. The route
// returns parallel card_id/item entries so a caller can spot an orphan — a
// link whose noteboard item is gone, which arrives as a non-empty card_id
// with a null item.
type entityCard struct {
	CardID string          `json:"card_id"`
	Item   json.RawMessage `json:"item"`
}

// LinkedTodoForSession returns the noteboard todo id linked to this session,
// or "" when the session has no link.
//
// A session can be linked to several cards — the kanban classifier mints one
// per distinct piece of work it recognizes in a session — so this picks the
// oldest link. A session's reason for existing is recorded before it does
// anything: the autoworker links its dispatch todo at fire time, and every
// later link describes work the session has since done. The oldest link is
// therefore the todo the session is *for*, and the ordering that makes it
// reachable is kanban-store's, not a re-sort here.
//
// An orphaned link is skipped rather than returned. Its card id would still
// resolve to nothing in a todo view, and pointing a signal at a deleted item
// is worse than leaving it unlinked.
func (c *Client) LinkedTodoForSession(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id is required")
	}
	endpoint := fmt.Sprintf("%s/api/entities/session/%s/cards", c.baseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build linked-todo request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("kanban-store linked-todo lookup: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read linked-todo response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kanban-store linked-todo lookup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cards []entityCard
	if err := json.Unmarshal(body, &cards); err != nil {
		return "", fmt.Errorf("parse linked-todo response: %w", err)
	}
	for _, card := range cards {
		if card.CardID == "" {
			continue
		}
		// A null item is an orphan: the link outlived the noteboard item.
		if len(card.Item) == 0 || string(card.Item) == "null" {
			continue
		}
		return card.CardID, nil
	}
	return "", nil
}
