// Package requestcaller holds who an authorized request acts as. Request
// authorization puts the caller on the request's context, and every handler
// that narrows its answer reads it from there.
package requestcaller

import (
	"context"
	"net/http"
)

// requestCallerContextKey carries who an authorized request acts as.
type requestCallerContextKey struct{}

// Peer is what a peer token says about the server presenting it: the
// grant it was issued by, the connection that grant is on, its level, and the
// peer principal it acts as. When the request also names one of the calling
// server's users (peerUserHeader), remoteUserPrincipalID is that user's id
// there and mappingID the mapping that made it one of this server's
// principals; the request's principalID is then the mapped local principal,
// and peerPrincipalID still limits what it may do.
type Peer struct {
	GrantID         string
	ConnectionID    string
	Level           string
	PeerPrincipalID string
	// PeerIsAdministrator is principal-store's answer for the peer principal.
	PeerIsAdministrator   bool
	RemoteUserPrincipalID string
	MappingID             string
}

// ActsForRemoteUser reports whether a peer request names one of the calling
// server's users and so acts as a mapped local principal.
func (peer *Peer) ActsForRemoteUser() bool {
	return peer != nil && peer.RemoteUserPrincipalID != ""
}

// Caller is who an authorized request acts as, as request authorization
// established it.
type Caller struct {
	// PrincipalID is the principal the request acts as, and empty for the
	// internal service acting as itself.
	PrincipalID string
	// IsAdministrator is principal-store's own answer for principalID. An
	// administrator is past every per-resource check: every session whoever
	// owns it, every operator route, every list unfiltered, both store
	// proxies.
	IsAdministrator bool
	// LoginSessionID is the login session whose cookie authenticated the
	// request, and empty for every other credential.
	LoginSessionID string
	// Peer is set when another llm-bridge-server presented a peer token;
	// principalID is then the peer principal the token acts as.
	Peer *Peer
	// PostingAgentOfSessionID is set, and nothing else is, when the caller
	// is a session's own agent presenting its session posting token. It reaches
	// only routeSessionOwnerOrItsAgentPostsIntoSession, for this session.
	PostingAgentOfSessionID string
	// DelegatingAgentOfSessionID is set when the caller is a session's agent
	// presenting its posting token on a route that lets it start or drive its
	// delegates. principalID is then the principal that session runs with,
	// empty when it runs with none.
	DelegatingAgentOfSessionID string
	// ObservesEverySession is set on a read-only route that narrows to one
	// principal's sessions when the caller's can_observe_sessions_on grants
	// name every instance: SessionScopeRestrictingRequest then narrows
	// nothing.
	ObservesEverySession bool
}

// SessionScopeOfCaller is the sessions a restricted caller reaches by
// ownership (peer_users.go).
func SessionScopeOfCaller(caller Caller) SessionScope {
	scope := SessionScope{PrincipalID: caller.PrincipalID}
	if caller.Peer.ActsForRemoteUser() {
		scope.ThroughPeerPrincipalID = caller.Peer.PeerPrincipalID
	}
	return scope
}

// NarrowsToOnePrincipal reports whether this caller's answers must be limited
// to one principal's own records. The internal service and an administrator
// are identified but not narrowed.
func (caller Caller) NarrowsToOnePrincipal() bool {
	return caller.PrincipalID != "" && !caller.IsAdministrator
}

func FromRequest(r *http.Request) (Caller, bool) {
	caller, ok := r.Context().Value(requestCallerContextKey{}).(Caller)
	return caller, ok
}

// PrincipalRestrictingRequest returns the principal a request is restricted
// to, and "" with false when it is not restricted: the caller is the internal
// service acting as itself, or an administrator. Handlers that narrow their
// answer to one principal's sessions read it here and nowhere else.
//
// The id is empty whenever the bool is false, so a caller that reads only the
// id — a store filter where "" means every owner — cannot narrow an
// administrator to the sessions that happen to carry their own id.
// PrincipalIdentityOfRequest is the one that names an administrator.
func PrincipalRestrictingRequest(r *http.Request) (string, bool) {
	caller, ok := FromRequest(r)
	if !ok || !caller.NarrowsToOnePrincipal() {
		return "", false
	}
	return caller.PrincipalID, true
}

// PrincipalIdentityOfRequest returns who the request acts as, whether or not
// that narrows what it may reach — an administrator is identified here and
// restricted nowhere. The store proxies set X-Principal-Id from this, because
// kanban-store and grant-store make their own administrator check.
func PrincipalIdentityOfRequest(r *http.Request) (string, bool) {
	caller, ok := FromRequest(r)
	return caller.PrincipalID, ok && caller.PrincipalID != ""
}

func WithCaller(r *http.Request, caller Caller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestCallerContextKey{}, caller))
}

// SessionScope is which sessions a restricted caller reaches: those started
// as principalID, and, for a connected server's user, only those started
// through that server's peer principal.
type SessionScope struct {
	PrincipalID            string
	ThroughPeerPrincipalID string
}

// SessionScopeRestrictingRequest is PrincipalRestrictingRequest for the
// session routes: the scope a request is narrowed to, and false when it is
// not narrowed — the internal service, an administrator, or a caller whose
// grants let it observe every session on a read-only route.
func SessionScopeRestrictingRequest(r *http.Request) (SessionScope, bool) {
	caller, ok := FromRequest(r)
	if !ok || !caller.NarrowsToOnePrincipal() || caller.ObservesEverySession {
		return SessionScope{}, false
	}
	scope := SessionScope{PrincipalID: caller.PrincipalID}
	if caller.Peer.ActsForRemoteUser() {
		scope.ThroughPeerPrincipalID = caller.Peer.PeerPrincipalID
	}
	return scope, true
}

// SessionScopeFilterOfRequest is the two store filter fields a restricted
// request sets, both empty when it is not narrowed.
func SessionScopeFilterOfRequest(r *http.Request) (ownedByPrincipalID, ownedThroughPeerPrincipalID string) {
	scope, restricted := SessionScopeRestrictingRequest(r)
	if !restricted {
		return "", ""
	}
	return scope.PrincipalID, scope.ThroughPeerPrincipalID
}
