package server

// registerLoginAndStoreProxyRoutes mounts the login routes, the login session
// and identity binding administration routes, and the kanban-store and
// grant-store proxies. They are always mounted: a switched-off sign-in method
// answers 404 from its handler, so the route table does not depend on
// settings.
func (s *Server) registerLoginAndStoreProxyRoutes() {
	s.mux.HandleFunc("POST /auth/demo-login", s.handleDemoLogin)
	s.mux.HandleFunc("GET /auth/entra/start", s.handleEntraSignInStart)
	s.mux.HandleFunc("GET /auth/entra/callback", s.handleEntraSignInCallback)
	s.mux.HandleFunc("GET /auth/principal", s.handleGetLoggedInPrincipal)
	s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /auth/sessions", s.handleListLoginSessions)
	s.mux.HandleFunc("POST /auth/sessions/{id}/revoke", s.handleRevokeLoginSession)
	s.mux.HandleFunc("GET /auth/login-attempts", s.handleListLoginAttempts)
	s.mux.HandleFunc("POST /service-principal-tokens", s.handleIssueServicePrincipalToken)
	s.mux.HandleFunc("GET /service-principal-tokens", s.handleListServicePrincipalTokens)
	s.mux.HandleFunc("GET /self", s.handleSelf)
	s.mux.HandleFunc("POST /service-principal-tokens/{id}/revoke", s.handleRevokeServicePrincipalToken)
	s.mux.HandleFunc("PUT /sessions/{id}/starter", s.handleRecordSessionStarter)
	s.mux.HandleFunc("GET /admin/identity-bindings", s.handleListIdentityBindings)
	s.mux.HandleFunc("POST /admin/identity-bindings", s.handleLinkIdentityBinding)
	s.mux.HandleFunc("POST /admin/identity-bindings/{id}/unlink", s.handleUnlinkIdentityBinding)
	s.peers.Mount(s.mux)
	s.mux.HandleFunc("GET /server", s.handleGetServer)
	s.mux.HandleFunc("GET /restart-readiness", s.handleRestartReadiness)
	s.mux.HandleFunc("POST /restart-drain", s.handleStartRestartDrain)
	s.mux.HandleFunc("DELETE /restart-drain", s.handleEndRestartDrain)
	s.mux.HandleFunc(issuesMountPrefix+"/", s.handleIssuesAsCaller)
	s.mux.HandleFunc(mailMountPrefix+"/", s.handleMailstackAsCaller)
	s.mux.HandleFunc(mailstackIngressDeliveryPattern, s.handleMailstackIngressDelivery)
}
