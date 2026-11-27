package server

// Who wrote a user message.
//
// Every user_message this server records carries the sender's principal id
// and their name at that moment (msg.Event.SenderPrincipalID and
// SenderDisplayName), so the chat can say who spoke in a session several
// people write in. The sender is the request's caller; a message held over
// a restart is sent later without a request, so the hold keeps the sender
// and the replay passes it to sendMessageToSession.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge/msg"
)

// senderPrincipalIDOfRequest is the principal who wrote the request's
// message: the caller. Empty when the caller acts as no principal (the
// internal service, a harness).
func senderPrincipalIDOfRequest(r *http.Request) string {
	if caller, ok := requestcaller.FromRequest(r); ok {
		return caller.PrincipalID
	}
	return ""
}

// messageSender is who wrote something a person put into a session, as an
// event carries it. The zero value is "no principal".
type messageSender struct {
	principalID string
	displayName string
}

// messageSenderOfRequest is the request's sender, with the principal's
// current name from principal-store.
func (s *Server) messageSenderOfRequest(r *http.Request) (messageSender, error) {
	return s.messageSenderOf(r.Context(), senderPrincipalIDOfRequest(r))
}

// messageSenderOf is principalID as a sender, with the principal's current
// name from principal-store. An empty id is no principal.
func (s *Server) messageSenderOf(ctx context.Context, principalID string) (messageSender, error) {
	if principalID == "" {
		return messageSender{}, nil
	}
	if s.principalLookupCache == nil {
		return messageSender{}, fmt.Errorf("the sender %s is named from principal-store, and this server has no principal lookup", principalID)
	}
	principal, err := s.principalLookupCache.lookUp(ctx, principalID)
	if err != nil {
		return messageSender{}, fmt.Errorf("read the sender %s from principal-store: %w", principalID, err)
	}
	return messageSender{principalID: principalID, displayName: principal.DisplayName}, nil
}

// stamp writes the sender onto an event.
func (sender messageSender) stamp(event *msg.Event) {
	event.SenderPrincipalID = sender.principalID
	event.SenderDisplayName = sender.displayName
}

// stampMessageSender sets an event's sender from the request.
func (s *Server) stampMessageSender(r *http.Request, event *msg.Event) error {
	sender, err := s.messageSenderOfRequest(r)
	if err != nil {
		return err
	}
	sender.stamp(event)
	return nil
}
