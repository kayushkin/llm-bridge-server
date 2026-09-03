package harness

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	logstore "github.com/kayushkin/log-store/client"
)

// recordingPusher stands in for the log-store client. It records what it
// was asked to write, in the order it was asked, and can be made slow or
// made to fail so a test can see the difference between "queued" and
// "written".
type recordingPusher struct {
	mu      sync.Mutex
	written []string // "<session>/<turn>" per accepted event, in write order
	delay   time.Duration
	gate    chan struct{} // when non-nil, every push waits on it first
	err     error
	// failFirst makes the next N pushes fail with err and then succeed —
	// a log-store that is restarting rather than gone. Zero means err
	// applies to every push.
	failFirst int
	attempts  int
}

func (p *recordingPusher) push(ev msg.Event) (int64, error) {
	p.mu.Lock()
	gate, delay := p.gate, p.delay
	p.mu.Unlock()

	if gate != nil {
		<-gate
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if p.err != nil && (p.failFirst == 0 || p.attempts <= p.failFirst) {
		return 0, p.err
	}
	p.written = append(p.written, ev.BridgeSessionID+"/"+ev.TurnID)
	return int64(len(p.written)), nil
}

func (p *recordingPusher) attemptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

func (p *recordingPusher) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.written...)
}

func (p *recordingPusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.written)
}

func event(session, turn string) msg.Event {
	return msg.Event{Type: msg.EventStream, BridgeSessionID: session, TurnID: turn}
}

// A session's events must reach log-store in the order they were
// produced: log-store materialises history in row order, so an unordered
// write corrupts the transcript. Two sessions writing at once must not
// interleave into each other's order either.
func TestLogStoreQueue_PreservesPerSessionOrder(t *testing.T) {
	p := &recordingPusher{delay: time.Millisecond}
	q := newLogStoreQueue(p.push)

	const perSession = 40
	sessions := []string{"br-a", "br-b"}
	for _, s := range sessions {
		q.Open(s)
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			for i := 0; i < perSession; i++ {
				q.Enqueue(session, "event", event(session, fmt.Sprintf("t%03d", i)))
			}
		}(s)
	}
	wg.Wait()
	for _, s := range sessions {
		q.Close(s)
	}

	perSessionOrder := map[string][]string{}
	for _, w := range p.snapshot() {
		session := w[:len("br-a")]
		perSessionOrder[session] = append(perSessionOrder[session], w)
	}
	for _, s := range sessions {
		got := perSessionOrder[s]
		if len(got) != perSession {
			t.Fatalf("session %s: wrote %d events; want %d", s, len(got), perSession)
		}
		for i, w := range got {
			want := fmt.Sprintf("%s/t%03d", s, i)
			if w != want {
				t.Fatalf("session %s position %d: wrote %q; want %q", s, i, w, want)
			}
		}
	}
}

// Flush is the read-after-write guarantee. Every read-back path calls it
// before querying log-store, so it must not return while any event queued
// before it is still unwritten.
func TestLogStoreQueue_FlushWaitsForEveryQueuedEvent(t *testing.T) {
	p := &recordingPusher{delay: 2 * time.Millisecond}
	q := newLogStoreQueue(p.push)
	q.Open("br-flush")
	defer q.Close("br-flush")

	const n = 25
	for i := 0; i < n; i++ {
		q.Enqueue("br-flush", "event", event("br-flush", fmt.Sprintf("t%03d", i)))
	}
	q.Flush("br-flush")

	if got := p.count(); got != n {
		t.Fatalf("after Flush, log-store holds %d of %d events; Flush returned early", got, n)
	}
}

// A full queue must block the producer, never drop. Dropping would trade
// this fix's latency win for silent data loss in the durable store.
func TestLogStoreQueue_BlocksWhenFullRatherThanDropping(t *testing.T) {
	gate := make(chan struct{})
	p := &recordingPusher{gate: gate}
	q := newLogStoreQueue(p.push)
	q.depth = 4
	q.Open("br-full")

	const n = 40 // ten times the queue depth
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			q.Enqueue("br-full", "event", event("br-full", fmt.Sprintf("t%03d", i)))
		}
	}()

	// With every push blocked on the gate, the producer must still be
	// stuck: a queue that dropped on overflow would have finished by now.
	select {
	case <-done:
		t.Fatal("producer finished while every push was blocked — events were dropped, not queued")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer never finished after the pushes were released")
	}
	q.Close("br-full")

	got := p.snapshot()
	if len(got) != n {
		t.Fatalf("log-store holds %d of %d events — %d were dropped", len(got), n, n-len(got))
	}
	for i, w := range got {
		if want := fmt.Sprintf("br-full/t%03d", i); w != want {
			t.Fatalf("position %d: wrote %q; want %q", i, w, want)
		}
	}
}

