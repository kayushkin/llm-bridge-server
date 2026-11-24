package journalstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NoteboardClient creates todos in noteboard, which owns them. This service
// keeps only the todo's id, written into the entry as a ref: link.
type NoteboardClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewNoteboardClient talks to noteboard at baseURL.
func NewNoteboardClient(baseURL string) *NoteboardClient {
	return &NoteboardClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type noteboardTodo struct {
	Type  string   `json:"type"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
	Links []string `json:"links"`
}

// CreateTodo creates an open todo and returns the id noteboard gave it.
func (c *NoteboardClient) CreateTodo(title, body string, tags, links []string) (string, error) {
	payload, err := json.Marshal(noteboardTodo{Type: "todo", Title: title, Body: body, Tags: tags, Links: links})
	if err != nil {
		return "", err
	}
	response, err := c.HTTP.Post(c.BaseURL+"/api/items", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: noteboard unreachable: %v", ErrUpstream, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read noteboard's answer: %v", ErrUpstream, err)
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: noteboard answered %d: %s", ErrUpstream, response.StatusCode, bytes.TrimSpace(answer))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("%w: noteboard answered %d with no item id: %s", ErrUpstream, response.StatusCode, bytes.TrimSpace(answer))
	}
	return created.ID, nil
}
