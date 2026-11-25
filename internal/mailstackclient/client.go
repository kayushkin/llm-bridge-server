// Package mailstackclient reads one message's headers out of mailstack, for
// question triage to address a drafted reply to the person who sent the mail
// a card was filed from.
//
// mailstack runs in this process (internal/mail/mailstack); the client calls
// its handler with no socket, as the internal service, which is past every
// per-person check, as the standalone service's bearer token was.
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

	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack"
)

type Client struct {
	http *http.Client
}

// New builds a client that calls handler, mailstack's own, in this process.
// The handler reaches a mail provider only when the server's provider
// connections are on; otherwise a message on a Gmail, Outlook or IMAP account
// is 503 and GetMessageHeaders an error.
func New(handler http.Handler) *Client {
	if handler == nil {
		panic("mailstackclient: New needs mailstack's handler")
	}
	client := inprocesshttp.Client(handler)
	// A provider read is bounded as the HTTP client's was; a local account
	// answers from Postgres.
	client.Timeout = 5 * time.Second
	return &Client{http: client}
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
	endpoint := fmt.Sprintf("%s/api/messages/%s?account=%s", inprocesshttp.BaseURL, url.PathEscape(providerID), url.QueryEscape(accountID))
	ctx = mailstack.ContextWithCaller(ctx, mailstack.Caller{InternalService: true})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build message request: %w", err)
	}
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
