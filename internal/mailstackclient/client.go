// Package mailstackclient reads one message's headers out of mailstack, for
// question triage to address a drafted reply to the person who sent the mail
// a card was filed from.
//
// mailstack runs in this process (internal/mail/mailstack); the client calls
// it in Go, as the internal service, which is past every per-person check, as
// the standalone service's bearer token was.
//
// It reads and never writes: mailstack has no send route, and this client
// must not be the place one is invented.
package mailstackclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/backend"
)

// MessageReader is mailstack as the client reads it: *mailstack.Server's
// ReadMessage, which reads as the internal service.
type MessageReader interface {
	ReadMessage(ctx context.Context, accountID, folderID, id string) (backend.Message, error)
}

type Client struct {
	mail MessageReader
}

// readTimeout bounds one read, as the HTTP client's timeout did: a provider
// read can hang, and a local account answers from Postgres.
const readTimeout = 5 * time.Second

// New builds a client of mail, mailstack in this process. mailstack reaches a
// mail provider only when the server's provider connections are on;
// otherwise a message on a Gmail, Outlook or IMAP account is an error.
func New(mail MessageReader) *Client {
	if mail == nil {
		panic("mailstackclient: New needs mailstack")
	}
	return &Client{mail: mail}
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
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := c.mail.ReadMessage(ctx, accountID, "", providerID)
	if err != nil {
		return nil, fmt.Errorf("mailstack message lookup: %w", err)
	}
	// Only the headers are read; the body stays in mailstack.
	headers := MessageHeaders{ID: message.Meta.ID, AccountID: message.Meta.AccountID, Subject: message.Meta.Subject,
		From: Address{Name: message.Meta.From.Name, Email: message.Meta.From.Email}, MessageID: message.Meta.MessageID}
	if headers.From.Email == "" {
		// A message with no sender cannot be replied to, and a caller that
		// got an empty address back would draft a reply to nobody without
		// knowing. Measured once already: reading the top level instead of
		// meta gave exactly that, silently.
		return nil, fmt.Errorf("message %s in account %s carries no sender address", providerID, accountID)
	}
	return &headers, nil
}
