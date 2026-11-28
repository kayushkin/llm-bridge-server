package grantcheck

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	GrantStore               func() GrantReader
	GrantsPrincipalDirectory func() grants.PrincipalDirectory
	PrincipalLookupCache     func() PrincipalLookupCache
}

// PrincipalLookupCache is what this package calls on the server's principalLookupCache.
type PrincipalLookupCache interface {
	LookUp(ctx context.Context, principalID string) (principals.Principal, error)
}

// Checker is the package's state and what it was built with.
type Checker struct {
	Dependencies
}
