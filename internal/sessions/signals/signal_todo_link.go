package signals

import (
	"context"
	"log"
)

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
func (s *Service) linkedTodoForSession(sessionID string) string {
	if s.IssueReads() == nil || sessionID == "" {
		return ""
	}
	todoID, err := s.IssueReads().LinkedTodoForSession(context.Background(), sessionID)
	if err != nil {
		log.Printf("[signals] %s: resolve linked todo: %v", sessionID, err)
		return ""
	}
	return todoID
}
