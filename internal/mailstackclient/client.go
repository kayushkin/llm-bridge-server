// Package mailstackclient reads one message's headers out of mailstack, for
// question triage to address a drafted reply to the person who sent the mail
// a card was filed from.
//
// It reads and never writes: mailstack has no send route, and this client
// must not be the place one is invented.
package mailstackclient

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

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client. An empty token is refused: mailstack answers 401 to
// every read without one, so a client built without it can only ever fail,
// and the caller is better off knowing that at construction.
func New(baseURL, token string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("mailstack url is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("mailstack token is required")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 5 * time.Second},
	}, nil
}

// Address is a mail address with its display name, as mailstack returns it.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// MessageHeaders is the part of a message triage needs to draft a reply: who
// sent it and what it was called. The body is deliberately not read here —
// the card already carries the classifier's summary of it, and that is what
// triage reads.
type MessageHeaders struct {
	ID        string  `json:"id"`
	AccountID string  `json:"account_id"`
	Subject   string  `json:"subject"`
	From      Address `json:"from"`
	MessageID string  `json:"message_id,omitempty"`
}

// ParseLocator splits a kanban "email" link ref — "<account_id>:<provider
// message id>", the only pair mailstack can resolve — into its two parts.
func ParseLocator(ref string) (accountID, providerID string, err error) {
	accountID, providerID, ok := strings.Cut(ref, ":")
	if !ok || accountID == "" || providerID == "" {
		return "", "", fmt.Errorf("email ref %q is not <account_id>:<message id>", ref)
	}
	return accountID, providerID, nil
}

// GetMessageHeaders fetches one message by its locator.
func (c *Client) GetMessageHeaders(ctx context.Context, accountID, providerID string) (*MessageHeaders, error) {
	if accountID == "" || providerID == "" {
		return nil, fmt.Errorf("account id and message id are required")
	}
	endpoint := fmt.Sprintf("%s/api/messages/%s?account=%s", c.baseURL, url.PathEscape(providerID), url.QueryEscape(accountID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build message request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mailstack message lookup: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read message response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mailstack message lookup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// mailstack answers {"meta": {...headers...}, "body": ...}. Only meta is
	// read; the body stays on the wire.
	var envelope struct {
		Meta MessageHeaders `json:"meta"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("parse message response: %w", err)
	}
	if envelope.Meta.From.Email == "" {
		// A message with no sender cannot be replied to, and a caller that
		// got an empty address back would draft a reply to nobody without
		// knowing. Measured once already: reading the top level instead of
		// meta gave exactly that, silently.
		return nil, fmt.Errorf("message %s in account %s carries no sender address", providerID, accountID)
	}
	return &envelope.Meta, nil
}
