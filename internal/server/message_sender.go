package server

// Who wrote a user message.
//
// Every user_message this server records carries the sender's principal id
// and their name at that moment (msg.Event.SenderPrincipalID and
// SenderDisplayName), so the chat can say who spoke in a session several
// people write in. The sender is the request's caller; a message held over
// a restart is sent later without a request, so the hold keeps the sender
// and the replay names them on the context (withHeldMessageSender).

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

type heldMessageSenderContextKey struct{}

// withHeldMessageSender names the sender of a held message being replayed.
// Only deliverHeldMessage sets it: a request through the route table never
// carries it, because serveInternally is not reachable from outside.
func withHeldMessageSender(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, heldMessageSenderContextKey{}, principalID)
}

// senderPrincipalIDOfRequest is the principal who wrote the request's
// message: the held message's sender on a replay, else the caller. Empty
// when the caller acts as no principal (the internal service, a harness).
func senderPrincipalIDOfRequest(r *http.Request) string {
	if principalID, ok := r.Context().Value(heldMessageSenderContextKey{}).(string); ok {
		return principalID
	}
	if caller, ok := callerOfRequest(r); ok {
		return caller.principalID
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
	principalID := senderPrincipalIDOfRequest(r)
	if principalID == "" {
		return messageSender{}, nil
	}
	if s.principalLookupCache == nil {
		return messageSender{}, fmt.Errorf("the sender %s is named from principal-store, and this server has no principal lookup", principalID)
	}
	principal, err := s.principalLookupCache.lookUp(r.Context(), principalID)
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
