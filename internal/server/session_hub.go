package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// sseKeepaliveInterval is how often an otherwise-silent SSE stream emits a
// comment frame. It must stay comfortably below the idle read timeout of any
// intermediary in front of the server — nginx's proxy_read_timeout defaults to
// 60s, which is the shortest one this server is actually deployed behind.
const sseKeepaliveInterval = 25 * time.Second

// sessionListReplayCapacity is how many past frames the hub keeps so a client
// that reconnects with a Last-Event-ID can be handed exactly what it missed.
// A client that was away for more frames than this is told so (resume "gap")
// and re-seeds from GET /sessions — the buffer bounds memory, and saying "gap"
// out loud is what keeps a too-small buffer from silently losing an upsert,
// which is the defect this replay path exists to close.
const sessionListReplayCapacity = 512

// resumeState tells a reconnecting client what the hub was able to give it.
// It is deliberately three values rather than a bool: "this is a fresh
// connection" and "you asked to resume and I could not" both leave the client
// re-seeding, but only the second one means frames were lost, and collapsing
// them would hide that.
const (
	resumeNone     = "none"     // no Last-Event-ID was sent; nothing to replay
	resumeReplayed = "replayed" // every frame since Last-Event-ID was delivered
	resumeGap      = "gap"      // frames were missed; the client must re-seed
)

