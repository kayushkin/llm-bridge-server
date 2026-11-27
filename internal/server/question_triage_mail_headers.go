package server

// Reading one message's headers out of mailstack, for question triage to
// address a drafted reply to the person who sent the mail a card was filed
// from.
//
// mailstack runs in this process (internal/mail); the read is a Go call, as
// the internal service, which is past every per-person check, as the
// standalone service's bearer token was. It reads and never writes: mailstack
// has no send route, and this must not be the place one is invented.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mail/backend"
)

// mailMessageReader is mailstack as question triage reads it: *mail.Server's
// ReadMessage, which reads as the internal service. mailstack reaches a mail
// provider only when the server's provider connections are on; otherwise a
// message on a Gmail, Outlook or IMAP account is an error.
type mailMessageReader interface {
	ReadMessage(ctx context.Context, accountID, folderID, id string) (backend.Message, error)
}

// mailMessageReadTimeout bounds one read, as the HTTP client's timeout did: a provider
// read can hang, and a local account answers from Postgres.
const mailMessageReadTimeout = 5 * time.Second

// mailAddress is a mail address with its display name, as mailstack returns it.
type mailAddress struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// mailMessageHeaders is the part of a message triage needs to draft a reply: who
// sent it and what it was called. The body is deliberately not read here —
// the card already carries the classifier's summary of it, and that is what
// triage reads.
type mailMessageHeaders struct {
	ID        string      `json:"id"`
	AccountID string      `json:"account_id"`
	Subject   string      `json:"subject"`
	From      mailAddress `json:"from"`
	MessageID string      `json:"message_id,omitempty"`
}

// parseEmailLocator splits a kanban "email" link ref — "<account_id>:<provider
// message id>", the only pair mailstack can resolve — into its two parts.
func parseEmailLocator(ref string) (accountID, providerID string, err error) {
	accountID, providerID, ok := strings.Cut(ref, ":")
	if !ok || accountID == "" || providerID == "" {
		return "", "", fmt.Errorf("email ref %q is not <account_id>:<message id>", ref)
	}
	return accountID, providerID, nil
}

// readMailMessageHeaders fetches one message's headers by its locator.
func readMailMessageHeaders(ctx context.Context, mail mailMessageReader, accountID, providerID string) (*mailMessageHeaders, error) {
	if accountID == "" || providerID == "" {
		return nil, fmt.Errorf("account id and message id are required")
	}
	ctx, cancel := context.WithTimeout(ctx, mailMessageReadTimeout)
	defer cancel()
	message, err := mail.ReadMessage(ctx, accountID, "", providerID)
	if err != nil {
		return nil, fmt.Errorf("mailstack message lookup: %w", err)
	}
	// Only the headers are read; the body stays in mailstack.
	headers := mailMessageHeaders{ID: message.Meta.ID, AccountID: message.Meta.AccountID, Subject: message.Meta.Subject,
		From: mailAddress{Name: message.Meta.From.Name, Email: message.Meta.From.Email}, MessageID: message.Meta.MessageID}
	if headers.From.Email == "" {
		// A message with no sender cannot be replied to, and a caller that
		// got an empty address back would draft a reply to nobody without
		// knowing. Measured once already: reading the top level instead of
		// meta gave exactly that, silently.
		return nil, fmt.Errorf("message %s in account %s carries no sender address", providerID, accountID)
	}
	return &headers, nil
}
