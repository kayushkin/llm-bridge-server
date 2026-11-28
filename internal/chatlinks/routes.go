package chatlinks

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the chat link routes on the server's mux.
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("POST /sessions/{id}/chat-links", s.HandleCreateChatLink)
	mux.HandleFunc("GET /sessions/{id}/chat-links", s.HandleListChatLinks)
	mux.HandleFunc("GET /sessions/{id}/chat-links/{link_id}", s.HandleGetChatLink)
	mux.HandleFunc("POST /sessions/{id}/chat-links/{link_id}/end", s.HandleEndChatLink)
}
