package briefs

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the session brief routes on the server's mux: what each
// chat is for, where it stands, what each turn changed, and what it said that
// nobody answered.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /sessions/{id}/brief", s.HandleGetSessionBrief)
	mux.HandleFunc("GET /session-briefs", s.HandleListSessionBriefs)
	mux.HandleFunc("POST /sessions/{id}/loose-ends/{loose_end_id}/answer", s.HandleAnswerLooseEnd)
	mux.HandleFunc("POST /sessions/{id}/loose-ends/{loose_end_id}/dismiss", s.HandleDismissLooseEnd)
}
