package login

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the sign-in routes on the server's mux: demo and Entra sign-in, the signed-in principal, logout and login sessions, and service principal tokens.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("POST /auth/demo-login", s.HandleDemoLogin)
	mux.HandleFunc("GET /auth/entra/start", s.HandleEntraSignInStart)
	mux.HandleFunc("GET /auth/entra/callback", s.HandleEntraSignInCallback)
	mux.HandleFunc("GET /auth/principal", s.HandleGetLoggedInPrincipal)
	mux.HandleFunc("POST /auth/logout", s.HandleLogout)
	mux.HandleFunc("GET /auth/sessions", s.HandleListLoginSessions)
	mux.HandleFunc("POST /auth/sessions/{id}/revoke", s.HandleRevokeLoginSession)
	mux.HandleFunc("GET /auth/login-attempts", s.HandleListLoginAttempts)
	mux.HandleFunc("POST /service-principal-tokens", s.HandleIssueServicePrincipalToken)
	mux.HandleFunc("GET /service-principal-tokens", s.HandleListServicePrincipalTokens)
	mux.HandleFunc("GET /self", s.HandleSelf)
	mux.HandleFunc("POST /service-principal-tokens/{id}/revoke", s.HandleRevokeServicePrincipalToken)
	mux.HandleFunc("PUT /sessions/{id}/starter", s.HandleRecordSessionStarter)
}