// The /send path returns its log-store error to the client, so it cannot
// move off the caller's goroutine. It must still land after everything
// the pump queued before it, and must hand back the real failure.
func TestLogStoreQueue_PushSyncOrdersAfterQueuedEventsAndReturnsError(t *testing.T) {
	p := &recordingPusher{delay: 2 * time.Millisecond}
	q := newLogStoreQueue(p.push)
	q.Open("br-sync")
	defer q.Close("br-sync")

	for i := 0; i < 10; i++ {
		q.Enqueue("br-sync", "event", event("br-sync", fmt.Sprintf("t%03d", i)))
	}
	if _, err := q.PushSync("br-sync", event("br-sync", "sync")); err != nil {
		t.Fatalf("PushSync: %v", err)
	}
	got := p.snapshot()
	if len(got) != 11 || got[10] != "br-sync/sync" {
		t.Fatalf("write order = %v; want the ten queued events then br-sync/sync", got)
	}

	wantErr := errors.New("log-store unreachable")
	p.mu.Lock()
	p.err = wantErr
	p.mu.Unlock()
	if _, err := q.PushSync("br-sync", event("br-sync", "boom")); !errors.Is(err, wantErr) {
		t.Fatalf("PushSync error = %v; want the real log-store error %v", err, wantErr)
	}
}

// Close is what makes a finished session complete in log-store. It must
// write everything still queued before it returns.
func TestLogStoreQueue_CloseDrainsBeforeReturning(t *testing.T) {
	p := &recordingPusher{delay: time.Millisecond}
	q := newLogStoreQueue(p.push)
	q.Open("br-close")

	const n = 30
	for i := 0; i < n; i++ {
		q.Enqueue("br-close", "event", event("br-close", fmt.Sprintf("t%03d", i)))
	}
	q.Close("br-close")

	if got := p.count(); got != n {
		t.Fatalf("after Close, log-store holds %d of %d events", got, n)
	}
}

// A session with no running pump has no queue. Those events still have to
// reach log-store — synchronously, exactly as before this change.
func TestLogStoreQueue_WritesSynchronouslyWithNoQueueOpen(t *testing.T) {
	p := &recordingPusher{}
	q := newLogStoreQueue(p.push)

	q.Enqueue("br-none", "event", event("br-none", "t000"))
	if got := p.snapshot(); len(got) != 1 || got[0] != "br-none/t000" {
		t.Fatalf("wrote %v; want the event pushed synchronously", got)
	}

	// Flush on an unknown session is a no-op, not a hang.
	done := make(chan struct{})
	go func() { q.Flush("br-none"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Flush blocked on a session with no queue")
	}
}

// slowLogStore wraps the in-memory stub with a fixed per-ingest delay, so
// a test can tell whether the SSE fan-out is waiting on the log-store
// POST or running ahead of it.
type slowLogStore struct {
	inner   http.Handler
	delay   time.Duration
	mu      sync.Mutex
	ingests int
}

func (s *slowLogStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/events" {
		time.Sleep(s.delay)
		s.mu.Lock()
		s.ingests++
		s.mu.Unlock()
	}
	s.inner.ServeHTTP(w, r)
}

func newSlowLogStoreManager(t *testing.T, delay time.Duration) (*Manager, *slowLogStore) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	slow := &slowLogStore{inner: newLogStoreStub(), delay: delay}
	ls := httptest.NewServer(slow)
	t.Cleanup(ls.Close)

	return NewManager(st, ls.URL, "http://127.0.0.1:0", "", 0, nil), slow
}

