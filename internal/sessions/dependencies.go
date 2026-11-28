package sessions

import (
	"context"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/harnesscatalog"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/permissiongate"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/login"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/agents/bundles"
	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/keepawake"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/machines/machineroutes"
	"github.com/kayushkin/llm-bridge-server/internal/peers"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/parkedasks"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	AgentStore           func() *agents.Store
	BridgePrefs          *BridgePrefsStore
	BriefService         *briefs.Service
	BundleStore          func() BundleStore
	CfState              *harnesscatalog.ConformanceState
	Cfg                  *config.Config
	ChatLinkService      *chatlinks.Service
	GrantChecker         *grantcheck.Checker
	Harness              *harness.Manager
	HarnessStore         func() *machines.Store
	HookStore            *hooks.Store
	IssueReads           func() *issuereads.Reader
	KeepAwake            func() *keepawake.Controller
	Login                *login.Service
	MachineRoutes        *machineroutes.Service
	MessageSenders       *messagesender.Senders
	ModelStore           func() *models.Store
	OneShotRouter        func() *modelrouting.Router
	ParkedAsks           *parkedasks.Asks
	Peers                *peers.Service
	PermissionGate       *permissiongate.Service
	PrincipalLookupCache func() PrincipalLookupCache
	PrincipalStore       func() PrincipalStore
	ResponseCache        *ResponseCache
	SessionHub           *SessionHub
	SessionLog           func() *SessionLogModule
	Settings             *servicesettings.Registry
	Signals              *signals.Service
	SnapshotStore        func() *snapshots.Store
	Store                *store.Store
	ToolRegistry         func() ToolRegistry
}

// BundleStore is what this package calls on the server's bundleStore.
type BundleStore interface {
	CheckSessionBundleExists(ctx context.Context, bundleID string) error
	ResolveSessionBundle(ctx context.Context, bundleID string) (*bundles.ResolveResponse, error)
}

// PrincipalLookupCache is what this package calls on the server's principalLookupCache.
type PrincipalLookupCache interface {
	LookUp(ctx context.Context, principalID string) (principals.Principal, error)
}

// PrincipalStore is what this package calls on the server's principalStore.
type PrincipalStore interface {
	Get(ctx context.Context, id string, includeDisabledMemberships bool) (*principals.Principal, error)
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
	DiscoveryMu                 sync.Mutex
	HarnessStartLockBySessionID sync.Map
	RenamesInFlight             sync.Map
	RestartDrain                RestartDrainState
}
