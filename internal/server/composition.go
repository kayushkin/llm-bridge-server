package server

import (
	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"
	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/machines/machineroutes"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/messagesender"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"
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
	s.operationRoutes = &operationroutes.Service{Dependencies: operationroutes.Dependencies{
		GrantChecker:   s.grantChecker,
		IssueReads:     func() *issuereads.Reader { return s.issueReads },
		ModelStore:     func() *models.Store { return s.modelStore },
		Operations:     func() *operations.Coordinator { return s.operations },
		PrincipalStore: func() operationroutes.PrincipalStore { return s.principalStore },
		Settings:       s.settings,
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
