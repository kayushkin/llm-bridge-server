package server

import (
	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelcredentials"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/oneshot"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"
	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/machines/machineroutes"
	"github.com/kayushkin/llm-bridge-server/internal/peers"
	"github.com/kayushkin/llm-bridge-server/internal/sessionactions"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
)

// composeServices builds the packages split out of this server from its
// fields, each with only what it reads. New calls it once its own fields are
// set; a test that builds a Server by hand calls it the same way.
func (s *Server) composeServices() {
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
		Sessions:               s,
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
		Sessions:         s,
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
		PrincipalStore: func() peers.PrincipalStore { return s.principalStore },
		SessionActions: s.sessionActions,
		Settings:       s.settings,
		Store:          s.store,
		Server:         s,
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
		Sessions:       s,
	}}
	s.chatLinkService = &chatlinks.Service{Dependencies: chatlinks.Dependencies{
		Store:             s.store,
		Settings:          s.settings,
		ReferenceResolver: s.referenceResolver.Load,
		Sessions:          s,
	}}
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
