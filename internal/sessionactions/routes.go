package sessionactions

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the session action routes on the server's mux: the chat's buttons.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("POST /sessions/{id}/actions", s.HandleOfferSessionAction)
	mux.HandleFunc("GET /sessions/{id}/actions", s.HandleListSessionActions)
	mux.HandleFunc("POST /sessions/{id}/actions/{action_id}/run", s.HandleRunSessionAction)
}
