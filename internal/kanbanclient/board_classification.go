package kanbanclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// ClassificationSettings reads the complete owning contract as the caller.
func (c *Client) ClassificationSettings(ctx context.Context, boardID, principalID string) (msg.BoardClassification, error) {
	var out msg.BoardClassification
	body, err := c.getAsPrincipal(ctx, c.baseURL+"/api/boards/"+url.PathEscape(boardID)+"/classification", principalID, "classification settings")
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	if out.BoardID != boardID {
		return out, fmt.Errorf("classification settings name another board")
	}
	return out, nil
}

// ClassificationCard relays the owning response in full. The executor extracts
// its source snapshot; no narrowed copy of kanban's card/page contracts lives here.
func (c *Client) ClassificationCard(ctx context.Context, boardID, cardID, principalID string) (json.RawMessage, error) {
	endpoint := c.baseURL + "/api/boards/" + url.PathEscape(boardID) + "/card-page?" + url.Values{"card_id": {cardID}, "limit": {"1"}, "offset": {"0"}}.Encode()
	return c.getAsPrincipal(ctx, endpoint, principalID, "classification card")
}

func (c *Client) PublishClassification(ctx context.Context, publication msg.BoardClassificationDecisionPublication, headers http.Header) (json.RawMessage, error) {
	body, err := json.Marshal(publication)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/boards/"+url.PathEscape(publication.BoardID)+"/classification/decisions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := readKanbanAnswer(response.Body, "classification publication")
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("publish classification: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("publication returned invalid JSON")
	}
	return data, nil
}
