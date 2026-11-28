package signals

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the signal routes on the server's mux: questions and decisions a session raises, and their answers.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /sessions/{id}/signals", s.HandleListSessionSignals)
	mux.HandleFunc("POST /sessions/{id}/signals", s.HandleCreateSessionSignal)
	mux.HandleFunc("GET /signals", s.HandleListSignals)
	mux.HandleFunc("POST /signals/{id}/resolve", s.HandleResolveSignal)
	mux.HandleFunc("POST /signals/{id}/answer", s.HandleAnswerSignal)
}
