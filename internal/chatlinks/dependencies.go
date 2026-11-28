package chatlinks

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionanswer"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	ChatLinks         *ChatLinks
	ReferenceResolver func() *references.Resolver
	Settings          *servicesettings.Registry
	Store             *store.Store
	Sessions          SessionMessenger
}

// SessionMessenger is what this package calls on the server that composes it.
type SessionMessenger interface {
	SendMessageFromThisServer(ctx context.Context, sessionID string, req msg.SendMessageRequest, senderPrincipalID string) sessionanswer.Answer
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
