// Package referencestore is kanban-store's client for reference-store, which
// owns the list of reference types: what each kind of record a card can link
// is called, what its ids look like and where it is fetched. kanban-store used
// to keep that list itself; it now serves reference-store's list unchanged at
// GET /api/entity-types and checks a ticket's source type against it.
package referencestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client reads one reference-store's types, keeping them for TTL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	TTL     time.Duration

	mu        sync.Mutex
	rows      []json.RawMessage
	names     map[string]bool
	fetchedAt time.Time
}

// New returns a client that rereads the list every five minutes.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 3 * time.Second}, TTL: 5 * time.Minute}
}

// Types answers every reference type row, as reference-store serves it. A
// reference-store that cannot answer is an error, never an empty list.
func (c *Client) Types(ctx context.Context) ([]json.RawMessage, error) {
	rows, _, err := c.load(ctx)
	return rows, err
}

// Known says whether a type name is on the list.
func (c *Client) Known(ctx context.Context, typeName string) (bool, error) {
	_, names, err := c.load(ctx)
	if err != nil {
		return false, err
	}
	return names[typeName], nil
}

func (c *Client) load(ctx context.Context) ([]json.RawMessage, map[string]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows != nil && time.Since(c.fetchedAt) < c.TTL {
		return c.rows, c.names, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/types", nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("reference-store did not answer GET /types: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("reference-store answered GET /types with %s", response.Status)
	}
	var answer struct {
		Types []json.RawMessage `json:"types"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, nil, fmt.Errorf("reference-store's GET /types: %w", err)
	}
	names := make(map[string]bool, len(answer.Types))
	for _, row := range answer.Types {
		var named struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(row, &named); err != nil || named.Type == "" {
			return nil, nil, fmt.Errorf("reference-store's GET /types holds a row with no type")
		}
		names[named.Type] = true
	}
	c.rows, c.names, c.fetchedAt = answer.Types, names, time.Now()
	return c.rows, c.names, nil
}
