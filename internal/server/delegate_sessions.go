package server

// Delegates: sessions a session's own agent starts under itself and drives,
// as bridge-agent does, with nothing but the session posting token every
// local and runner harness child already holds.
//
// Until 2026-10-27 an agent did this by signing in through POST
// /auth/demo-login as whichever principal it named, which also let it act as
// anyone. With the posting token the agent proves only which session it runs
// in, and acts with exactly what that session has:
//
//   - POST /sessions creates a session whose manager_session_id is the agent's
//     session and whose principal is that session's principal. A body naming
//     another manager or another principal is refused, never rewritten.
//   - GET /sessions/{id}, POST …/send, POST …/stop and GET …/events reach a
//     session only when the agent's session manages it (GET also the agent's
//     own session); anything else is 404, the answer for a missing session.
//   - GET /instances, which a start needs, reads as the session's principal.
//
// The routes that take this are marked by sessionAgentAccess on their rule in
// routeAccessRules. Every other route refuses the token as before.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// serveDelegatingAgent authorizes a request carrying a session posting token
// on a route whose rule names sessionAgentAccess, and dispatches it.
func (s *Server) serveDelegatingAgent(w http.ResponseWriter, r *http.Request, pattern string, rule routeAccessRule) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		writeJSONError(w, http.StatusUnauthorized, "invalid_session_posting_token",
			"the Authorization header must be \"Bearer <session posting token>\"")
		return
	}
	agentSession, credentialError := s.verifiedPostingSession(token)
	if credentialError != nil {
		credentialError.write(w)
		return
	}
	agentSessionID := agentSession.SessionID
	// An agent whose session has ended has no business driving anything: a
	// token left in its transcript must not start, send to, stop or read its
	// delegates.
	if slices.Contains(sessionStatesThatEndSessionAgentTokens, msg.SessionState(agentSession.State)) {
		writeJSONError(w, http.StatusForbidden, "delegating_session_ended", fmt.Sprintf(
			"session %s is %s, so its agent may not start or drive delegates", agentSessionID, agentSession.State))
		return
	}

	caller := requestCaller{}
	if agentSession.PrincipalID != "" {
		principalCaller, credentialError := s.callerForPrincipalID(r.Context(), agentSession.PrincipalID, "the session the posting token names runs as")
		if credentialError != nil {
			credentialError.write(w)
			return
		}
		caller = principalCaller
		// A session a connected server started for one of its users acts
		// within the peer's grants as well as its principal's, so its agent is
		// an administrator here only when both are: a peer user mapped to a
		// local administrator through a peer that is not one gets no
		// administrator's reach through its agent's token.
		if caller.isAdministrator {
			origin, throughPeer, err := s.store.SessionPeerOriginOfLineage(agentSessionID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "session_peer_origin_unreadable", err.Error())
				return
			}
			if throughPeer {
				peer, credentialError := s.callerForPrincipalID(r.Context(), origin.PeerPrincipalID, "the peer session "+agentSessionID+" was started through is")
				if credentialError != nil {
					credentialError.write(w)
					return
				}
				caller.isAdministrator = peer.isAdministrator
			}
		}
	}
	caller.delegatingAgentOfSessionID = agentSessionID

	switch rule.sessionAgentAccess {
	case sessionAgentCreatesItsDelegate:
	case sessionAgentReachesItsDelegate, sessionAgentReachesItsDelegateOrItself:
		targetSessionID, err := pathValueForPattern(pattern, r.URL.EscapedPath(), rule.ownedResourcePathValueName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", err.Error())
			return
		}
		ownSession := rule.sessionAgentAccess == sessionAgentReachesItsDelegateOrItself && targetSessionID == agentSessionID
		if !ownSession && !s.sessionIsDelegateOf(targetSessionID, agentSessionID) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	case sessionAgentReadsCatalog:
	default:
		writeJSONError(w, http.StatusInternalServerError, "route_rule_invalid", fmt.Sprintf(
			"route %q lets a session's agent in with a kind of access this server does not handle", pattern))
		return
	}
	s.mux.ServeHTTP(w, withRequestCaller(r, caller))
}

// sessionIsDelegateOf reports whether the session exists and managerSessionID
// started it under itself.
func (s *Server) sessionIsDelegateOf(sessionID, managerSessionID string) bool {
	session, err := s.store.GetSession(sessionID)
	return err == nil && session.ManagerSessionID == managerSessionID
}

// delegateRequestRefusal holds a create request from a session's agent to its
// own session: the delegate sits under that session and runs as its
// principal. It fills in either when the request leaves it empty, and refuses
// a request that names another, so a caller that asked for more is told so.
func delegateRequestRefusal(req *CreateSessionRequest, agentSessionID, agentPrincipalID string) *requestCredentialError {
	if req.ManagerSessionID != "" && req.ManagerSessionID != agentSessionID {
		return &requestCredentialError{http.StatusForbidden, "delegate_manager_mismatch", fmt.Sprintf(
			"manager_session_id %q is not %s, the session this posting token belongs to; an agent starts delegates only under its own session (omit manager_session_id)",
			req.ManagerSessionID, agentSessionID)}
	}
	if req.PrincipalID != "" && req.PrincipalID != agentPrincipalID {
		own := "no principal"
		if agentPrincipalID != "" {
			own = "principal " + agentPrincipalID
		}
		return &requestCredentialError{http.StatusForbidden, "delegate_principal_mismatch", fmt.Sprintf(
			"principal_id %q is not the principal of session %s, which runs with %s; a delegate runs with its session's principal and no other (omit principal_id)",
			req.PrincipalID, agentSessionID, own)}
	}
	req.ManagerSessionID = agentSessionID
	req.PrincipalID = agentPrincipalID
	return nil
}
