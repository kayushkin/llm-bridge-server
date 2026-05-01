package harness

import (
	"sync"

	"github.com/kayushkin/llm-bridge/msg"
)

// pendingHooks tracks HookEvents that emitted phase="awaiting_resolution"
// and have not yet been closed by a matching phase="completed". Keyed by
// bridge_session_id → request_id. The Manager updates this map inside
// readEvents so newly-connected SSE clients (or the dedicated
// /hooks/pending endpoint) can recover the current set without replaying
// the full event stream.
type pendingHooks struct {
	mu sync.RWMutex
	m  map[string]map[string]msg.Event
}

func newPendingHooks() *pendingHooks {
	return &pendingHooks{m: make(map[string]map[string]msg.Event)}
}

// record updates the pending set for the given session based on a HookEvent.
// awaiting_resolution adds; completed removes; other phases are ignored.
// Returns true if the set changed (caller may want to log / metricize).
func (p *pendingHooks) record(bridgeID string, ev *msg.Event) bool {
	if ev == nil || ev.Hook == nil || ev.Hook.RequestID == "" {
		return false
	}
	switch ev.Hook.Phase {
	case "awaiting_resolution":
		p.mu.Lock()
		defer p.mu.Unlock()
		bucket := p.m[bridgeID]
		if bucket == nil {
			bucket = make(map[string]msg.Event)
			p.m[bridgeID] = bucket
		}
		bucket[ev.Hook.RequestID] = *ev
		return true
	case "completed":
		p.mu.Lock()
		defer p.mu.Unlock()
		bucket, ok := p.m[bridgeID]
		if !ok {
			return false
		}
		if _, exists := bucket[ev.Hook.RequestID]; !exists {
			return false
		}
		delete(bucket, ev.Hook.RequestID)
		if len(bucket) == 0 {
			delete(p.m, bridgeID)
		}
		return true
	}
	return false
}

// list returns a snapshot of currently-pending awaiting_resolution events
// for the session. Returns nil for sessions with no pending hooks.
func (p *pendingHooks) list(bridgeID string) []msg.Event {
	p.mu.RLock()
	defer p.mu.RUnlock()
	bucket, ok := p.m[bridgeID]
	if !ok || len(bucket) == 0 {
		return nil
	}
	out := make([]msg.Event, 0, len(bucket))
	for _, ev := range bucket {
		out = append(out, ev)
	}
	return out
}

// drop removes all pending hooks for a session — called when the harness
// process exits so a respawn doesn't see ghost requests.
func (p *pendingHooks) drop(bridgeID string) {
	p.mu.Lock()
	delete(p.m, bridgeID)
	p.mu.Unlock()
}
