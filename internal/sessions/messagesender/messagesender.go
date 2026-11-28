// Package messagesender says who wrote a user message.
//
// Every user_message this server records carries the sender's principal id
// and their name at that moment (msg.Event.SenderPrincipalID and
// SenderDisplayName), so the chat can say who spoke in a session several
// people write in. The sender is the request's caller; a message held over
// a restart is sent later without a request, so the hold keeps the sender
// and the replay passes it to sendMessageToSession.
package messagesender

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge/msg"
)

// SenderPrincipalIDOfRequest is the principal who wrote the request's
// message: the caller. Empty when the caller acts as no principal (the
// internal service, a harness).
func SenderPrincipalIDOfRequest(r *http.Request) string {
	if caller, ok := requestcaller.FromRequest(r); ok {
		return caller.PrincipalID
	}
	return ""
}

// Sender is who wrote something a person put into a session, as an
// event carries it. The zero value is "no principal".
type Sender struct {
	principalID string
	displayName string
}

// OfRequest is the request's sender, with the principal's
// current name from principal-store.
func (s *Senders) OfRequest(r *http.Request) (Sender, error) {
	return s.Of(r.Context(), SenderPrincipalIDOfRequest(r))
}

// Of is principalID as a sender, with the principal's current
// name from principal-store. An empty id is no principal.
func (s *Senders) Of(ctx context.Context, principalID string) (Sender, error) {
	if principalID == "" {
		return Sender{}, nil
	}
	if s.PrincipalLookupCache() == nil {
		return Sender{}, fmt.Errorf("the sender %s is named from principal-store, and this server has no principal lookup", principalID)
	}
	principal, err := s.PrincipalLookupCache().LookUp(ctx, principalID)
	if err != nil {
		return Sender{}, fmt.Errorf("read the sender %s from principal-store: %w", principalID, err)
	}
	return Sender{principalID: principalID, displayName: principal.DisplayName}, nil
}

// Stamp writes the sender onto an event.
func (sender Sender) Stamp(event *msg.Event) {
	event.SenderPrincipalID = sender.principalID
	event.SenderDisplayName = sender.displayName
}

// Stamp sets an event's sender from the request.
func (s *Senders) Stamp(r *http.Request, event *msg.Event) error {
	sender, err := s.OfRequest(r)
	if err != nil {
		return err
	}
	sender.Stamp(event)
	return nil
}
