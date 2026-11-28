package userinterface

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the built-in web interface on the server's mux: /ui/ and the redirect from /.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("GET /ui/", s.HandleUserInterface)
	mux.HandleFunc("GET /{$}", s.HandleRootRedirectsToUserInterface)
}
