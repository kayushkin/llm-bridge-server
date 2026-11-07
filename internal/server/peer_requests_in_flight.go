package server

import (
	"context"
	"net/http"
	"sync"
)

// peerRequestsInFlight holds a cancel for every request a peer token is being
// served under right now, by https or over a peer socket. Access is checked
// once, when a request starts, so without this an event stream opened before
// a grant was revoked, a connection removed or a user mapping taken away kept
// flowing after it (found 2026-11-07). Ending access cancels the requests it
// covered; the other server may reopen a stream, and that request is checked
// against access as it is now.
type peerRequestsInFlight struct {
	mutex    sync.Mutex
	nextID   uint64
	requests map[uint64]peerRequestInFlight
}

type peerRequestInFlight struct {
	// grantID is the grant whose token the request presented. Every way
	// access ends — a revoke, a removed connection on either side, a new
	// grant replacing it — revokes that grant.
	grantID string
	// mappingID is the user mapping the request acts through, empty when it
	// acts as the whole peer.
	mappingID string
	cancel    context.CancelFunc
}

func newPeerRequestsInFlight() *peerRequestsInFlight {
	return &peerRequestsInFlight{requests: map[uint64]peerRequestInFlight{}}
}

// track returns r with a context that ending the caller's access cancels,
// and the function that forgets it once the request is served.
func (inFlight *peerRequestsInFlight) track(r *http.Request, peer *peerCaller) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	inFlight.mutex.Lock()
	inFlight.nextID++
	id := inFlight.nextID
	inFlight.requests[id] = peerRequestInFlight{grantID: peer.grantID, mappingID: peer.mappingID, cancel: cancel}
	inFlight.mutex.Unlock()
	return r.WithContext(ctx), func() {
		inFlight.mutex.Lock()
		delete(inFlight.requests, id)
		inFlight.mutex.Unlock()
		cancel()
	}
}

// cancelGrant ends every request served under the grant's token and answers
// how many it ended.
func (inFlight *peerRequestsInFlight) cancelGrant(grantID string) int {
	return inFlight.cancelWhere(func(request peerRequestInFlight) bool { return request.grantID == grantID })
}

// cancelMapping ends every request acting through one user mapping.
func (inFlight *peerRequestsInFlight) cancelMapping(mappingID string) int {
	return inFlight.cancelWhere(func(request peerRequestInFlight) bool { return request.mappingID == mappingID })
}

func (inFlight *peerRequestsInFlight) cancelWhere(matches func(peerRequestInFlight) bool) int {
	inFlight.mutex.Lock()
	defer inFlight.mutex.Unlock()
	cancelled := 0
	for id, request := range inFlight.requests {
		if matches(request) {
			request.cancel()
			delete(inFlight.requests, id)
			cancelled++
		}
	}
	return cancelled
}
