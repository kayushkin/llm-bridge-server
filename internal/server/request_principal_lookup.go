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
	if principal.Kind != "human" {
		return requestCaller{}, &requestCredentialError{http.StatusBadRequest, "principal_not_human", fmt.Sprintf(
			"%s %s, which is a %q principal; a group is a set of people, not someone who acts", subjectDescription, principalID, principal.Kind)}
	}
	// Disabled is decided before is_administrator is looked at: a departed
	// administrator is a departed person first.
	if principal.DisabledAt != 0 {
		return requestCaller{}, &requestCredentialError{http.StatusForbidden, "principal_disabled", fmt.Sprintf(
			"principal %s was disabled in principal-store at %s", principalID, time.Unix(principal.DisabledAt, 0).UTC().Format(time.RFC3339))}
	}
	return requestCaller{principalID: principal.ID, isAdministrator: principal.IsAdministrator}, nil
}
