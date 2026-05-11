package harness

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"sync"

	"github.com/creack/pty"
)

// defaultPTYRingBufferBytes is the fallback when the manager is not given
// a configured size. The Server constructs Managers with the explicit
// LLMBRIDGE_PTY_RING_BUFFER_BYTES; this default only kicks in for tests
// or callers that pass 0.
const defaultPTYRingBufferBytes = 64 * 1024

// AttachHub fans pty output to multiple WebSocket clients while keeping
// a single writer with permission to type and resize. It owns the read
// pump from the underlying pty fd, a per-session ring buffer of recent
// output for late-attach replay, and the writer-promotion bookkeeping
// that runs when the current writer disconnects.
//
// One hub exists per pty session for the lifetime of the PTYProcess.
// When the underlying child exits, the hub drains its read pump, closes
// every client's done channel, and refuses further Attach calls.
type AttachHub struct {
	pp       *PTYProcess
	bridgeID string
	ringCap  int

	// attachToken gates the WebSocket upgrade. Minted once at hub
	// construction and immutable for the hub's lifetime; clients pass it
	// as ?token=… on the attach URL. In-memory only — when the pty
	// dies and the hub is dropped, the token is unreachable, so no
	// further attaches are possible against a session whose process is
	// gone. See PTY-MODE.md "Auth" for the chosen posture.
	attachToken string

	mu       sync.Mutex
	ring     []byte // grows up to ringCap, then operates as circular
	ringPos  int    // write head into ring
	ringFull bool
	lastSize pty.Winsize
	clients  map[*AttachClient]struct{}
	writer   *AttachClient

	closed    chan struct{}
	closeOnce sync.Once
}

