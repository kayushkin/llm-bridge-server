package server

// registerLoginAndStoreProxyRoutes mounts the login routes, the login session
// and identity binding administration routes, and the kanban-store and
// grant-store proxies. They are always mounted: a switched-off sign-in method
// answers 404 from its handler, so the route table does not depend on
// settings.
func (s *Server) registerLoginAndStoreProxyRoutes() {
	s.login.Mount(s.mux)
	s.mux.HandleFunc("GET /admin/identity-bindings", s.handleListIdentityBindings)
	s.mux.HandleFunc("POST /admin/identity-bindings", s.handleLinkIdentityBinding)
	s.mux.HandleFunc("POST /admin/identity-bindings/{id}/unlink", s.handleUnlinkIdentityBinding)
	s.peers.Mount(s.mux)
	s.mux.HandleFunc("GET /server", s.handleGetServer)
	s.mux.HandleFunc("GET /restart-readiness", s.sessions.HandleRestartReadiness)
	s.mux.HandleFunc("POST /restart-drain", s.sessions.HandleStartRestartDrain)
	s.mux.HandleFunc("DELETE /restart-drain", s.sessions.HandleEndRestartDrain)
	s.mux.HandleFunc(issuesMountPrefix+"/", s.handleIssuesAsCaller)
	s.mux.HandleFunc(mailMountPrefix+"/", s.handleMailstackAsCaller)
	s.mux.HandleFunc(mailstackIngressDeliveryPattern, s.handleMailstackIngressDelivery)
}
