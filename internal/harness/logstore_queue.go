package harness

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	logstore "github.com/kayushkin/log-store/client"
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

// A push that fails is retried, not dropped. The header above promises
// producers block rather than lose an event, and since the queue was
// written the one place that promise was not kept was the push itself: a
// log-store restart — deploy.sh stops and starts the unit, about a second
// — refused every connection in that window and each refused event was
// logged once and gone. Measured in the system journal 2026-08-20 →
// 2026-09-03: 59 events lost, 55 of them "connection refused", 4 client
// timeouts.
//
// The writer sleeps between attempts, doubling from the base delay up to
// the cap, and gives up at the deadline. Thirty seconds covers a restart
// several times over and a slow migration on boot; past it log-store is
// down rather than restarting, and the event is dropped LOUDLY — the drop
// is the failure, and the log line says so with the word DROPPED.
//
// While the writer retries, the session's queue fills and its producer —
// the event pump — blocks on Enqueue, which stalls that session's SSE
// fan-out too. That is the contract stated above, and a one-second
// restart costs a one-second pause. What it must NOT cost is every live
// chat frozen at one event per deadline while log-store is dead for an
// hour, so hitting the deadline once marks log-store down: from then on
// each push gets ONE attempt — still loud when it fails — until one
// succeeds, which marks it up again. A dead log-store degrades to what
// this code did before, minus the silence.
const (
	logStoreRetryBaseDelay = 50 * time.Millisecond
	logStoreRetryMaxDelay  = 2 * time.Second
	logStoreRetryDeadline  = 30 * time.Second
)

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
	// The retry schedule for a failed push; see logStoreRetryDeadline.
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	retryDeadline  time.Duration
	// sleep is time.Sleep, replaceable so a test can run the schedule
	// without waiting it out.
	sleep func(time.Duration)
	// downSince is when a push last exhausted its deadline, in UnixNano,
	// or 0 while log-store is believed up. Queue-wide, not per session:
	// one log-store serves every session, so one of them learning it is
	// down is all of them learning it.
	downSince atomic.Int64
}

func newLogStoreQueue(push func(msg.Event) (int64, error)) *logStoreQueue {
	return &logStoreQueue{
		queues:         make(map[string]*sessionLogQueue),
		push:           push,
		depth:          logStoreQueueDepth,
		drainTimeout:   logStoreDrainTimeout,
		retryBaseDelay: logStoreRetryBaseDelay,
		retryMaxDelay:  logStoreRetryMaxDelay,
		retryDeadline:  logStoreRetryDeadline,
		sleep:          time.Sleep,
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
		q.pushOrDrop(label, ev)
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
		q.pushOrDrop(item.label, item.event)
	}
}

// pushOrDrop writes ev, retrying a failure that a retry can fix, and drops
// it — loudly — when one cannot or when the deadline passes. It runs on
// the session's writer goroutine, so a retry here holds that session's
// queue and keeps its order; the next event does not overtake this one.
func (q *logStoreQueue) pushOrDrop(label string, ev msg.Event) {
	start := time.Now()
	delay := q.retryBaseDelay
	for attempt := 1; ; attempt++ {
		_, err := q.push(ev)
		if err == nil {
			if since := q.downSince.Swap(0); since != 0 {
				log.Printf("[harness] log-store is back after being down since %s; pushes are retried again",
					time.Unix(0, since).Format(time.RFC3339))
			}
			if attempt > 1 {
				log.Printf("[harness] pushed %s for %s to log-store on attempt %d after %s",
					label, ev.BridgeSessionID, attempt, time.Since(start).Round(time.Millisecond))
			}
			return
		}
		if why := whyNotToRetryLogStorePush(err); why != "" {
			log.Printf("[harness] DROPPED %s for %s: log-store push failed and %s: %v",
				label, ev.BridgeSessionID, why, err)
			return
		}
		if since := q.downSince.Load(); since != 0 {
			log.Printf("[harness] DROPPED %s for %s: log-store has been down since %s and is not retried until a push succeeds: %v",
				label, ev.BridgeSessionID, time.Unix(0, since).Format(time.RFC3339), err)
			return
		}
		if time.Since(start)+delay > q.retryDeadline {
			q.downSince.CompareAndSwap(0, time.Now().UnixNano())
			log.Printf("[harness] DROPPED %s for %s after %d attempts over %s; log-store is down, not restarting, and gets one attempt per push until one succeeds: %v",
				label, ev.BridgeSessionID, attempt, time.Since(start).Round(time.Millisecond), err)
			return
		}
		if attempt == 1 {
			log.Printf("[harness] failed to push %s for %s to log-store; retrying for up to %s: %v",
				label, ev.BridgeSessionID, q.retryDeadline, err)
		}
		q.sleep(delay)
		if delay *= 2; delay > q.retryMaxDelay {
			delay = q.retryMaxDelay
		}
	}
}

// whyNotToRetryLogStorePush names the reason a failed push must not be
// re-sent, or returns "" when a retry is the right move.
//
// Two failures are final. A 4xx is log-store REJECTING the event, and the
// same body re-sent is rejected the same way. A timeout is worse: the
// request may have reached log-store and been stored before the client
// gave up, and log-store's ingest is a plain INSERT with no idempotency
// key, so re-sending could store the event twice — and a duplicated
// stream delta corrupts a transcript in a way a missing one does not.
// Everything else — connection refused, reset, EOF before a response,
// 5xx — means log-store stored nothing, so a retry cannot duplicate.
func whyNotToRetryLogStorePush(err error) string {
	var status *logstore.StatusError
	if errors.As(err, &status) {
		if status.StatusCode < 500 {
			return "log-store rejected it, so re-sending the same event cannot help"
		}
		return ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "the request timed out, so log-store may already hold it and a retry could store it twice"
	}
	return ""
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
