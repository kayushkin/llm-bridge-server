package modelcredentials

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the credential routes (auth-store) and, when model-store
// is loaded, GET /models on the server's mux.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /credentials", s.HandleCredentialsList)
	mux.HandleFunc("POST /credentials", s.HandleCredentialCreate)
	mux.HandleFunc("DELETE /credentials/{id}", s.HandleCredentialDelete)
	if s.ModelStore() != nil {
		mux.HandleFunc("GET /models", s.HandleModels)
	}
}
