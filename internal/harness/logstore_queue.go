package harness

import (
	"log"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// logStoreQueueDepth bounds how many events may sit unwritten per session.
// The busiest observed session sustained ~70 events/second while log-store
// ingest answered in 1.6ms at one concurrent session and 8.8ms at eight, so
// this depth absorbs several seconds of a burst. When it fills, producers
// block: log-store is the durable source of truth and dropping an event
// would trade a latency defect for a data-loss one.
const logStoreQueueDepth = 256

// logStoreDrainTimeout bounds how long a caller waits for a session's
// queue to drain. A full queue drains in about 0.4s when log-store is
// healthy (256 events at the measured 1.6ms ingest) and about 2.3s under
// the eight-session load (8.8ms), so this leaves several times the
// headroom a real drain needs and only expires when log-store is wedged.
// It is deliberately close to the log-store client's own 5s per-request
// timeout: one of these waits can run while the manager lock is held
// (AssignMessageID → loadMsgState → RecoverInFlightTurn), and a wait much
// longer than a single synchronous push would have taken there is a
// regression however rare. Left unbounded, a full queue of 5s timeouts
// would hold that lock for twenty minutes.
//
// Expiry abandons the wait, never the events: the writer goroutine keeps
// draining behind it. The cost is a read that may miss the tail of a
// session, which is why it is logged rather than swallowed.
const logStoreDrainTimeout = 10 * time.Second

// logStoreItem is one unit of work for a session's writer goroutine.
// Exactly one of event/barrier is meaningful: a barrier carries no event
// and is closed once every item queued before it has been written, which
// is how Flush learns the session is drained.
type logStoreItem struct {
	event msg.Event
	// label reproduces the wording each call site logs today, so a push
	// failure still reads the same in the log after the move off the
	// caller's goroutine.
	label   string
	barrier chan struct{}
}

// sessionLogQueue is one session's ordered write queue plus the state
// needed to retire it without losing an event.
type sessionLogQueue struct {
	items chan logStoreItem
	// inflight counts producers that have taken a reference to this queue
	// but have not finished sending. Close waits on it so the channel is
	// never closed underneath a blocked send.
	inflight sync.WaitGroup
	// done closes when the writer goroutine has drained items and exited.
	done chan struct{}
}

// logStoreQueue writes events to log-store off the caller's goroutine,
// one ordered queue and one writer goroutine per session.
//
// It exists because the log-store push used to sit in front of the SSE
// fan-out: every token delta the browser was waiting on paid an HTTP
// round trip first. Moving the push behind the fan-out has to preserve
// two things log-store depends on — per-session write ORDER (it
// materialises history in row order) and READ-AFTER-WRITE for the three
// manager methods that read back what the pump just wrote. Order comes
// from the single writer per session; read-after-write comes from Flush.
//
// A queue exists only while a session's event pump is running: Open at
// the top of readEvents, Close in its exit path. Producers that arrive
// with no queue registered — a bridge-originated event on a session whose
// process has already gone, an event on a session that never had a pump —
// fall back to a synchronous push, which is exactly the behaviour that
// was there before.
type logStoreQueue struct {
	mu     sync.Mutex
	queues map[string]*sessionLogQueue
	push   func(msg.Event) (int64, error)
	depth  int
	// drainTimeout bounds Flush and Close; see logStoreDrainTimeout.
	drainTimeout time.Duration
}

func newLogStoreQueue(push func(msg.Event) (int64, error)) *logStoreQueue {
	return &logStoreQueue{
		queues:       make(map[string]*sessionLogQueue),
		push:         push,
		depth:        logStoreQueueDepth,
		drainTimeout: logStoreDrainTimeout,
	}
}

// Open registers a write queue for bridgeID and starts its writer
// goroutine. Calling it for a session that already has one is a no-op, so
// a restarted pump does not strand the previous queue.
func (q *logStoreQueue) Open(bridgeID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.queues[bridgeID]; ok {
		return
	}
	sq := &sessionLogQueue{
		items: make(chan logStoreItem, q.depth),
		done:  make(chan struct{}),
	}
	q.queues[bridgeID] = sq
	go q.writeLoop(sq)
}

// Close drains bridgeID's queue, stops its writer, and unregisters it.
// Every event already queued is written before Close returns — unless
// log-store is wedged and the drain outlives drainTimeout, in which case
// it logs and leaves the writer to finish on its own. Safe to call for a
// session with no queue.
//
// It drains BEFORE unregistering, not after, so an event produced by an
// HTTP handler while the pump is exiting still queues behind the pump's
// own events instead of overtaking them on the synchronous fallback.
// That leaves one narrow window, between the drain finishing and the
// unregister taking the lock, in which a bridge-originated event can
// land ahead of a straggler. It is not closed: doing so would mean
// holding the queue-wide lock across the whole drain, which would stall
// every other session, and what rides that window is teardown
// bookkeeping (hook, folder and rename events) on a session whose
// process has already gone.
func (q *logStoreQueue) Close(bridgeID string) {
	q.Flush(bridgeID)

	q.mu.Lock()
	sq := q.queues[bridgeID]
	delete(q.queues, bridgeID)
	q.mu.Unlock()
	if sq == nil {
		return
	}
	// Producers take their inflight ticket under q.mu while the queue is
	// still registered, so removing it above means no further send can
	// start. Waiting here lets the ones already blocked on a full channel
	// finish before the channel closes.
	sq.inflight.Wait()
	close(sq.items)
	if !waitFor(sq.done, q.drainTimeout) {
		log.Printf("[harness] log-store queue for %s still draining after %s; leaving %d events to the writer",
			bridgeID, q.drainTimeout, len(sq.items))
	}
}

// Enqueue hands ev to bridgeID's writer, blocking while the queue is
// full. With no queue registered it pushes synchronously, matching the
// pre-queue behaviour for sessions that have no running pump.
func (q *logStoreQueue) Enqueue(bridgeID, label string, ev msg.Event) {
	sq := q.acquire(bridgeID)
	if sq == nil {
		q.pushLogged(label, ev)
		return
	}
	sq.items <- logStoreItem{event: ev, label: label}
	sq.inflight.Done()
}

// Flush blocks until everything queued for bridgeID so far has been
// written to log-store. Call it before any read-back of log-store and
// before any synchronous push, so neither can observe a session whose
// most recent events are still sitting in the queue.
func (q *logStoreQueue) Flush(bridgeID string) {
	sq := q.acquire(bridgeID)
	if sq == nil {
		return
	}
	barrier := make(chan struct{})
	sq.items <- logStoreItem{barrier: barrier}
	sq.inflight.Done()
	if !waitFor(barrier, q.drainTimeout) {
		log.Printf("[harness] log-store queue for %s did not drain within %s; the read that follows may miss the session's most recent events",
			bridgeID, q.drainTimeout)
	}
}

// PushSync flushes bridgeID's queue and then writes ev on the caller's
// goroutine, returning log-store's row id and the real error. The write
// path that answers /send uses this: its caller propagates the failure to
// the client, so it cannot be moved off the request goroutine.
func (q *logStoreQueue) PushSync(bridgeID string, ev msg.Event) (int64, error) {
	q.Flush(bridgeID)
	return q.push(ev)
}

// acquire returns bridgeID's queue with an inflight ticket already taken,
// or nil when the session has none. The ticket is taken under the same
// lock that guards the map so it always happens-before a concurrent
// Close observes the map without this queue. The caller must call
// sq.inflight.Done() once its send has completed.
func (q *logStoreQueue) acquire(bridgeID string) *sessionLogQueue {
	q.mu.Lock()
	defer q.mu.Unlock()
	sq := q.queues[bridgeID]
	if sq == nil {
		return nil
	}
	sq.inflight.Add(1)
	return sq
}

func (q *logStoreQueue) writeLoop(sq *sessionLogQueue) {
	defer close(sq.done)
	for item := range sq.items {
		if item.barrier != nil {
			close(item.barrier)
			continue
		}
		q.pushLogged(item.label, item.event)
	}
}

func (q *logStoreQueue) pushLogged(label string, ev msg.Event) {
	if _, err := q.push(ev); err != nil {
		log.Printf("[harness] failed to push %s to log-store: %v", label, err)
	}
}

// waitFor reports whether ch closed before the timeout expired.
func waitFor(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}