// AttachClient is a single WebSocket attached to an AttachHub. Output
// bytes for this client land on Out(); the client copies them onto its
// WebSocket as binary frames. Done() closes when the hub tears down or
// the client is detached.
//
// Whether the client currently holds the writer slot is determined at
// the moment of Write/Resize via the hub. Role can flip silently when
// the previous writer disconnects (per PTY-MODE.md spec).
type AttachClient struct {
	hub       *AttachHub
	out       chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

// NewAttachHub constructs a hub bound to a running PTYProcess. The hub
// starts its read pump immediately so output produced before the first
// attacher connects is captured into the ring buffer.
//
// ringBufferBytes <= 0 falls back to defaultPTYRingBufferBytes. Callers
// (the manager) plumb the configured value through from config.
func NewAttachHub(pp *PTYProcess, ringBufferBytes int) *AttachHub {
	if ringBufferBytes <= 0 {
		ringBufferBytes = defaultPTYRingBufferBytes
	}
	hub := &AttachHub{
		pp:          pp,
		bridgeID:    pp.SessionID(),
		ringCap:     ringBufferBytes,
		attachToken: mintAttachToken(),
		lastSize:    pty.Winsize{Rows: 24, Cols: 80},
		clients:     make(map[*AttachClient]struct{}),
		closed:      make(chan struct{}),
	}
	go hub.readPump()
	go hub.watchExit()
	return hub
}

// mintAttachToken returns a 32-hex-char (128-bit) random token. Panics if
// the crypto RNG fails — that's an unrecoverable environmental error and
// silently falling back to a weaker source would defeat the auth posture.
func mintAttachToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("harness: attach token rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Token returns the per-hub attach token. The handler compares this
// against the ?token= query string using a constant-time compare before
// upgrading. Empty only on hubs constructed for tests via reflection.
func (h *AttachHub) Token() string {
	return h.attachToken
}

// Closed reports whether the hub has shut down (pty exited or fd closed).
func (h *AttachHub) Closed() bool {
	select {
	case <-h.closed:
		return true
	default:
		return false
	}
}

// readPump copies bytes from the pty fd to the ring buffer and to every
// attached client's output channel. Returns when the pty returns EOF or
// any other error, then closes the hub.
func (h *AttachHub) readPump() {
	defer h.shutdown()
	buf := make([]byte, 8*1024)
	for {
		n, err := h.pp.PTY().Read(buf)
		if n > 0 {
			h.broadcast(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[attach %s] pty read: %v", h.bridgeID, err)
			}
			return
		}
	}
}

// broadcast appends bytes to the ring buffer and sends a copy to each
// attached client. Slow clients drop frames silently — xterm.js redraws
// on every output frame so the only consequence is a momentary blank.
func (h *AttachHub) broadcast(data []byte) {
	h.mu.Lock()
	h.appendRing(data)
	clients := make([]*AttachClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	if len(clients) == 0 {
		return
	}
	frame := make([]byte, len(data))
	copy(frame, data)
	for _, c := range clients {
		select {
		case c.out <- frame:
		case <-c.closed:
		default:
		}
	}
}

// appendRing writes b into the circular buffer. Caller holds h.mu.
func (h *AttachHub) appendRing(b []byte) {
	if h.ringCap <= 0 {
		return
	}
	if len(b) >= h.ringCap {
		if h.ring == nil {
			h.ring = make([]byte, h.ringCap)
		}
		copy(h.ring, b[len(b)-h.ringCap:])
		h.ringPos = 0
		h.ringFull = true
		return
	}
	if h.ring == nil {
		h.ring = make([]byte, h.ringCap)
	}
	for len(b) > 0 {
		n := copy(h.ring[h.ringPos:], b)
		b = b[n:]
		h.ringPos += n
		if h.ringPos >= h.ringCap {
			h.ringPos = 0
			h.ringFull = true
		}
	}
}

// snapshotRing returns a chronologically-ordered copy of the current ring
// contents. Caller holds h.mu.
func (h *AttachHub) snapshotRing() []byte {
	if h.ring == nil {
		return nil
	}
	if !h.ringFull {
		out := make([]byte, h.ringPos)
		copy(out, h.ring[:h.ringPos])
		return out
	}
	out := make([]byte, h.ringCap)
	n := copy(out, h.ring[h.ringPos:])
	copy(out[n:], h.ring[:h.ringPos])
	return out
}

// watchExit closes the hub when the underlying pty process completes.
// readPump's shutdown defer also covers this; whichever runs first wins
// via closeOnce.
func (h *AttachHub) watchExit() {
	<-h.pp.Done()
	h.shutdown()
}

// shutdown closes the hub, releasing every connected client. Idempotent.
func (h *AttachHub) shutdown() {
	h.closeOnce.Do(func() {
		close(h.closed)
		h.mu.Lock()
		clients := h.clients
		h.clients = make(map[*AttachClient]struct{})
		h.writer = nil
		h.mu.Unlock()
		for c := range clients {
			c.closeOnce.Do(func() { close(c.closed) })
		}
	})
}

// ErrAttachClosed is returned by Attach when the hub has already shut
// down (the pty exited).
var ErrAttachClosed = errors.New("attach hub closed")

// Attach registers a new client. The first attacher becomes the writer;
// subsequent attachers are readers (their input frames will be silently
// dropped). The returned snapshot is the ring buffer contents at attach
// time and should be sent to the client before the live stream begins.
// The returned size is the last known winsize so a fresh writer can pick
// up where the previous left off.
func (h *AttachHub) Attach() (*AttachClient, []byte, pty.Winsize, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closed:
		return nil, nil, pty.Winsize{}, ErrAttachClosed
	default:
	}
	c := &AttachClient{
		hub: h,
		// Buffered so the read-pump's broadcast doesn't block on a slow
		// client. 64 frames is plenty: at 8KB/frame that's 512KB of
		// in-flight terminal output before drops kick in.
		out:    make(chan []byte, 64),
		closed: make(chan struct{}),
	}
	h.clients[c] = struct{}{}
	if h.writer == nil {
		h.writer = c
	}
	return c, h.snapshotRing(), h.lastSize, nil
}

// Detach unregisters a client. If the detached client was the writer,
// promotes the next remaining reader (any one) to writer. Idempotent
// for the same client; safe to call from a defer alongside hub shutdown.
func (h *AttachHub) Detach(c *AttachClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, c)
	wasWriter := h.writer == c
	if wasWriter {
		h.writer = nil
		// Pick any remaining client as the new writer. Map iteration
		// order is randomized in Go, but with at most a handful of
		// attached observers per session that's fine; first-attacher-
		// wins is only meaningful at initial connect time.
		for next := range h.clients {
			h.writer = next
			break
		}
	}
	h.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
}

// Write delivers bytes typed by a client to the pty fd. Writes from
// non-writer clients are silently dropped per spec ("read-only observers
// + one writer"). Returns an error only on real pty fd write failure.
func (h *AttachHub) Write(c *AttachClient, data []byte) error {
	h.mu.Lock()
	isWriter := h.writer == c
	h.mu.Unlock()
	if !isWriter {
		return nil
	}
	_, err := h.pp.PTY().Write(data)
	return err
}

// Resize forwards a writer-issued resize to the pty fd and stashes the
// size so a subsequent writer (after promotion) sees the same dimensions
// until it sends its own resize. Resizes from readers are silently
// dropped (no error returned, no fd touched).
func (h *AttachHub) Resize(c *AttachClient, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	h.mu.Lock()
	if h.writer != c {
		h.mu.Unlock()
		return nil
	}
	size := pty.Winsize{Rows: rows, Cols: cols}
	h.lastSize = size
	h.mu.Unlock()
	return pty.Setsize(h.pp.PTY(), &size)
}

// IsWriter reports whether the client currently holds the writer slot.
// Useful for clients that want to render a "read-only" indicator,
// though v1 leaves that decision to the integrator.
func (h *AttachHub) IsWriter(c *AttachClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writer == c
}

// Out returns the per-client output channel. The attach handler ranges
// over it, writing each chunk as a binary WebSocket frame.
func (c *AttachClient) Out() <-chan []byte { return c.out }

// Done is closed when the client is detached or the hub shuts down.
func (c *AttachClient) Done() <-chan struct{} { return c.closed }
