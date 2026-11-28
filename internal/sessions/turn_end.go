package sessions

import "github.com/kayushkin/llm-bridge/msg"

// OnTurnEnd is the Manager's turn-end observer. The manager already runs it
// on its own goroutine; the renamer gets one more so a slow title call never
// holds up the classifier, whose verdict changes the session's state.
func (s *Service) OnTurnEnd(bridgeID string, ev *msg.Event, state msg.SessionState) {
	go s.maybeAutoRename(bridgeID)
	go s.Peers.ReportPeerRequestTurnEnd(bridgeID, ev, state)
	go s.ChatLinkService.OnTurnEnd(bridgeID, ev, state)
	go s.BriefService.UpdateSessionBriefAtTurnEnd(bridgeID, state)
	s.Signals.ClassifyTurnEnd(bridgeID, ev, state)
}
