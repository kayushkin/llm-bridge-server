package peers

import (
	"context"
	"net/http"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/sessionactions"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what the peer code reads from the server that composes it.
type Dependencies struct {
	AuthClient               func() *authstoreclient.Client
	Cfg                      *config.Config
	GrantChecker             *grantcheck.Checker
	GrantStore               func() grantcheck.GrantReader
	GrantsPrincipalDirectory func() grants.PrincipalDirectory
	GrantsResourceChecker    func() grants.ResourceChecker
	HarnessStore             func() *machines.Store
	PeerRequestsInFlight     *PeerRequestsInFlight
	PeerSockets              *PeerSocketRegistry
	PrincipalLookupCache     func() PrincipalLookupCache
	PrincipalStore           func() PrincipalStore
	SessionActions           *sessionactions.Service
	Settings                 *servicesettings.Registry
	Store                    *store.Store
	Server                   Host
}

// Host is the server itself as a connected server reaches it: its mux,
// its name, and whether a caller may still drive a session it started.
type Host interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
	CallerStartedSessionIDAndMayDriveIt(ctx context.Context, caller requestcaller.Caller, sessionID string) (bool, error)
	ThisServerName() string
}

// PrincipalLookupCache is what this package calls on the server's principalLookupCache.
type PrincipalLookupCache interface {
	LookUp(ctx context.Context, principalID string) (principals.Principal, error)
	Forget(principalID string)
}

// PrincipalStore is what this package calls on the server's principalStore.
type PrincipalStore interface {
	AddMember(ctx context.Context, groupID string, memberID string) (created bool, err error)
	Create(ctx context.Context, input *principals.Principal) (*principals.Principal, error)
	Get(ctx context.Context, id string, includeDisabledMemberships bool) (*principals.Principal, error)
	Patch(ctx context.Context, id string, patch principals.Patch) (*principals.Principal, error)
	RemoveMember(ctx context.Context, groupID string, memberID string) error
}

// Service answers the routes for connected servers and keeps their sockets,
// messages and proxied calls.
type Service struct {
	Dependencies
	// proxyCalls are the calls made through a connection, for its allowance
	// and for answering a repeated read once.
	proxyCalls PeerProxyCalls
	// selfUpdates serializes finishing an approved notice's update.
	selfUpdates sync.Mutex
}