// The defect this change exists to remove: the log-store POST sat in
// front of the SSE fan-out, so every token delta the browser was waiting
// on paid an HTTP round trip first. With a log-store that takes 50ms per
// ingest, ten deltas used to cost the subscriber at least half a second.
func TestReadEventsFanOutDoesNotWaitOnLogStorePush(t *testing.T) {
	const ingestDelay = 50 * time.Millisecond
	const deltas = 10

	m, _ := newSlowLogStoreManager(t, ingestDelay)
	const bridgeID = "br-fanout-latency"
	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, deltas)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	start := time.Now()
	for i := 0; i < deltas; i++ {
		proc.ch <- msg.Event{
			Type:            msg.EventStream,
			BridgeSessionID: bridgeID,
			Harness:         msg.HarnessClaudeCode,
			TurnID:          "turn-1",
			Stream: &msg.HarnessStream{
				Delta: &msg.BlockDelta{Type: msg.DeltaText, Text: fmt.Sprintf("delta-%d", i)},
			},
		}
	}

	for i := 0; i < deltas; i++ {
		select {
		case <-sub:
		case <-time.After(10 * time.Second):
			t.Fatalf("subscriber received only %d of %d deltas", i, deltas)
		}
	}
	fanOut := time.Since(start)

	// Serialised behind the pushes this is at least deltas*ingestDelay =
	// 500ms. A quarter of that still leaves room for the ten SQLite
	// writes and the derivation the loop does per event, while failing
	// loudly if the push ever moves back in front of the fan-out.
	if budget := deltas * ingestDelay / 4; fanOut > budget {
		t.Fatalf("fan-out of %d deltas took %v; budget %v — the log-store push is back in front of the fan-out",
			deltas, fanOut, budget)
	}

	// The events are queued, not skipped: the pump drains the queue on
	// exit, and only then closes its subscriber channels.
	close(proc.ch)
	drainUntilClosed(t, sub)
	if n := m.eventsInLogStore(bridgeID); n != deltas {
		t.Fatalf("log-store holds %d of %d deltas after the pump exited", n, deltas)
	}
}

// drainUntilClosed reads a subscriber channel until readEvents closes it,
// which it does only after draining the session's log-store write queue.
// Gives a test a definite "the pump is finished" point.
func drainUntilClosed(t *testing.T, sub chan StoredEvent) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-sub:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("pump never closed its subscriber channel")
		}
	}
}

// eventsInLogStore counts stream events log-store holds for the session.
// Test-only helper; reads through the same client the manager uses.
func (m *Manager) eventsInLogStore(bridgeID string) int {
	evs, err := m.logStore.ListEvents(bridgeID, 0, []string{"stream"})
	if err != nil {
		return 0
	}
	return len(evs)
}

// The read-back paths query log-store for what the pump just wrote. Now
// that the write is queued, a read that does not drain the queue first
// sees a session missing its most recent events. The subscriber receiving
// the user_message is the moment that race opens: fan-out now happens
// before the write, by design.
func TestInterruptedTurnSeesAnEventStillInTheWriteQueue(t *testing.T) {
	m, _ := newSlowLogStoreManager(t, 100*time.Millisecond)
	const bridgeID = "br-read-after-write"
	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 4)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)
	defer func() {
		close(proc.ch)
		drainUntilClosed(t, sub)
	}()

	proc.ch <- msg.Event{
		Type:            msg.EventUserMessage,
		BridgeSessionID: bridgeID,
		Harness:         msg.HarnessClaudeCode,
		TurnID:          "turn-1",
		Result:          &msg.ResultEvent{Text: "what is the capital of France"},
	}

	// Wait for the fan-out, which now runs ahead of the log-store write.
	select {
	case <-sub:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never saw the user_message")
	}

	turn, err := m.InterruptedTurn(bridgeID)
	if err != nil {
		t.Fatalf("InterruptedTurn: %v", err)
	}
	if turn == nil || turn.UserMessageText != "what is the capital of France" {
		t.Fatalf("InterruptedTurn = %+v; want the queued user_message — the read did not drain the write queue", turn)
	}
	if turn.ToolCallsAlreadyRun != 0 {
		t.Fatalf("ToolCallsAlreadyRun = %d; want 0 — this turn ran no tools", turn.ToolCallsAlreadyRun)
	}
}

