package requestcaller

import "github.com/kayushkin/llm-bridge-server/internal/store"

// SessionLineage is the bridge store as a session scope reads it: a session,
// and the connected server its lineage was started through.
type SessionLineage interface {
	GetSession(sessionID string) (*store.Session, error)
	SessionPeerOriginOfLineage(sessionID string) (store.SessionPeerOrigin, bool, error)
}

// Reaches reports whether a session is one the scope reaches.
func (scope SessionScope) Reaches(lineage SessionLineage, session *store.Session) bool {
	if session == nil || scope.PrincipalID == "" || session.PrincipalID != scope.PrincipalID {
		return false
	}
	if scope.ThroughPeerPrincipalID == "" {
		return true
	}
	origin, found, err := lineage.SessionPeerOriginOfLineage(session.SessionID)
	return err == nil && found && origin.PeerPrincipalID == scope.ThroughPeerPrincipalID
}

// ReachesSessionID is Reaches for a session id.
func (scope SessionScope) ReachesSessionID(lineage SessionLineage, sessionID string) bool {
	session, err := lineage.GetSession(sessionID)
	return err == nil && scope.Reaches(lineage, session)
}
