package login

import (
	"context"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	AgentStore              func() *agents.Store
	Cfg                     *config.Config
	EntraSignIn             func() *EntraSignIn
	FileStore               func() sessionfiles.SessionFileStore
	GrantChecker            *grantcheck.Checker
	HarnessChildTokenSigner func() *HarnessChildTokenSigner
	LoginClock              func() func() time.Time
	PrincipalLookupCache    func() PrincipalLookupCache
	PrincipalStore          func() PrincipalStore
	Store                   *store.Store
}

// PrincipalLookupCache is what this package calls on the server's principalLookupCache.
type PrincipalLookupCache interface {
	LookUp(ctx context.Context, principalID string) (principals.Principal, error)
}

// PrincipalStore is what this package calls on the server's principalStore.
type PrincipalStore interface {
	Get(ctx context.Context, id string, includeDisabledMemberships bool) (*principals.Principal, error)
	ResolveIdentity(ctx context.Context, provider string, tenantID string, subject string) (*principals.IdentityBindingResolution, error)
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
