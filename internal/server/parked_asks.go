package server

import (
	"encoding/json"
	"sync"
)

// permissionDecision is the verdict delivered to a parked permission ask.
// Behavior is "allow" or "deny"; UpdatedInput and Message ride through to
// the hook response forwarded to Claude Code.
type permissionDecision struct {
	Behavior     string
	UpdatedInput json.RawMessage
	Message      string
	ResolvedBy   string
}

// parkedAsks tracks PreToolUse permission hooks whose verdict is "ask",
// blocked on bridge-ui's banner click. Keyed by bridge_session_id →
// request_id; the value is the channel the prehook handler is selecting on.
//
// Replaces the per-harness pendingHooks infrastructure inside
// llm-bridge-claudecode's PermissionMCP. Lives in bridge-server because
// that's where the parked HTTP handler (POST /permission/cc-prehook) and
// the resolve handler (POST /sessions/:id/hooks/:rid/resolve) both run.
type parkedAsks struct {
	mu sync.Mutex
	m  map[string]map[string]chan permissionDecision
}

func newParkedAsks() *parkedAsks {
	return &parkedAsks{m: make(map[string]map[string]chan permissionDecision)}
}

// park registers a new parked ask and returns the channel to block on.
// The buffered channel capacity is 1 so deliver() never blocks even if the
// reader is racing on its own context cancellation.
func (p *parkedAsks) park(bridgeID, requestID string) chan permissionDecision {
	ch := make(chan permissionDecision, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	bucket := p.m[bridgeID]
	if bucket == nil {
		bucket = make(map[string]chan permissionDecision)
		p.m[bridgeID] = bucket
	}
	bucket[requestID] = ch
	return ch
}

// deliver hands the decision to a parked ask. Returns true if a parked
// request was found and the decision was delivered; false if no matching
// entry exists (stale resolve, harness restart, never-parked request).
//
// The entry is removed on first delivery; a second resolve for the same
// (bridgeID, requestID) lands in the false branch — the caller can emit a
// stale phase=completed event for UI cleanup.
func (p *parkedAsks) deliver(bridgeID, requestID string, d permissionDecision) bool {
	p.mu.Lock()
	bucket, ok := p.m[bridgeID]
	if !ok {
		p.mu.Unlock()
		return false
	}
	ch, ok := bucket[requestID]
	if !ok {
		p.mu.Unlock()
		return false
	}
	delete(bucket, requestID)
	if len(bucket) == 0 {
		delete(p.m, bridgeID)
	}
	p.mu.Unlock()
	ch <- d
	return true
}

// cancel removes a parked entry without delivering. Called by the prehook
// handler when its request context is canceled (CC died, network drop) so
// a later resolve doesn't try to deliver to a dead channel.
func (p *parkedAsks) cancel(bridgeID, requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bucket, ok := p.m[bridgeID]
	if !ok {
		return
	}
	delete(bucket, requestID)
	if len(bucket) == 0 {
		delete(p.m, bridgeID)
	}
}

// dropSession drops every parked entry for a session. Caller is responsible
// for emitting completed-with-stale events; this method does not deliver.
// Used when the harness process exits so a respawn doesn't see ghosts.
func (p *parkedAsks) dropSession(bridgeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, bridgeID)
}
