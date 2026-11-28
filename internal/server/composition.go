package server

import (
	"io/fs"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harnesscatalog"
	"github.com/kayushkin/llm-bridge-server/internal/keepawake"
	"github.com/kayushkin/llm-bridge-server/internal/serviceinventory/inventoryroutes"
	"github.com/kayushkin/llm-bridge-server/internal/sessions"
	"github.com/kayushkin/llm-bridge-server/internal/userinterface"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/login"
	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelcredentials"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/oneshot"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"
	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/inprocesshttp"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/machines/machineroutes"
	"github.com/kayushkin/llm-bridge-server/internal/peers"
	"github.com/kayushkin/llm-bridge-server/internal/sessionactions"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/permissiongate"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
)

// composeServices builds the packages split out of this server from its
// fields, each with only what it reads. New calls it once its own fields are
// set; a test that builds a Server by hand calls it the same way.
func (s *Server) composeServices() {
	// The session core is reached by most of the others and reaches most of
	// them, so it is made first and given its dependencies last.
	s.sessions = &sessions.Service{}
	s.userInterface = &userinterface.Service{Dependencies: userinterface.Dependencies{UserInterfaceBuild: func() fs.FS { return s.userInterfaceBuild }}}
	s.serviceInventoryRoutes = &inventoryroutes.Service{Dependencies: inventoryroutes.Dependencies{Cfg: s.cfg}}
	s.harnessCatalog = &harnesscatalog.Service{Dependencies: harnesscatalog.Dependencies{
		AgentStore: func() *agents.Store { return s.agentStore },
		CfState:    s.cfState,
		Cfg:        s.cfg,
		Store:      s.store,
	}}
	s.grantChecker = &grantcheck.Checker{Dependencies: grantcheck.Dependencies{
		GrantStore:               func() grantcheck.GrantReader { return s.grantStore },
		GrantsPrincipalDirectory: func() grants.PrincipalDirectory { return s.grantsPrincipalDirectory },
		PrincipalLookupCache: func() grantcheck.PrincipalLookupCache {
			if s.principalLookupCache == nil {
				return nil
			}
			return s.principalLookupCache
		},
	}}
	s.messageSenders = &messagesender.Senders{Dependencies: messagesender.Dependencies{
		PrincipalLookupCache: func() messagesender.PrincipalLookupCache {
			if s.principalLookupCache == nil {
				return nil
			}
			return s.principalLookupCache
		},
	}}
	s.sessionFiles = &sessionfiles.Service{Dependencies: sessionfiles.Dependencies{
		Cfg:            s.cfg,
		FileStore:      func() sessionfiles.SessionFileStore { return s.fileStore },
		Harness:        s.harness,
		MessageSenders: s.messageSenders,
		Store:          s.store,
	}}
	s.modelCredentials = &modelcredentials.Service{Dependencies: modelcredentials.Dependencies{
		AuthClient: func() *authstoreclient.Client { return s.authClient },
		ModelStore: func() *models.Store { return s.modelStore },
	}}
	s.login = &login.Service{Dependencies: login.Dependencies{
		AgentStore:              func() *agents.Store { return s.agentStore },
		Cfg:                     s.cfg,
		EntraSignIn:             func() *login.EntraSignIn { return s.entraSignIn },
		FileStore:               func() sessionfiles.SessionFileStore { return s.fileStore },
		GrantChecker:            s.grantChecker,
		HarnessChildTokenSigner: func() *login.HarnessChildTokenSigner { return s.harnessChildTokenSigner },
		LoginClock:              func() func() time.Time { return s.loginClock },
		PrincipalLookupCache: func() login.PrincipalLookupCache {
			if s.principalLookupCache == nil {
				return nil
			}
			return s.principalLookupCache
		},
		PrincipalStore: func() login.PrincipalStore { return s.principalStore },
		Store:          s.store,
	}}
	s.sessionActions = &sessionactions.Service{Dependencies: sessionactions.Dependencies{
		Harness:                s.harness,
		HarnessStore:           func() *machines.Store { return s.harnessStore },
		ModelStore:             func() *models.Store { return s.modelStore },
		Mux:                    s.mux,
		SchedulerMountPrefix:   schedulerMountPrefix,
		RepoRegistryForDeploys: func() sessionactions.RepoRegistryForDeploys { return s.repoRegistryForDeploys },
		SessionActionModels:    func() sessionactions.SessionActionModels { return s.sessionActionModels },
		Settings:               s.settings,
		Store:                  s.store,
		Sessions:               s.sessions,
		ServerReads:            s,
		PostingEnvironments:    s.login,
	}}
	s.signals = &signals.Service{Dependencies: signals.Dependencies{
		Harness:          s.harness,
		HarnessStore:     func() *machines.Store { return s.harnessStore },
		IssueReads:       func() *issuereads.Reader { return s.issueReads },
		MailMessages:     func() signals.MailMessageReader { return s.mailMessages },
		MessageSenders:   s.messageSenders,
		ParkedAsks:       s.parkedAsks,
		QuestionTriage:   func() *signals.QuestionTriage { return s.questionTriage },
		SignalClassifier: func() *signals.SignalClassifier { return s.signalClassifier },
		Store:            s.store,
		Sessions:         s.sessions,
	}}
	s.peers = &peers.Service{Dependencies: peers.Dependencies{
		AuthClient:               func() *authstoreclient.Client { return s.authClient },
		Cfg:                      s.cfg,
		GrantChecker:             s.grantChecker,
		GrantStore:               func() grantcheck.GrantReader { return s.grantStore },
		GrantsPrincipalDirectory: func() grants.PrincipalDirectory { return s.grantsPrincipalDirectory },
		GrantsResourceChecker:    func() grants.ResourceChecker { return s.grantsResourceChecker },
		HarnessStore:             func() *machines.Store { return s.harnessStore },
		PeerRequestsInFlight:     s.peerRequestsInFlight,
		PeerSockets:              s.peerSockets,
		PrincipalLookupCache: func() peers.PrincipalLookupCache {
			if s.principalLookupCache == nil {
				return nil
			}
			return s.principalLookupCache
		},
		PrincipalStore:  func() peers.PrincipalStore { return s.principalStore },
		SessionActions:  s.sessionActions,
		Settings:        s.settings,
		Store:           s.store,
		Server:          s,
		SessionStarters: s.login,
	}}
	s.operationRoutes = &operationroutes.Service{Dependencies: operationroutes.Dependencies{
		GrantChecker:   s.grantChecker,
		IssueReads:     func() *issuereads.Reader { return s.issueReads },
		ModelStore:     func() *models.Store { return s.modelStore },
		Operations:     func() *operations.Coordinator { return s.operations },
		PrincipalStore: func() operationroutes.PrincipalStore { return s.principalStore },
		Settings:       s.settings,
	}}
	s.oneShot = &oneshot.Service{Dependencies: oneshot.Dependencies{
		FileStore:     func() sessionfiles.SessionFileStore { return s.fileStore },
		HarnessStore:  func() *machines.Store { return s.harnessStore },
		IssueReads:    func() *issuereads.Reader { return s.issueReads },
		ModelStore:    func() *models.Store { return s.modelStore },
		OneShotRouter: func() *modelrouting.Router { return s.oneShotRouter },
		SessionFiles:  s.sessionFiles,
		Settings:      s.settings,
		Store:         s.store,
		Server:        s,
	}}
	s.machineRoutes = &machineroutes.Service{Dependencies: machineroutes.Dependencies{
		GrantChecker: s.grantChecker,
		Harness:      s.harness,
		HarnessStore: func() *machines.Store { return s.harnessStore },
		Store:        s.store,
	}}
	s.briefService = &briefs.Service{Dependencies: briefs.Dependencies{
		Harness:        s.harness,
		Store:          s.store,
		TurnClassifier: s.turnClassifierForBriefs,
		Sessions:       s.sessions,
	}}
	s.chatLinkService = &chatlinks.Service{Dependencies: chatlinks.Dependencies{
		Store:             s.store,
		Settings:          s.settings,
		ReferenceResolver: s.referenceResolver.Load,
		Sessions:          s.sessions,
	}}
	permissionGate := &permissiongate.Service{Dependencies: permissiongate.Dependencies{
		BridgePrefs:      s.bridgePrefs,
		Harness:          s.harness,
		HookStore:        s.hookStore,
		ParkedAsks:       s.parkedAsks,
		PermissionEngine: func() permissiongate.PermissionEngine { return s.permissionEngine },
		Signals:          s.signals,
		Store:            s.store,
		Sessions:         s.sessions,
	}}
	s.sessions.Dependencies = sessions.Dependencies{
		PermissionGate:  permissionGate,
		AgentStore:      func() *agents.Store { return s.agentStore },
		BridgePrefs:     s.bridgePrefs,
		BriefService:    s.briefService,
		BundleStore:     func() sessions.BundleStore { return s.bundleStore },
		Cfg:             s.cfg,
		ChatLinkService: s.chatLinkService,
		GrantChecker:    s.grantChecker,
		Harness:         s.harness,
		HarnessStore:    func() *machines.Store { return s.harnessStore },
		HookStore:       s.hookStore,
		IssueReads:      func() *issuereads.Reader { return s.issueReads },
		KeepAwake:       func() *keepawake.Controller { return s.keepAwake },
		Login:           s.login,
		MachineRoutes:   s.machineRoutes,
		MessageSenders:  s.messageSenders,
		ModelStore:      func() *models.Store { return s.modelStore },
		OneShotRouter:   func() *modelrouting.Router { return s.oneShotRouter },
		ParkedAsks:      s.parkedAsks,
		Peers:           s.peers,
		PrincipalLookupCache: func() sessions.PrincipalLookupCache {
			if s.principalLookupCache == nil {
				return nil
			}
			return s.principalLookupCache
		},
		PrincipalStore: func() sessions.PrincipalStore { return s.principalStore },
		ResponseCache:  s.responseCache,
		SessionHub:     s.sessionHub,
		SessionLog:     s.sessionLog.Load,
		Settings:       s.settings,
		Signals:        s.signals,
		SnapshotStore:  func() *snapshots.Store { return s.snapshotStore },
		Store:          s.store,
		ToolRegistry:   func() sessions.ToolRegistry { return s.toolRegistry },
	}
}

