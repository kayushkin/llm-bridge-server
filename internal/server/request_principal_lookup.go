package server

// Reading the caller's principal from principal-store.
//
// Request authorization establishes *which* principal a request acts as — from
// a login cookie, a session agent token, or the internal service asserting one
// — and then asks principal-store what that principal is: active or disabled,
// and an administrator or not. Neither answer is derived here and neither is
// guessed. A principal-store that cannot be asked is a 502; it is never read as
// "assume not an administrator" (which would lock an administrator out of their
// own product) and never as "assume administrator" (which needs no explaining).
//
// The answer is cached for principalLookupCacheLifetime, because one page load
// of the chat is dozens of requests and each would otherwise be a round trip.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// principalLookupCacheLifetime is how long principal-store's answer about one
// principal is reused.
//
// It is the delay on a change made in principal-store. Disable a person, or
// take their administrator flag away, and this server keeps letting them do
// what they could for up to 30 seconds more — until the cached answer expires
// and the next request re-reads it. Nothing else extends that: the entry is
// not refreshed on use, so the clock starts at the read, not at the last
// request. 30 seconds is short enough to be a wait rather than a hole, and
// long enough that a page load asks principal-store once per principal.
const principalLookupCacheLifetime = 30 * time.Second

// cachedPrincipal is one principal-store answer and the moment it was read.
type cachedPrincipal struct {
	principal principalclient.Principal
	readAt    time.Time
}

// principalLookupCache holds principal-store's answers for a short while.
// Only answers are cached: a refusal and an unreachable store are re-asked
// every time, so a principal created a second ago is not denied for 30
// seconds and a store that comes back up is used at once.
type principalLookupCache struct {
	mutex            sync.Mutex
	principalsByID   map[string]cachedPrincipal
	lifetime         time.Duration
	now              func() time.Time
	readFromUpstream func(ctx context.Context, principalID string) (principalclient.Principal, error)
}

func newPrincipalLookupCache(client *principalclient.Client) *principalLookupCache {
	return &principalLookupCache{
		principalsByID:   map[string]cachedPrincipal{},
		lifetime:         principalLookupCacheLifetime,
		now:              time.Now,
		readFromUpstream: client.Get,
	}
}

// lookUp returns principal-store's record for principalID, from the cache when
// it was read less than lifetime ago and from principal-store otherwise.
func (cache *principalLookupCache) lookUp(ctx context.Context, principalID string) (principalclient.Principal, error) {
	cache.mutex.Lock()
	cached, known := cache.principalsByID[principalID]
	fresh := known && cache.now().Sub(cached.readAt) < cache.lifetime
	cache.mutex.Unlock()
	if fresh {
		return cached.principal, nil
	}

	principal, err := cache.readFromUpstream(ctx, principalID)
	if err != nil {
		return principalclient.Principal{}, err
	}
	cache.mutex.Lock()
	cache.principalsByID[principalID] = cachedPrincipal{principal: principal, readAt: cache.now()}
	cache.mutex.Unlock()
	return principal, nil
}

// callerActingAsPrincipal turns a principal id this server has already
// authenticated — a login cookie or a session agent token — into the caller
// the route rules are applied to.
func (s *Server) callerActingAsPrincipal(ctx context.Context, principalID string) (requestCaller, *requestCredentialError) {
	return s.callerForPrincipalID(ctx, principalID, "the login names")
}

// callerAssertedByInternalService turns the principal id a caller presenting
// the service token asserted in X-Principal-Id into the caller the route rules
// are applied to. dash holds that token and is the browser's front door on this
// host, so it may say which of its logged-in users a request is for — but the
// principal is still checked here, not taken on trust.
func (s *Server) callerAssertedByInternalService(ctx context.Context, principalID string) (requestCaller, *requestCredentialError) {
	if !isWellFormedPrincipalID(principalID) {
		return requestCaller{}, &requestCredentialError{http.StatusBadRequest, "invalid_principal_id", fmt.Sprintf(
			"%s: %q is not a principal-store id (principal_000001); a name or an email is not accepted, because neither is unique", principalIdentityHeader, principalID)}
	}
	return s.callerForPrincipalID(ctx, principalID, principalIdentityHeader+" names")
}

