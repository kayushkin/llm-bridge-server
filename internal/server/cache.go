package server

import (
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// responseCache is a tiny in-memory cache for the SERIALIZED dashv2 summary and
// recent-bundle responses (L3 in dashv2-architecture.md §3). It caches the
// already-encoded bytes so a hot refresh skips both the DB projection and JSON
// encoding — the DB is fast, but re-serializing the list/bundle on every poll
// is the avoidable cost this removes.
//
// It is NOT a DB row cache. Coherence is push-based: responseCache implements
// store.Notifier, and any session-row mutation clears it wholesale. That is
// deliberately coarse — correctness over cleverness. A mutation is comparatively
// rare against reads, and clearing is O(1); keying invalidation to individual
// rows would risk serving a stale list after a delete (which lowers the row
// count without necessarily moving max(updated_at)).
type responseCache struct {
	mu      sync.RWMutex
	entries map[string][]byte
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string][]byte{}}
}

// get returns the cached bytes for key and whether they were present. The
// returned slice must not be mutated by the caller (it is shared).
func (c *responseCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.entries[key]
	return b, ok
}

// put stores the serialized response bytes under key.
func (c *responseCache) put(key string, body []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = body
}

// invalidate drops every cached response. Called from the store Notifier on any
// session-row change or delete.
func (c *responseCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > 0 {
		c.entries = map[string][]byte{}
	}
}

// OnSessionChanged implements store.Notifier.
func (c *responseCache) OnSessionChanged(string) { c.invalidate() }

// OnSessionDeleted implements store.Notifier.
func (c *responseCache) OnSessionDeleted(string) { c.invalidate() }

// OnSignalsChanged invalidates too. The cached session summaries do not carry
// signal rows today, but the sidebar's waiting mark is derived from the open
// set, and a cache that outlived a question being answered would keep serving
// a session as waiting after it stopped. Invalidating on a signal write is
// cheaper than reasoning about which projections happen to embed one.
func (c *responseCache) OnSignalsChanged(string) { c.invalidate() }

// notifierFanout multiplexes store mutation signals to several Notifiers. The
// store accepts a single Notifier; the SSE hub and the response cache both need
// the signal, so this fans it out. Order is preserved; all targets are always
// called.
type notifierFanout struct {
	targets []store.Notifier
}

func newNotifierFanout(targets ...store.Notifier) *notifierFanout {
	return &notifierFanout{targets: targets}
}

func (f *notifierFanout) OnSessionChanged(id string) {
	for _, t := range f.targets {
		t.OnSessionChanged(id)
	}
}

func (f *notifierFanout) OnSessionDeleted(id string) {
	for _, t := range f.targets {
		t.OnSessionDeleted(id)
	}
}

func (f *notifierFanout) OnSignalsChanged(id string) {
	for _, t := range f.targets {
		t.OnSignalsChanged(id)
	}
}
