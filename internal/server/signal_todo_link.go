package server

import (
	"context"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
)

// newKanbanClient builds the kanban-store client, or returns nil when no URL
// is configured. Nil means the link lookup is off: every signal is minted
// with an empty LinkedTodoID, which is the honest answer when the service
// that owns the link cannot be asked.
func newKanbanClient(baseURL string) *kanbanclient.Client {
	if baseURL == "" {
		return nil
	}
	return kanbanclient.New(baseURL)
}

// linkedTodoForSession resolves the noteboard todo this session is linked to,
// for stamping onto a signal at mint time. Returns "" when there is no link,
// no configured kanban-store, or the lookup fails.
//
// Resolved at mint time rather than joined at read time, because the record
// is the source of truth for what was true when the signal was raised, and
// because the inbox query would otherwise call kanban-store once per row.
//
// The consequence, stated plainly: a session that acquires its first link
// *after* raising a signal leaves that signal unlinked forever. That is the
// autoworker case working and the chat case mostly not — the autoworker links
// its dispatch todo before the session starts, while the kanban classifier
// links a chat session's work up to fifteen minutes later. An empty
// LinkedTodoID is the truth about that moment; back-filling one later would
// claim a link the signal was not raised under.
//
// Failure is not fatal. A signal is a surface on top of work that already
// happened, and losing the todo pointer must never cost the signal itself.
func (s *Server) linkedTodoForSession(sessionID string) string {
	if s.kanbanClient == nil || sessionID == "" {
		return ""
	}
	todoID, err := s.kanbanClient.LinkedTodoForSession(context.Background(), sessionID)
	if err != nil {
		log.Printf("[signals] %s: resolve linked todo: %v", sessionID, err)
		return ""
	}
	return todoID
}