// A wedged log-store must not hold a caller forever. Flush and Close both
// give up waiting after drainTimeout — the events are not abandoned, only
// the wait is, so the writer finishes them once log-store answers again.
func TestLogStoreQueue_FlushAndCloseGiveUpOnAWedgedLogStore(t *testing.T) {
	gate := make(chan struct{})
	p := &recordingPusher{gate: gate}
	q := newLogStoreQueue(p.push)
	q.drainTimeout = 50 * time.Millisecond
	q.Open("br-wedged")

	const n = 5
	for i := 0; i < n; i++ {
		q.Enqueue("br-wedged", "event", event("br-wedged", fmt.Sprintf("t%03d", i)))
	}

	flushed := make(chan struct{})
	go func() { q.Flush("br-wedged"); close(flushed) }()
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("Flush never gave up on a wedged log-store")
	}

	closed := make(chan struct{})
	go func() { q.Close("br-wedged"); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never gave up on a wedged log-store")
	}

	// Nothing was dropped: releasing log-store lets the writer finish.
	close(gate)
	deadline := time.After(5 * time.Second)
	for p.count() < n {
		select {
		case <-deadline:
			t.Fatalf("writer wrote %d of %d events after log-store recovered", p.count(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// retryingQueue is a queue whose retry schedule runs in test time: no real
// sleeping, a deadline measured in wall-clock but with a base delay of one
// microsecond so the schedule is exhausted in milliseconds.
func retryingQueue(p *recordingPusher, deadline time.Duration) (*logStoreQueue, *int) {
	q := newLogStoreQueue(p.push)
	q.retryBaseDelay = time.Microsecond
	q.retryMaxDelay = 10 * time.Microsecond
	q.retryDeadline = deadline
	sleeps := 0
	q.sleep = func(d time.Duration) {
		sleeps++
		time.Sleep(d)
	}
	return q, &sleeps
}

// timedOut stands in for the error net/http returns when Client.Timeout
// expires: a net.Error whose Timeout() is true.
type timedOut struct{}

func (timedOut) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (timedOut) Timeout() bool   { return true }
func (timedOut) Temporary() bool { return false }

// A log-store restart refuses connections for about a second. Every event
// pushed in that window used to be logged once and lost; now the writer
// waits it out and the event lands, once, in its place in the order.
func TestLogStoreQueue_RetriesAPushRefusedDuringARestart(t *testing.T) {
	p := &recordingPusher{err: errors.New("dial tcp: connect: connection refused"), failFirst: 3}
	q, sleeps := retryingQueue(p, time.Second)
	q.Open("br-restart")

	for i := 0; i < 5; i++ {
		q.Enqueue("br-restart", "event", event("br-restart", fmt.Sprintf("t%03d", i)))
	}
	q.Close("br-restart")

	got := p.snapshot()
	if len(got) != 5 {
		t.Fatalf("log-store holds %d of 5 events after a transient failure: %v", len(got), got)
	}
	for i, w := range got {
		if want := fmt.Sprintf("br-restart/t%03d", i); w != want {
			t.Fatalf("position %d: wrote %q; want %q — a retried event lost its place", i, w, want)
		}
	}
	if p.attemptCount() != 5+3 {
		t.Fatalf("push attempted %d times; want 8 (three refusals, then one per event)", p.attemptCount())
	}
	if *sleeps != 3 {
		t.Fatalf("writer slept %d times; want once per refused attempt (3)", *sleeps)
	}
}

// A 5xx is log-store answering that it stored nothing, so it is retried
// like a refused connection.
func TestLogStoreQueue_RetriesAServerError(t *testing.T) {
	p := &recordingPusher{
		err:       &logstore.StatusError{StatusCode: 500, Status: "500 Internal Server Error", Body: "store failed"},
		failFirst: 2,
	}
	q, _ := retryingQueue(p, time.Second)
	q.Open("br-5xx")
	q.Enqueue("br-5xx", "event", event("br-5xx", "t000"))
	q.Close("br-5xx")

	if got := p.snapshot(); len(got) != 1 {
		t.Fatalf("log-store holds %d events after two 500s; want the event, retried until stored", len(got))
	}
}

// A 4xx is log-store REJECTING the event. Re-sending the same body is
// rejected the same way, so the writer drops it after one attempt and the
// events behind it are not held up.
func TestLogStoreQueue_DropsARejectedEventAfterOneAttempt(t *testing.T) {
	p := &recordingPusher{
		err:       &logstore.StatusError{StatusCode: 400, Status: "400 Bad Request", Body: "missing bridge_session_id"},
		failFirst: 1,
	}
	q, sleeps := retryingQueue(p, time.Second)
	q.Open("br-4xx")
	q.Enqueue("br-4xx", "event", event("br-4xx", "rejected"))
	q.Enqueue("br-4xx", "event", event("br-4xx", "next"))
	q.Close("br-4xx")

	if got := p.snapshot(); len(got) != 1 || got[0] != "br-4xx/next" {
		t.Fatalf("wrote %v; want only the event behind the rejected one", got)
	}
	if p.attemptCount() != 2 {
		t.Fatalf("push attempted %d times; want 2 — a rejected event must not be retried", p.attemptCount())
	}
	if *sleeps != 0 {
		t.Fatalf("writer slept %d times on a rejection; want 0", *sleeps)
	}
}

// A timeout is the one transport failure that is NOT retried: log-store's
// ingest is a plain INSERT, so a request it stored before the client gave
// up would be stored again. Dropped after one attempt, and said so.
func TestLogStoreQueue_DoesNotRetryATimedOutPush(t *testing.T) {
	p := &recordingPusher{err: fmt.Errorf("post event: %w", timedOut{}), failFirst: 1}
	q, sleeps := retryingQueue(p, time.Second)
	q.Open("br-timeout")
	q.Enqueue("br-timeout", "event", event("br-timeout", "ambiguous"))
	q.Enqueue("br-timeout", "event", event("br-timeout", "next"))
	q.Close("br-timeout")

	if got := p.snapshot(); len(got) != 1 || got[0] != "br-timeout/next" {
		t.Fatalf("wrote %v; want only the event behind the timed-out one", got)
	}
	if p.attemptCount() != 2 || *sleeps != 0 {
		t.Fatalf("attempts=%d sleeps=%d; want 2 and 0 — a timed-out push must not be re-sent", p.attemptCount(), *sleeps)
	}
}

// When log-store stays down past the deadline the event is dropped rather
// than the writer wedged forever, and the writer goes on to the next one.
func TestLogStoreQueue_GivesUpAtTheDeadlineAndMovesOn(t *testing.T) {
	p := &recordingPusher{err: errors.New("dial tcp: connect: connection refused")}
	q, _ := retryingQueue(p, 20*time.Millisecond)
	q.Open("br-down")
	q.Enqueue("br-down", "event", event("br-down", "lost"))

	// Comes back up only after the first event's deadline has passed.
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	q.Enqueue("br-down", "event", event("br-down", "after"))
	q.Close("br-down")

	if got := p.snapshot(); len(got) != 1 || got[0] != "br-down/after" {
		t.Fatalf("wrote %v; want the deadline to drop the first event and the writer to carry on", got)
	}
	if p.attemptCount() < 3 {
		t.Fatalf("push attempted %d times; want several before the deadline gave up", p.attemptCount())
	}
}

// The synchronous fallback — a session with no queue — gets the same
// retry, so a teardown event on a session whose pump has gone does not
// vanish in a restart window either.
func TestLogStoreQueue_RetriesTheSynchronousFallbackToo(t *testing.T) {
	p := &recordingPusher{err: errors.New("connection refused"), failFirst: 2}
	q, _ := retryingQueue(p, time.Second)
	q.Enqueue("br-none", "hook event", event("br-none", "t000"))
	if got := p.snapshot(); len(got) != 1 {
		t.Fatalf("wrote %v; want the event pushed after two refusals", got)
	}
}

// Once a push has exhausted the deadline, log-store is down rather than
// restarting, and every live chat's pump would otherwise stall for a full
// deadline per event. From then on each push gets one attempt until one
// succeeds; then the retry is back.
func TestLogStoreQueue_StopsRetryingWhileLogStoreStaysDown(t *testing.T) {
	p := &recordingPusher{err: errors.New("dial tcp: connect: connection refused")}
	q, sleeps := retryingQueue(p, 20*time.Millisecond)
	q.Open("br-dead")

	q.Enqueue("br-dead", "event", event("br-dead", "exhausts-the-deadline"))
	q.Flush("br-dead")
	if q.downSince.Load() == 0 {
		t.Fatal("hitting the deadline did not mark log-store down")
	}
	attemptsBefore, sleepsBefore := p.attemptCount(), *sleeps

	q.Enqueue("br-dead", "event", event("br-dead", "while-down"))
	q.Flush("br-dead")
	if got := p.attemptCount() - attemptsBefore; got != 1 {
		t.Fatalf("a push while log-store is down was attempted %d times; want exactly 1", got)
	}
	if *sleeps != sleepsBefore {
		t.Fatal("a push while log-store is down slept between attempts; it must not wait at all")
	}

	// Back up: the next success clears the mark, and the retry is back for
	// the failure after that.
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	q.Enqueue("br-dead", "event", event("br-dead", "back"))
	q.Flush("br-dead")
	if q.downSince.Load() != 0 {
		t.Fatal("a successful push did not mark log-store up again")
	}
	p.mu.Lock()
	p.err = errors.New("dial tcp: connect: connection refused")
	p.failFirst = p.attempts + 2
	p.mu.Unlock()
	q.Enqueue("br-dead", "event", event("br-dead", "retried-again"))
	q.Close("br-dead")

	if got := p.snapshot(); len(got) != 2 || got[0] != "br-dead/back" || got[1] != "br-dead/retried-again" {
		t.Fatalf("wrote %v; want the two events pushed after log-store came back, the second after two refusals", got)
	}
}
