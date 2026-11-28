package sessions

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

	"github.com/kayushkin/llm-bridge-server/internal/serversentevents"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// SessionListReplayCapacity is how many past frames the hub keeps so a client
// that reconnects with a Last-Event-ID can be handed exactly what it missed.
// A client that was away for more frames than this is told so (resume "gap")
// and re-seeds from GET /sessions — the buffer bounds memory, and saying "gap"
// out loud is what keeps a too-small buffer from silently losing an upsert,
// which is the defect this replay path exists to close.
const SessionListReplayCapacity = 512

// resumeState tells a reconnecting client what the hub was able to give it.
// It is deliberately three values rather than a bool: "this is a fresh
// connection" and "you asked to resume and I could not" both leave the client
// re-seeding, but only the second one means frames were lost, and collapsing
// them would hide that.
const (
	ResumeNone     = "none"     // no Last-Event-ID was sent; nothing to replay
	ResumeReplayed = "replayed" // every frame since Last-Event-ID was delivered
	ResumeGap      = "gap"      // frames were missed; the client must re-seed
)

// SessionListEvent is one frame on the global session-list SSE stream.
// Type is either "upsert" (Session populated) or "delete" (SessionID populated).
type SessionListEvent struct {
	Type      string         `json:"type"`
	Session   *store.Session `json:"session,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
}

// SessionListFrame is one numbered, already-rendered frame. The sequence lives
// beside the payload rather than inside it because it travels on the SSE `id:`
// line, which is where EventSource already reads it from and echoes it back in
// Last-Event-ID.
//
// The payload is marshalled once, at publish, rather than once per subscriber
// and again on every replay. That also puts the only place marshalling can fail
// at the only place that can do something about it: a frame that will not
// render is never numbered, so it leaves no hole in the sequence and no frame
// that would fail again on every reconnect.
type SessionListFrame struct {
	Seq  uint64
	typ  string
	data []byte
	// ownerPrincipalID is the principal_id of the session the frame is about,
	// read when the frame was published, so a subscriber restricted to one
	// principal is written only that principal's frames. Empty for a session
	// with no principal — and for a delete, whose row is already gone when
	// the store announces it; no path deletes a principal's session today
	// (only the renamer deletes, and its sessions have no principal), so a
	// restricted subscriber misses no delete it could have been sent.
	ownerPrincipalID string
	// sessionID is the session the frame is about, so a subscriber that is a
	// connected server's user is written only the frames of sessions started
	// through that server.
	sessionID string
}

// SessionHub fans out session-row mutation signals to all SSE subscribers.
// One hub per server instance. Subscriber channels are buffered; if a client
// can't keep up, its channel is closed (the SSE handler then disconnects).
//
// The hub also numbers every frame it publishes and keeps the last
// SessionListReplayCapacity of them, so a reconnecting client can be given the
// frames it missed instead of a silent hole. Numbering is per process: streamID
// is minted at construction and prefixes every event id, so a sequence number
// from a previous process can never be mistaken for one of this process's.
type SessionHub struct {
	store    *store.Store
	StreamID string
	Mu       sync.Mutex
	subs     map[uint64]chan SessionListFrame
	seq      uint64
	Replay   []SessionListFrame
	next     atomic.Uint64
}

func NewSessionHub(st *store.Store) *SessionHub {
	return &SessionHub{
		store:    st,
		StreamID: newStreamID(),
		subs:     map[uint64]chan SessionListFrame{},
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

// EventID renders the SSE `id:` line value for a sequence number.
func (h *SessionHub) EventID(seq uint64) string {
	return h.StreamID + "-" + strconv.FormatUint(seq, 10)
}

// parseEventID pulls the sequence number out of a Last-Event-ID this hub
// minted. ok is false when the id came from another process's stream (or is
// malformed), which means its sequence numbers mean nothing here.
func (h *SessionHub) parseEventID(id string) (uint64, bool) {
	prefix, rest, found := strings.Cut(id, "-")
	if !found || prefix != h.StreamID {
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
func (h *SessionHub) OnSessionChanged(bridgeID string) {
	if h == nil {
		return
	}
	sess, err := h.store.GetSession(bridgeID)
	if err != nil || sess == nil {
		return
	}
	h.PublishOwnedBy(SessionListEvent{Type: "upsert", Session: sess}, sess.PrincipalID)
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
func (h *SessionHub) OnSignalsChanged(bridgeID string) {
	if h == nil {
		return
	}
	ownerPrincipalID := ""
	if sess, err := h.store.GetSession(bridgeID); err == nil {
		ownerPrincipalID = sess.PrincipalID
	}
	h.PublishOwnedBy(SessionListEvent{Type: "signal", SessionID: bridgeID}, ownerPrincipalID)
}

// OnSessionBriefChanged announces that a session's brief or loose ends moved.
// Like a signal frame it names the session and carries no rows: a client
// re-reads the brief through GET /sessions/{id}/brief.
func (h *SessionHub) OnSessionBriefChanged(bridgeID string) {
	if h == nil {
		return
	}
	ownerPrincipalID := ""
	if sess, err := h.store.GetSession(bridgeID); err == nil {
		ownerPrincipalID = sess.PrincipalID
	}
	h.PublishOwnedBy(SessionListEvent{Type: "session_brief", SessionID: bridgeID}, ownerPrincipalID)
}

// OnSessionDeleted implements store.Notifier.
func (h *SessionHub) OnSessionDeleted(bridgeID string) {
	if h == nil {
		return
	}
	h.PublishOwnedBy(SessionListEvent{Type: "delete", SessionID: bridgeID}, "")
}

func (h *SessionHub) PublishOwnedBy(ev SessionListEvent, ownerPrincipalID string) {
	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[session-hub] marshal %s: %v", ev.Type, err)
		return
	}

	h.Mu.Lock()
	defer h.Mu.Unlock()

	h.seq++
	frameSessionID := ev.SessionID
	if frameSessionID == "" && ev.Session != nil {
		frameSessionID = ev.Session.SessionID
	}
	frame := SessionListFrame{Seq: h.seq, typ: ev.Type, data: data, ownerPrincipalID: ownerPrincipalID, sessionID: frameSessionID}

	h.Replay = append(h.Replay, frame)
	if len(h.Replay) > SessionListReplayCapacity {
		// Drop the oldest, keeping the slice's backing array bounded by
		// re-slicing onto a fresh array rather than growing forever.
		trimmed := make([]SessionListFrame, SessionListReplayCapacity)
		copy(trimmed, h.Replay[len(h.Replay)-SessionListReplayCapacity:])
		h.Replay = trimmed
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

// Subscribe registers a new subscriber and, when lastEventID names a frame this
// hub still holds, hands back everything published after it.
//
// The backlog is taken under the same lock that registers the channel, so a
// frame published concurrently is either in the backlog or on the channel and
// never in both and never in neither. resume says which of the three states
// the caller is in; on ResumeGap the caller has missed frames and must re-seed.
func (h *SessionHub) Subscribe(lastEventID string) (id uint64, ch <-chan SessionListFrame, backlog []SessionListFrame, resume string) {
	sub := make(chan SessionListFrame, 256)
	id = h.next.Add(1)

	h.Mu.Lock()
	defer h.Mu.Unlock()
	h.subs[id] = sub

	if lastEventID == "" {
		return id, sub, nil, ResumeNone
	}
	lastSeq, ok := h.parseEventID(lastEventID)
	if !ok || lastSeq > h.seq {
		// Either another process minted it, or it names a frame this process
		// has not reached. Both mean its number tells us nothing.
		return id, sub, nil, ResumeGap
	}
	if lastSeq == h.seq {
		return id, sub, nil, ResumeReplayed
	}
	// The buffer must still reach back far enough to cover the very next frame
	// after the one the client saw, or there is a hole between them.
	if len(h.Replay) == 0 || h.Replay[0].Seq > lastSeq+1 {
		return id, sub, nil, ResumeGap
	}
	for i, frame := range h.Replay {
		if frame.Seq > lastSeq {
			backlog = append(backlog, h.Replay[i:]...)
			break
		}
	}
	return id, sub, backlog, ResumeReplayed
}

func (h *SessionHub) Unsubscribe(id uint64) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
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

// HandleSessionListEvents serves the global session-list SSE stream. Clients
// open one connection per browser session and patch their local session list
// from the upsert/delete frames; the initial GET /sessions snapshot still
// seeds the list.
//
// Every frame carries an `id:` line, and a reconnect that sends Last-Event-ID
// back is replayed from the hub's buffer — the same contract the per-session
// stream in sessions.go has always had. Without it an upsert that landed during
// a reconnect window was gone for good, which is why bridge-ui still polls
// /sessions and runs a stuck-running watchdog alongside this stream.
func (s *Service) HandleSessionListEvents(w http.ResponseWriter, r *http.Request) {
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
	id, ch, backlog, resume := s.SessionHub.Subscribe(lastEventID)
	defer s.SessionHub.Unsubscribe(id)

	// Initial hello so the client knows the stream is live without waiting
	// for the first mutation.
	hello, err := json.Marshal(sessionListHello{
		StreamID:    s.SessionHub.StreamID,
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
	scope, restricted := requestcaller.SessionScopeRestrictingRequest(r)
	frameIsOutOfScope := func(frame SessionListFrame) bool {
		if !restricted {
			return false
		}
		if frame.ownerPrincipalID != scope.PrincipalID {
			return true
		}
		return scope.ThroughPeerPrincipalID != "" && !scope.ReachesSessionID(s.Store, frame.sessionID)
	}
	for _, frame := range backlog {
		if frameIsOutOfScope(frame) {
			continue
		}
		if !s.writeSessionListFrame(w, frame) {
			return
		}
	}
	if len(backlog) > 0 {
		flusher.Flush()
	}

	keepalive := time.NewTicker(serversentevents.SseKeepaliveInterval)
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
			if frameIsOutOfScope(frame) {
				continue
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
func (s *Service) writeSessionListFrame(w http.ResponseWriter, frame SessionListFrame) bool {
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n",
		s.SessionHub.EventID(frame.Seq), frame.typ, frame.data)
	return err == nil
}