// callerForPrincipalID reads principalID from principal-store and turns the
// answer into a caller or the refusal it earns. subjectDescription says where
// the id came from, so a refusal names the credential that carried it.
func (s *Server) callerForPrincipalID(ctx context.Context, principalID, subjectDescription string) (requestCaller, *requestCredentialError) {
	// New always builds the cache, right beside the cookie codec, so there is
	// no server that gates a request and cannot read the caller's principal.
	principal, err := s.principalLookupCache.lookUp(ctx, principalID)
	if err != nil {
		if errors.Is(err, principalclient.ErrNotFound) {
			return requestCaller{}, &requestCredentialError{http.StatusBadRequest, "unknown_principal", fmt.Sprintf(
				"%s %s, which principal-store does not have: %v", subjectDescription, principalID, err)}
		}
		return requestCaller{}, &requestCredentialError{http.StatusBadGateway, "principal_store_unavailable", fmt.Sprintf(
			"could not read principal %s from principal-store, so this request is refused rather than answered as a principal nobody confirmed: %v", principalID, err)}
	}
	if !principal.ActsAsCaller {
		return requestCaller{}, &requestCredentialError{http.StatusBadRequest, "principal_does_not_act", fmt.Sprintf(
			"%s %s, which is a %q principal, and principal-store says that kind does not act as a caller: a group is a set of principals and a contact holds nothing", subjectDescription, principalID, principal.Kind)}
	}
	// Disabled is decided before is_administrator is looked at: a departed
	// administrator is a departed person first.
	if principal.DisabledAt != 0 {
		return requestCaller{}, &requestCredentialError{http.StatusForbidden, "principal_disabled", fmt.Sprintf(
			"principal %s was disabled in principal-store at %s", principalID, time.Unix(principal.DisabledAt, 0).UTC().Format(time.RFC3339))}
	}
	return requestCaller{principalID: principal.ID, isAdministrator: principal.IsAdministrator}, nil
}

// callerOfPeerToken turns a peer token another server presented into the
// caller the route rules are applied to: the peer principal the grant was
// made for, read from principal-store like any other principal. A token this
// server never issued, or one an administrator revoked, is 401, and so is a
// token presented over the socket of a connection it was not granted on. The
// token's value is never logged or echoed; only its grant's id is named.
func (s *Server) callerOfPeerToken(ctx context.Context, presentedToken, socketConnectionID string, arrivedOverPeerSocket bool) (requestCaller, *requestCredentialError) {
	grant, err := s.store.GetPeerGrantByTokenHash(hashPeerSecret(presentedToken))
	if errors.Is(err, store.ErrPeerGrantNotFound) {
		return requestCaller{}, &requestCredentialError{http.StatusUnauthorized, "peer_token_unknown", peerTokenHeader + " is not a peer token this server issued"}
	}
	if err != nil {
		return requestCaller{}, &requestCredentialError{http.StatusInternalServerError, "peer_token_lookup_failed", fmt.Sprintf("could not look up the peer token: %v", err)}
	}
	if grant.RevokedAt != nil {
		return requestCaller{}, &requestCredentialError{http.StatusUnauthorized, "peer_token_revoked", fmt.Sprintf(
			"peer grant %s was revoked at %s: %s", grant.ID, grant.RevokedAt.UTC().Format(time.RFC3339), grant.RevokeReason)}
	}
	if arrivedOverPeerSocket && grant.ConnectionID != socketConnectionID {
		return requestCaller{}, &requestCredentialError{http.StatusUnauthorized, "peer_token_on_another_connection", fmt.Sprintf(
			"peer grant %s is on %s, and this request came over %s's socket", grant.ID, grant.ConnectionID, socketConnectionID)}
	}
	caller, credentialError := s.callerForPrincipalID(ctx, grant.PeerPrincipalID, "peer grant "+grant.ID+" acts as")
	if credentialError != nil {
		return requestCaller{}, credentialError
	}
	caller.peer = &peerCaller{grantID: grant.ID, connectionID: grant.ConnectionID, level: grant.Level}
	return caller, nil
}

// forget drops the cached answer for principalID, so the next request reads
// principal-store again. This server calls it after it changes the principal
// itself, as granting a peer a level does, so a new level applies at once.
func (cache *principalLookupCache) forget(principalID string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	delete(cache.principalsByID, principalID)
}
