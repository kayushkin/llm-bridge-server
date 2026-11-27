package server

import (
	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/chatlinks"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/briefs"
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
