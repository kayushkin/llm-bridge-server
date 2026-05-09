package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// sessionListEvent is one frame on the global session-list SSE stream.
// Type is either "upsert" (Session populated) or "delete" (SessionID populated).
type sessionListEvent struct {
	Type      string         `json:"type"`
	Session   *store.Session `json:"session,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
}

// sessionHub fans out session-row mutation signals to all SSE subscribers.
// One hub per server instance. Subscriber channels are buffered; if a client
// can't keep up, its channel is closed (the SSE handler then disconnects).
type sessionHub struct {
	store *store.Store
	mu    sync.Mutex
	subs  map[uint64]chan sessionListEvent
	next  atomic.Uint64
}

func newSessionHub(st *store.Store) *sessionHub {
	return &sessionHub{store: st, subs: map[uint64]chan sessionListEvent{}}
}

// OnSessionChanged implements store.Notifier. Re-reads the session row and
// publishes an upsert frame. Read goes through the read-only handle, so it
// does not block the writer.
func (h *sessionHub) OnSessionChanged(bridgeID string) {
	if h == nil {
		return
	}
	sess, err := h.store.GetSession(bridgeID)
	if err != nil || sess == nil {
		return
	}
	h.publish(sessionListEvent{Type: "upsert", Session: sess})
}

// OnSessionDeleted implements store.Notifier.
func (h *sessionHub) OnSessionDeleted(bridgeID string) {
	if h == nil {
		return
	}
	h.publish(sessionListEvent{Type: "delete", SessionID: bridgeID})
}

func (h *sessionHub) publish(ev sessionListEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: close and remove. The SSE handler reads from
			// the channel and disconnects when it sees the close, freeing
			// the connection for the client to reconnect.
			close(ch)
			delete(h.subs, id)
		}
	}
}

func (h *sessionHub) subscribe() (uint64, <-chan sessionListEvent) {
	id := h.next.Add(1)
	ch := make(chan sessionListEvent, 256)
	h.mu.Lock()
	h.subs[id] = ch
	h.mu.Unlock()
	return id, ch
}

func (h *sessionHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		// Don't close — the publisher may have already closed it on a slow
		// subscriber path. Closing again would panic. The handler's select
		// loop tolerates an open-but-unread channel via context cancel.
		_ = ch
	}
}

// handleSessionListEvents serves the global session-list SSE stream. Clients
// open one connection per browser session and patch their local session list
// from the upsert/delete frames; the initial GET /sessions snapshot still
// seeds the list.
func (s *Server) handleSessionListEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Initial hello so the client knows the stream is live without waiting
	// for the first mutation.
	fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()

	id, ch := s.sessionHub.subscribe()
	defer s.sessionHub.unsubscribe(id)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Hub dropped us as a slow subscriber.
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				log.Printf("[session-hub] marshal: %v", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