// sessionListEvent is one frame on the global session-list SSE stream.
// Type is either "upsert" (Session populated) or "delete" (SessionID populated).
type sessionListEvent struct {
	Type      string         `json:"type"`
	Session   *store.Session `json:"session,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
}

// sessionListFrame is one numbered, already-rendered frame. The sequence lives
// beside the payload rather than inside it because it travels on the SSE `id:`
// line, which is where EventSource already reads it from and echoes it back in
// Last-Event-ID.
//
// The payload is marshalled once, at publish, rather than once per subscriber
// and again on every replay. That also puts the only place marshalling can fail
// at the only place that can do something about it: a frame that will not
// render is never numbered, so it leaves no hole in the sequence and no frame
// that would fail again on every reconnect.
type sessionListFrame struct {
	seq  uint64
	typ  string
	data []byte
}

// sessionHub fans out session-row mutation signals to all SSE subscribers.
// One hub per server instance. Subscriber channels are buffered; if a client
// can't keep up, its channel is closed (the SSE handler then disconnects).
//
// The hub also numbers every frame it publishes and keeps the last
// sessionListReplayCapacity of them, so a reconnecting client can be given the
// frames it missed instead of a silent hole. Numbering is per process: streamID
// is minted at construction and prefixes every event id, so a sequence number
// from a previous process can never be mistaken for one of this process's.
type sessionHub struct {
	store    *store.Store
	streamID string
	mu       sync.Mutex
	subs     map[uint64]chan sessionListFrame
	seq      uint64
	replay   []sessionListFrame
	next     atomic.Uint64
}

func newSessionHub(st *store.Store) *sessionHub {
	return &sessionHub{
		store:    st,
		streamID: newStreamID(),
		subs:     map[uint64]chan sessionListFrame{},
	}
}

// newStreamID mints the per-process prefix for this hub's event ids.
func newStreamID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A hub whose ids are not unique per process would let a stale
		// Last-Event-ID replay the wrong frames, which is worse than the gap
		// this whole path exists to close. Fail loudly rather than guess.
		panic(fmt.Sprintf("session hub: cannot mint stream id: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// eventID renders the SSE `id:` line value for a sequence number.
func (h *sessionHub) eventID(seq uint64) string {
	return h.streamID + "-" + strconv.FormatUint(seq, 10)
}

// parseEventID pulls the sequence number out of a Last-Event-ID this hub
// minted. ok is false when the id came from another process's stream (or is
// malformed), which means its sequence numbers mean nothing here.
func (h *sessionHub) parseEventID(id string) (uint64, bool) {
	prefix, rest, found := strings.Cut(id, "-")
	if !found || prefix != h.streamID {
		return 0, false
	}
	seq, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
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

// OnSignalsChanged announces that a session's open questions have moved:
// one was raised, answered, dismissed or superseded.
//
// It carries no signal payload, deliberately. The frame says WHICH session
// changed and nothing else, so a client re-reads the open set it already knows
// how to read. Putting the rows on the wire here would be a second encoding of
// the signals API that could disagree with it, and every surface would then
// have two ways to learn the same fact.
//
// This is the push channel signals never had. Freshness was a 30-second cache
// plus an in-process announcement, so two surfaces on one screen could
// disagree for half a minute, and a question answered elsewhere — another tab,
// the CLI, an orchestrator — stayed on screen until the TTL lapsed. Now that
// answering is a real round trip through the server, that staleness is
// something a person watches happen.
func (h *sessionHub) OnSignalsChanged(bridgeID string) {
	if h == nil {
		return
	}
	h.publish(sessionListEvent{Type: "signal", SessionID: bridgeID})
}

// OnSessionDeleted implements store.Notifier.
func (h *sessionHub) OnSessionDeleted(bridgeID string) {
	if h == nil {
		return
	}
	h.publish(sessionListEvent{Type: "delete", SessionID: bridgeID})
}

func (h *sessionHub) publish(ev sessionListEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[session-hub] marshal %s: %v", ev.Type, err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	frame := sessionListFrame{seq: h.seq, typ: ev.Type, data: data}

	h.replay = append(h.replay, frame)
	if len(h.replay) > sessionListReplayCapacity {
		// Drop the oldest, keeping the slice's backing array bounded by
		// re-slicing onto a fresh array rather than growing forever.
		trimmed := make([]sessionListFrame, sessionListReplayCapacity)
		copy(trimmed, h.replay[len(h.replay)-sessionListReplayCapacity:])
		h.replay = trimmed
	}

	for id, ch := range h.subs {
		select {
		case ch <- frame:
		default:
			// Slow subscriber: close and remove. The SSE handler reads from
			// the channel and disconnects when it sees the close, freeing
			// the connection for the client to reconnect.
			close(ch)
			delete(h.subs, id)
		}
	}
}

// subscribe registers a new subscriber and, when lastEventID names a frame this
// hub still holds, hands back everything published after it.
//
// The backlog is taken under the same lock that registers the channel, so a
// frame published concurrently is either in the backlog or on the channel and
// never in both and never in neither. resume says which of the three states
// the caller is in; on resumeGap the caller has missed frames and must re-seed.
func (h *sessionHub) subscribe(lastEventID string) (id uint64, ch <-chan sessionListFrame, backlog []sessionListFrame, resume string) {
	sub := make(chan sessionListFrame, 256)
	id = h.next.Add(1)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[id] = sub

	if lastEventID == "" {
		return id, sub, nil, resumeNone
	}
	lastSeq, ok := h.parseEventID(lastEventID)
	if !ok || lastSeq > h.seq {
		// Either another process minted it, or it names a frame this process
		// has not reached. Both mean its number tells us nothing.
		return id, sub, nil, resumeGap
	}
	if lastSeq == h.seq {
		return id, sub, nil, resumeReplayed
	}
	// The buffer must still reach back far enough to cover the very next frame
	// after the one the client saw, or there is a hole between them.
	if len(h.replay) == 0 || h.replay[0].seq > lastSeq+1 {
		return id, sub, nil, resumeGap
	}
	for i, frame := range h.replay {
		if frame.seq > lastSeq {
			backlog = append(backlog, h.replay[i:]...)
			break
		}
	}
	return id, sub, backlog, resumeReplayed
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

// sessionListHello is the first frame on the global session-list stream. It
// names the stream and reports what a resume attempt got, so a client can tell
// "you are up to date" from "you missed frames, re-seed" instead of assuming
// the first case and losing a row.
type sessionListHello struct {
	StreamID    string `json:"stream_id"`
	Resume      string `json:"resume"`
	LastEventID string `json:"last_event_id,omitempty"`
}

// handleSessionListEvents serves the global session-list SSE stream. Clients
// open one connection per browser session and patch their local session list
// from the upsert/delete frames; the initial GET /sessions snapshot still
// seeds the list.
//
// Every frame carries an `id:` line, and a reconnect that sends Last-Event-ID
// back is replayed from the hub's buffer — the same contract the per-session
// stream in sessions.go has always had. Without it an upsert that landed during
// a reconnect window was gone for good, which is why bridge-ui still polls
// /sessions and runs a stuck-running watchdog alongside this stream.
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

	lastEventID := r.Header.Get("Last-Event-ID")
	id, ch, backlog, resume := s.sessionHub.subscribe(lastEventID)
	defer s.sessionHub.unsubscribe(id)

	// Initial hello so the client knows the stream is live without waiting
	// for the first mutation.
	hello, err := json.Marshal(sessionListHello{
		StreamID:    s.sessionHub.streamID,
		Resume:      resume,
		LastEventID: lastEventID,
	})
	if err != nil {
		log.Printf("[session-hub] marshal hello: %v", err)
		return
	}
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello)
	flusher.Flush()

	// Everything the client missed, in the order it was published, before any
	// live frame — the backlog was snapshotted at subscribe time, so no live
	// frame can be older than the last one here.
	for _, frame := range backlog {
		if !s.writeSessionListFrame(w, frame) {
			return
		}
	}
	if len(backlog) > 0 {
		flusher.Flush()
	}

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			// This stream only emits on session mutation, so it can sit silent
			// for minutes. Any intermediary with an idle read timeout (nginx's
			// proxy_read_timeout defaults to 60s) will reap it, and over HTTP/2
			// that surfaces in the browser as ERR_HTTP2_PROTOCOL_ERROR rather
			// than a clean close. An SSE comment keeps the connection warm and
			// is ignored by EventSource.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-ch:
			if !ok {
				// Hub dropped us as a slow subscriber.
				return
			}
			if !s.writeSessionListFrame(w, frame) {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSessionListFrame renders one frame, id line included. It returns false
// when the connection is gone and the handler should stop.
func (s *Server) writeSessionListFrame(w http.ResponseWriter, frame sessionListFrame) bool {
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n",
		s.sessionHub.eventID(frame.seq), frame.typ, frame.data)
	return err == nil
}
