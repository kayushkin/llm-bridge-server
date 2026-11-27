package sessionfiles

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the session file routes on the server's mux: files shared into a session, and their bytes.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("POST /sessions/{id}/files", s.HandleShareSessionFile)
	mux.HandleFunc("GET /sessions/{id}/files", s.HandleListSessionFiles)
	mux.HandleFunc("GET /sessions/{id}/files/{file_id}/content", s.HandleSessionFileContent)
}
