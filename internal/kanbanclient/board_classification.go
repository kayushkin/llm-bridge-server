package kanbanclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kayushkin/llm-bridge/msg"
)

// ClassificationSettings reads the complete owning contract as the caller.
func (c *Client) ClassificationSettings(_ context.Context, boardID, principalID string) (msg.BoardClassification, error) {
	if principalID == "" {
		return msg.BoardClassification{}, fmt.Errorf("classification settings: a read as a principal names one")
	}
	classification, err := c.store.BoardClassification(asPrincipal(principalID), boardID)
	if err != nil {
		return msg.BoardClassification{}, failureAsPrincipal("classification settings", err)
	}
	if classification.BoardID != boardID {
		return *classification, fmt.Errorf("classification settings name another board")
	}
	return *classification, nil
}

// ClassificationCard is the card's page of its board, as the caller sees it,
// in the form the board's card-page route answers. The executor extracts its
// source snapshot; no narrowed copy of kanban's card/page contracts lives here.
func (c *Client) ClassificationCard(_ context.Context, boardID, cardID, principalID string) (json.RawMessage, error) {
	if principalID == "" {
		return nil, fmt.Errorf("classification card: a read as a principal names one")
	}
	page, err := c.store.BoardCardPage(asPrincipal(principalID), boardID, url.Values{"card_id": {cardID}, "limit": {"1"}, "offset": {"0"}})
	if err != nil {
		return nil, failureAsPrincipal("classification card", err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		return nil, fmt.Errorf("classification card: %w", err)
	}
	return encoded, nil
}

// PublishClassification publishes the classifier's decision as the internal
// service, and answers the decision kanban-store keeps. The effect headers are
// for a call over the network; kanban-store's route never read them, and a
// Go call has nowhere to carry them, so they are not used.
func (c *Client) PublishClassification(_ context.Context, publication msg.BoardClassificationDecisionPublication, _ http.Header) (json.RawMessage, error) {
	decision, _, err := c.store.PublishClassificationDecision(internalService, publication)
	if err != nil {
		return nil, failure("classification publication", err)
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil, fmt.Errorf("classification publication: %w", err)
	}
	return encoded, nil
}
