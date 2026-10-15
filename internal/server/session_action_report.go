package server

// When a person's press of a button ends, the agent that offered it is told:
// the server sends its session a message with the button's label, how the run
// ended, its output and its error. Until 2026-10-15 the agent learned the
// outcome only when the person came back and said so ("it saved, I shouldn't
// have to tell you that").
//
// The report carries the recorded outcome, so a secret typed into the button
// is already blanked in it, and the typed values themselves are never in it.
// A deploy is not reported here: deploy-gate already sends the session that
// started a deploy its outcome, and a second report would only repeat it.

import (
	"fmt"
	"log"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionActionReportsToItsSession says whether a finished action of this type
// is reported to the session that offered it.
func sessionActionReportsToItsSession(actionType msg.SessionActionType) bool {
	return actionType != msg.SessionActionDeploy
}

// sessionActionReport is the message the agent receives when its button's run ends.
func sessionActionReport(action store.SessionAction) string {
	var report strings.Builder
	fmt.Fprintf(&report, "[session action] The person pressed your button %s, %q, and it %s.",
		action.ActionID, action.Offer.Label, describeSessionActionEnd(action.State))
	if action.ResultSessionID != "" {
		fmt.Fprintf(&report, " It worked in session %s.", action.ResultSessionID)
	}
	if action.Error != "" {
		fmt.Fprintf(&report, "\n\nError: %s", action.Error)
	}
	if action.Output != "" {
		fmt.Fprintf(&report, "\n\nOutput:\n%s", action.Output)
	}
	report.WriteString("\n\nThis is a report, not a new instruction: the person's last request still stands. Carry on with it, and tell them what this outcome means.")
	return report.String()
}

func describeSessionActionEnd(state msg.SessionActionState) string {
	switch state {
	case msg.SessionActionSucceeded:
		return "succeeded"
	case msg.SessionActionFailed:
		return "failed"
	case msg.SessionActionOutcomeUnknown:
		return "ended with its outcome unknown"
	}
	return string(state)
}

// reportSessionActionToItsSession sends the report. A failure is logged, not
// raised: the action has ended and is recorded either way.
func (s *Server) reportSessionActionToItsSession(action store.SessionAction) {
	if !sessionActionReportsToItsSession(action.Offer.Type) {
		return
	}
	if _, err := s.sendMessageInternally(action.SessionID, sessionActionReport(action)); err != nil {
		log.Printf("[session-actions] ERROR %s ended %s but its session %s was not told: %v", action.ActionID, action.State, action.SessionID, err)
	}
}