// EnableOperations attaches the coordinator the operation routes use. Until
// it is called they answer 503 operations_not_configured.
func (s *Server) EnableOperations(coordinator *operations.Coordinator) {
	s.operations = coordinator
}

// ModelListPrice reads a model's list price from model-store. A model it
// does not know, or one whose prices are both zero, has no known price: a
// zero there means nobody entered one, and budgets must not treat it as free.
func (s *Server) ModelListPrice(model string) (inputPerMillion, outputPerMillion float64, known bool) {
	if s.modelStore == nil || model == "" {
		return 0, 0, false
	}
	resolved, err := s.modelStore.ResolveModel(model)
	if err != nil || resolved == nil || (resolved.InputCost <= 0 && resolved.OutputCost <= 0) {
		return 0, 0, false
	}
	return resolved.InputCost, resolved.OutputCost, true
}

// sessionActionModelsInThisServer is the one-shot path and model-store's list
// prices, which model_call and the command reviewer use.
type sessionActionModelsInThisServer struct {
	*oneshot.Service
	server *Server
}

// ModelListPrice is the server's model-store list price.
func (models sessionActionModelsInThisServer) ModelListPrice(model string) (inputPerMillion, outputPerMillion float64, known bool) {
	return models.server.ModelListPrice(model)
}

// EnableSessionLog serves log-store's routes from handler and has the harness
// manager write and read every session event through events. main calls it
// once the module is provisioned, migrated and open, before any session
// starts or resumes. In the server both are the log-store module's server;
// a test may give a fake log-store's handler and the HTTP client over it.
func (s *Server) EnableSessionLog(handler http.Handler, events harness.SessionEventLog) {
	s.sessionLog.Store(&sessions.SessionLogModule{Handler: handler, Client: inprocesshttp.Client(handler)})
	s.harness.SetSessionEventLog(events)
}

// UseSessionEventStore names where the harness manager stores every event
// and where this server reads them back: the stream, the stream head, crash
// recovery, the renamer, the brief pass, the reaper and wakeup redelivery:
// the session log, the only copy. main calls it once, after EnableSessionLog
// and before any session starts or resumes.
func (s *Server) UseSessionEventStore(events sessionlog.Store) {
	s.harness.SetSessionEventStore(events)
}
