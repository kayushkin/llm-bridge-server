package server

import "github.com/kayushkin/llm-bridge/msg"

// onTurnEnd is the Manager's turn-end observer. The manager already runs it
// on its own goroutine; the renamer gets one more so a slow title call never
// holds up the classifier, whose verdict changes the session's state.
func (s *Server) onTurnEnd(bridgeID string, ev *msg.Event, state msg.SessionState) {
	go s.maybeAutoRename(bridgeID)
	s.classifyTurnEnd(bridgeID, ev, state)
}
