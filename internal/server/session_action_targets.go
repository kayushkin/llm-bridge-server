package server

// The session actions that act on another session: send_to_session sends it a
// message and stop_session stops it. They exist for an agent that manages
// other sessions from its own, such as producer's orchestrator, so the person
// can approve the next step in one of them with a press.
//
// The target is named by its id and checked at offer and again at confirm: it
// must exist, must not be the session the button is in, and must be one the
// button's session's principal could reach through the routes: its own, or
// any session when principal-store says that principal is an administrator.
// So a button can reach no session its own session's owner could not. Its display name is in Command for the
// person to read, never used to find it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionActionTypeActsOnAnotherSession says whether an action type names a
// target session.
func sessionActionTypeActsOnAnotherSession(actionType msg.SessionActionType) bool {
	return actionType == msg.SessionActionSendToSession || actionType == msg.SessionActionStopSession
}

// targetSessionOfAction reads the session an offer names and holds it to the
// rules above.
func (s *Server) targetSessionOfAction(ctx context.Context, session *store.Session, offer msg.SessionActionOffer) (*store.Session, *sessionActionRefusal) {
	if offer.TargetSessionID == session.SessionID {
		return nil, &sessionActionRefusal{http.StatusBadRequest, "target_is_this_session", fmt.Sprintf(
			"target_session_id %s is the session the button is in; a %s button acts on another session", offer.TargetSessionID, offer.Type)}
	}
	target, err := s.store.GetSession(offer.TargetSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &sessionActionRefusal{http.StatusNotFound, "unknown_target_session", fmt.Sprintf("there is no session %s", offer.TargetSessionID)}
	}
	if err != nil {
		return nil, &sessionActionRefusal{http.StatusInternalServerError, "read_target_session", fmt.Sprintf("session %s could not be read: %v", offer.TargetSessionID, err)}
	}
	reachable, refusal := s.sessionPrincipalMayReachTarget(ctx, session, target)
	if refusal != nil {
		return nil, refusal
	}
	if !reachable {
		// The same answer as a missing session, as the routes give a principal
		// who reaches someone else's.
		return nil, &sessionActionRefusal{http.StatusNotFound, "unknown_target_session", fmt.Sprintf(
			"there is no session %s that %s, the principal of session %s, may reach", offer.TargetSessionID, orNone(session.PrincipalID), session.SessionID)}
	}
	if offer.Type == msg.SessionActionSendToSession && target.ControlledBy == msg.ControlledByHarness {
		return nil, &sessionActionRefusal{http.StatusConflict, "target_not_bridge_controlled", fmt.Sprintf(
			"session %s is a subagent its parent harness process controls, so it cannot be sent a message; send it to the session that started it, %s", target.SessionID, orNone(target.ParentID))}
	}
	return target, nil
}

// sessionPrincipalMayReachTarget applies the routes' rule to the principal
// of the session the button is in: a session reaches a target with its own
// principal, and an administrator's session reaches any target. Whether a
// principal is an administrator is principal-store's answer, read through
// the same cache the request gate uses.
//
// A session a connected server started for one of its users reaches only the
// sessions in that user's scope — its principal's, started through the same
// peer (peer_users.go) — unless both its principal and the peer are
// administrators, as the request gate rules.
func (s *Server) sessionPrincipalMayReachTarget(ctx context.Context, session, target *store.Session) (bool, *sessionActionRefusal) {
	origin, throughPeer, err := s.store.SessionPeerOriginOfLineage(session.SessionID)
	if err != nil {
		return false, &sessionActionRefusal{http.StatusInternalServerError, "session_peer_origin_lookup_failed", err.Error()}
	}
	if !throughPeer && target.PrincipalID == session.PrincipalID {
		return true, nil
	}
	if throughPeer && s.sessionIsInScope(target, sessionScope{principalID: session.PrincipalID, throughPeerPrincipalID: origin.PeerPrincipalID}) {
		return true, nil
	}
	if session.PrincipalID == "" {
		return false, nil
	}
	owner, credentialError := s.callerForPrincipalID(ctx, session.PrincipalID, "session "+session.SessionID+" belongs to")
	if credentialError != nil {
		return false, &sessionActionRefusal{credentialError.status, credentialError.code, credentialError.message}
	}
	if !throughPeer {
		return owner.isAdministrator, nil
	}
	peer, credentialError := s.callerForPrincipalID(ctx, origin.PeerPrincipalID, "session "+session.SessionID+" was started through")
	if credentialError != nil {
		return false, &sessionActionRefusal{credentialError.status, credentialError.code, credentialError.message}
	}
	return owner.isAdministrator && peer.isAdministrator, nil
}

// describeTargetSessionAction writes Command for a send_to_session or
// stop_session: the target's id and name, and the message.
func (s *Server) describeTargetSessionAction(ctx context.Context, session *store.Session, offer msg.SessionActionOffer) (string, *sessionActionRefusal) {
	target, refusal := s.targetSessionOfAction(ctx, session, offer)
	if refusal != nil {
		return "", refusal
	}
	if offer.Type == msg.SessionActionStopSession {
		return fmt.Sprintf("stop session %s (%q, now %s): kill its harness process and mark it aborted", target.SessionID, target.DisplayName, target.State), nil
	}
	return fmt.Sprintf("send session %s (%q) this message, as if the person had typed it there:\n%s%s",
		target.SessionID, target.DisplayName, offer.Message, describeSessionActionMessageInputs(offer.Inputs)), nil
}

// sendToSessionAction sends the message, with the typed values in place,
// through POST /sessions/{id}/send, so it reaches the target as a person's
// message would: over-budget refusal, harness start, archive reopening and all.
func (s *Server) sendToSessionAction(offer msg.SessionActionOffer, inputValues map[string]string) sessionActionOutcome {
	message := fillSessionActionMessageInputs(offer.Message, inputValues)
	output, err := s.sendMessageInternally(offer.TargetSessionID, message)
	if err != nil {
		return sessionActionOutcome{output: output, resultSessionID: offer.TargetSessionID, err: err}
	}
	return sessionActionOutcome{output: s.describeTargetSessionStateAfter("Sent the message to", offer.TargetSessionID), resultSessionID: offer.TargetSessionID}
}

// stopSessionAction stops the target the way POST /sessions/{id}/stop does.
func (s *Server) stopSessionAction(offer msg.SessionActionOffer) sessionActionOutcome {
	if err := s.stopSession(offer.TargetSessionID); err != nil {
		return sessionActionOutcome{resultSessionID: offer.TargetSessionID, err: fmt.Errorf("stop of %s failed: %w", offer.TargetSessionID, err)}
	}
	return sessionActionOutcome{output: s.describeTargetSessionStateAfter("Stopped", offer.TargetSessionID), resultSessionID: offer.TargetSessionID}
}

// describeTargetSessionStateAfter is the output of a run that worked: what it
// did and the state the target is in now, read back from the store.
func (s *Server) describeTargetSessionStateAfter(what, targetSessionID string) string {
	target, err := s.store.GetSession(targetSessionID)
	if err != nil {
		return fmt.Sprintf("%s session %s, whose state could not be read back: %v", what, targetSessionID, err)
	}
	return fmt.Sprintf("%s session %s (%q); its state is now %s", what, target.SessionID, target.DisplayName, target.State)
}
