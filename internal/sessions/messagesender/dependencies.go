package messagesender

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	PrincipalLookupCache func() PrincipalLookupCache
}

// PrincipalLookupCache is what this package calls on the server's principalLookupCache.
type PrincipalLookupCache interface {
	LookUp(ctx context.Context, principalID string) (principals.Principal, error)
}

// Senders is the package's state and what it was built with.
type Senders struct {
	Dependencies
}
