package harness

// A benchmark of the path every session event takes: a harness's frame handed
// to the manager, stored, and sent to an SSE subscriber; and of the reads that
// serve the stream, the reading page, crash recovery and the reaper.
//
// It is a benchmark, not a test, so it runs only when asked for:
//
//	export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"
//	go test ./internal/harness -run '^$' -bench BenchmarkSessionEventPaths -benchtime 1x -timeout 60m
//
// Each path is measured on a SQLite file in the test's temporary folder and a
// database of its own on that Postgres. The shared test container keeps its
// data in memory with fsync and synchronous_commit off; point
// POSTGRESTEST_ADMINISTRATOR_URL at a Postgres with the default durability on
// the same disk as the SQLite file to compare like with like.
//
// The event mix, sizes and pacing come from the live bridge database, read
// only, on 2026-11-25: the 232,047 events of Claude Code sessions among its
// newest 300,000. Only sizes and counts were read; every body here is made up.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// eventSizeQuantiles is the size in bytes of one stored event of a type, at
// the 50th, 90th and 99th percentile and the largest seen.
type eventSizeQuantiles struct {
	p50, p90, p99, maximum int
}

// benchmarkEventSizes is measured from the live bridge database (see the
// header). The largest tool result seen was 2 MB.
var benchmarkEventSizes = map[msg.EventType]eventSizeQuantiles{
	msg.EventSystem:      {481, 977, 5144, 42155},
	msg.EventToolResult:  {1028, 21553, 1040075, 2041045},
	msg.EventToolCall:    {2162, 6959, 32106, 134711},
	msg.EventAPICall:     {512, 531, 555, 560},
	msg.EventBlock:       {3894, 7968, 26544, 130395},
	msg.EventResult:      {2106, 9148, 31063, 83575},
	msg.EventUserMessage: {926, 5857, 27976, 31750},
	msg.EventSessionInfo: {3470, 3629, 3676, 6968},
}

// benchmarkTurnMix is how many of each harness event a turn holds, per 1,000,
// from the same sample: 42,555 system, 29,145 tool results, 16,217 tool calls,
// 11,873 API calls, 10,653 blocks and 906 session infos over 1,003 turns.
// The derived events (statuses, states, costs, totals) are not here: the
// manager derives them, as it does live.
var benchmarkTurnMix = []struct {
	eventType msg.EventType
	perMille  int
}{
	{msg.EventSystem, 381},
	{msg.EventToolResult, 261},
	{msg.EventToolCall, 145},
	{msg.EventAPICall, 106},
	{msg.EventBlock, 95},
	{msg.EventSessionInfo, 8},
}

// benchmarkEventsPerTurn is the mean number of harness events in one turn of
// the sample: 115 (111,349 events over 1,003 user messages), the result
// included.
const benchmarkEventsPerTurn = 115

// A busy Claude Code turn arrives in bursts. The busiest second in the sample
// held 275 events of one session and the busiest minute 858; so 70% of the
// gaps here are 4 ms (250 a second) and the rest 60 ms, about 48 events a
// second on average, a busy turn sustained.
const (
	benchmarkBurstGap        = 4 * time.Millisecond
	benchmarkCalmGap         = 60 * time.Millisecond
	benchmarkBurstGapPercent = 70
)

// sizeFromQuantiles draws a size: half up to the median, 40% to the 90th
// percentile, 9% to the 99th and 1% above it, uniform within each band.
func sizeFromQuantiles(random *rand.Rand, quantiles eventSizeQuantiles) int {
	between := func(low, high int) int {
		if high <= low {
			return low
		}
		return low + random.Intn(high-low)
	}
	switch draw := random.Intn(100); {
	case draw < 50:
		return between(quantiles.p50/2, quantiles.p50)
	case draw < 90:
		return between(quantiles.p50, quantiles.p90)
	case draw < 99:
		return between(quantiles.p90, quantiles.p99)
	default:
		return between(quantiles.p99, quantiles.maximum)
	}
}

// benchmarkEventFactory makes made-up events of a session with real sizes.
type benchmarkEventFactory struct {
	random    *rand.Rand
	sessionID string
	turn      int
	toolCall  int
	sequence  int
}

// benchmarkRaw is the raw source line every event carries, as a harness's
// events do. sequence identifies the event to the subscriber that times it.
type benchmarkRaw struct {
	Sequence int    `json:"benchmark_sequence"`
	Line     string `json:"line"`
}

func sequenceOfBenchmarkEvent(ev *msg.Event) (int, bool) {
	if len(ev.Raw) == 0 {
		return 0, false
	}
	var raw benchmarkRaw
	if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw.Sequence == 0 {
		return 0, false
	}
	return raw.Sequence, true
}

// padTo fills the event's raw line until the event marshals to about size bytes.
func (f *benchmarkEventFactory) padTo(ev *msg.Event, size int) {
	f.sequence++
	ev.Raw, _ = json.Marshal(benchmarkRaw{Sequence: f.sequence})
	data, _ := json.Marshal(ev)
	if missing := size - len(data); missing > 0 {
		ev.Raw, _ = json.Marshal(benchmarkRaw{Sequence: f.sequence, Line: strings.Repeat("x", missing)})
	}
}

func (f *benchmarkEventFactory) userMessage() msg.Event {
	f.turn++
	f.toolCall = 0
	ev := msg.Event{
		Type: msg.EventUserMessage, Harness: msg.HarnessClaudeCode, BridgeSessionID: f.sessionID,
		TurnID: fmt.Sprintf("turn-%s-%d", f.sessionID, f.turn), Timestamp: time.Now(),
		Result: &msg.ResultEvent{Text: fmt.Sprintf("made-up request %d", f.turn)},
	}
	f.padTo(&ev, sizeFromQuantiles(f.random, benchmarkEventSizes[msg.EventUserMessage]))
	return ev
}

// harnessEvent makes the next event of a turn; last makes its result.
func (f *benchmarkEventFactory) harnessEvent(last bool) msg.Event {
	ev := msg.Event{Harness: msg.HarnessClaudeCode, BridgeSessionID: f.sessionID, Timestamp: time.Now(),
		TurnID: fmt.Sprintf("turn-%s-%d", f.sessionID, f.turn)}
	eventType := msg.EventResult
	if !last {
		draw := f.random.Intn(1000)
		eventType = benchmarkTurnMix[len(benchmarkTurnMix)-1].eventType
		for _, entry := range benchmarkTurnMix {
			if draw < entry.perMille {
				eventType = entry.eventType
				break
			}
			draw -= entry.perMille
		}
	}
	ev.Type = eventType
	messageID := fmt.Sprintf("harness-message-%d-%d", f.turn, f.toolCall/4)
	switch eventType {
	case msg.EventSystem:
		ev.System = &msg.SystemEvent{Subtype: "thinking_tokens", Message: "made up"}
	case msg.EventToolCall:
		f.toolCall++
		ev.HarnessMessageID = messageID
		ev.ToolCall = &msg.ToolCallEvent{ToolID: fmt.Sprintf("tool-%d-%d", f.turn, f.toolCall), Name: "Bash", Input: json.RawMessage(`{"command":"true"}`)}
	case msg.EventToolResult:
		ev.HarnessMessageID = messageID
		ev.ToolResult = &msg.ToolResultEvent{ToolID: fmt.Sprintf("tool-%d-%d", f.turn, f.toolCall), Name: "Bash", Output: "made-up output"}
	case msg.EventAPICall:
		ev.APICall = &msg.APICallEvent{Model: "made-up-model", InputTokens: 1200, OutputTokens: 300, CacheReadTokens: 40000, CostUSD: 0.01, QuerySource: "repl_main_thread"}
	case msg.EventBlock:
		ev.HarnessMessageID = messageID
		ev.Block = &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "made-up assistant text"}}}
	case msg.EventSessionInfo:
		ev.Info = &msg.SessionInfo{Model: "made-up-model", WorkingDir: "/work/made-up"}
	case msg.EventResult:
		ev.Result = &msg.ResultEvent{Text: "made-up answer", Usage: msg.TokenUsage{InputTokens: 9000, OutputTokens: 2000}, DurationMS: 30000, Model: "made-up-model"}
	}
	f.padTo(&ev, sizeFromQuantiles(f.random, benchmarkEventSizes[eventType]))
	return ev
}

// sessionEventReads is every read of a session's events this benchmark times.
// The bridge database's store answers all of them.
type sessionEventReads interface {
	MaxEventRowID(sessionID string) (int64, error)
	ListEventsSinceID(sessionID string, afterID int) ([]store.EventWithID, error)
	ListCurrentTurnEventsWithIDs(sessionID string) ([]store.EventWithID, error)
	LastActivityAt(sessionID string) (time.Time, error)
	CountUserTurns(sessionID string) (int, error)
	CurrentTurnText(sessionID string) (string, []string, error)
	LastReportedBackgroundTasks(sessionID string) ([]msg.BackgroundTask, error)
	LatestScheduledWakeup(sessionID string, callsLoggedSince time.Time) (*store.ScheduledWakeup, error)
}

// benchmarkBench is one path built on fresh stores.
type benchmarkBench struct {
	manager      *Manager
	bridgeStore  *store.Store
	reads        sessionEventReads
	sessionLog   *logstoreserver.Server
	databasePath string
	// restart makes a new manager over the same stores, as a restarted
	// server would have.
	restart func() *Manager
}

// sessionEventPath is one way of keeping session events, built fresh.
type sessionEventPath struct {
	name  string
	build func(b *testing.B) *benchmarkBench
}

// openBenchmarkSessionLog opens the session_log module on a database of its
// own, its writer with synchronous_commit off when asynchronousCommit.
func openBenchmarkSessionLog(b *testing.B, asynchronousCommit bool) (*logstorestore.Store, *logstoreserver.Server) {
	module, err := logstorestore.Module()
	if err != nil {
		b.Fatal(err)
	}
	database := postgrestest.NewDatabase(b, module)
	writerURL, err := logstorestore.WriterURL(database.URL(), asynchronousCommit)
	if err != nil {
		b.Fatal(err)
	}
	writer, err := postgresstorage.Open(context.Background(), writerURL, module)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { writer.Close() })
	reader, err := logstorestore.OpenReader(context.Background(), database.URL(), module)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { reader.Close() })
	logStore := logstorestore.New(writer, reader)
	return logStore, logstoreserver.New(logStore, nil)
}

// bridgeDatabasePath is the path as it is today: every event stored in the
// bridge database's events table, which serves the stream, and queued to
// session_log, which serves the reading page.
var bridgeDatabasePath = sessionEventPath{
	name: "bridge_database",
	build: func(b *testing.B) *benchmarkBench {
		databasePath := filepath.Join(b.TempDir(), "bridge.db")
		bridgeStore, err := store.New(databasePath)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { bridgeStore.Close() })
		_, sessionLog := openBenchmarkSessionLog(b, false)
		newManager := func() *Manager {
			m := NewManager(bridgeStore, "http://127.0.0.1:0", "", 0, nil)
			m.SetSessionEventLog(sessionLog)
			return m
		}
		return &benchmarkBench{manager: newManager(), bridgeStore: bridgeStore, reads: bridgeStore, sessionLog: sessionLog, databasePath: databasePath, restart: newManager}
	},
}

// singleCopyPathWith is the merged path: session_log is the only copy,
// written once per event in order, and serves every read.
func singleCopyPathWith(name string, asynchronousCommit bool) sessionEventPath {
	return sessionEventPath{
		name: name,
		build: func(b *testing.B) *benchmarkBench {
			databasePath := filepath.Join(b.TempDir(), "bridge.db")
			bridgeStore, err := store.New(databasePath)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { bridgeStore.Close() })
			_, sessionLog := openBenchmarkSessionLog(b, asynchronousCommit)
			singleCopy, err := sessionevents.OpenSingleCopy(context.Background(), bridgeStore, sessionLog)
			if err != nil {
				b.Fatal(err)
			}
			newManager := func() *Manager {
				m := NewManager(bridgeStore, "http://127.0.0.1:0", "", 0, nil)
				m.SetSessionEventLog(sessionLog)
				m.SetSessionEventStore(singleCopy, true)
				return m
			}
			return &benchmarkBench{manager: newManager(), bridgeStore: bridgeStore, reads: singleCopy, sessionLog: sessionLog, databasePath: databasePath, restart: newManager}
		},
	}
}

// benchmarkSessionEventPaths lists the paths compared.
var benchmarkSessionEventPaths = []sessionEventPath{
	bridgeDatabasePath,
	singleCopyPathWith("single_copy", false),
	singleCopyPathWith("single_copy_asynchronous_commit", true),
}

// latencies collects durations and reports their quantiles.
type latencies struct {
	mu     sync.Mutex
	values []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.values = append(l.values, d)
	l.mu.Unlock()
}

func (l *latencies) quantile(q float64) time.Duration {
	if len(l.values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(q * float64(len(sorted)-1))
	return sorted[index]
}

func (l *latencies) maximum() time.Duration { return l.quantile(1) }

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// benchmarkResults prints one line per measurement, in a form that pastes
// into a table: path, measurement, then its numbers.
type benchmarkResults struct {
	mu    sync.Mutex
	lines []string
}

func (r *benchmarkResults) record(b *testing.B, path, measurement, numbers string) {
	line := fmt.Sprintf("| %s | %s | %s |", path, measurement, numbers)
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	fmt.Fprintln(os.Stdout, line)
}

func (r *benchmarkResults) recordLatencies(b *testing.B, path, measurement string, values *latencies, extra string) {
	numbers := fmt.Sprintf("n=%d p50=%.2fms p99=%.2fms max=%.2fms", len(values.values), milliseconds(values.quantile(0.5)), milliseconds(values.quantile(0.99)), milliseconds(values.maximum()))
	if extra != "" {
		numbers += " " + extra
	}
	r.record(b, path, measurement, numbers)
}

func createBenchmarkSession(b *testing.B, st *store.Store, sessionID string) {
	if err := st.CreateSession(&store.Session{SessionID: sessionID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		b.Fatalf("create session %s: %v", sessionID, err)
	}
}

// sessionDriver plays a made-up harness for one session: it opens a turn
// through BroadcastEvent as /send does, hands the turn's frames to the
// manager's event pump, and times each frame until its subscriber has it.
type sessionDriver struct {
	sessionID  string
	process    *fakeProcess
	subscriber chan StoredEvent
	factory    *benchmarkEventFactory
	sentAt     sync.Map // sequence → time.Time
	received   chan int
	ingest     *latencies
	send       *latencies
	done       chan struct{}
}

func startSessionDriver(b *testing.B, m *Manager, st *store.Store, sessionID string, seed int64, ingest, send *latencies) *sessionDriver {
	createBenchmarkSession(b, st, sessionID)
	driver := &sessionDriver{
		sessionID:  sessionID,
		process:    &fakeProcess{sid: sessionID, ch: make(chan msg.Event, 4096)},
		subscriber: m.Subscribe(sessionID),
		factory:    &benchmarkEventFactory{random: rand.New(rand.NewSource(seed)), sessionID: sessionID},
		received:   make(chan int, 1<<16),
		ingest:     ingest,
		send:       send,
		done:       make(chan struct{}),
	}
	go func() {
		defer close(driver.done)
		for stored := range driver.subscriber {
			sequence, ok := sequenceOfBenchmarkEvent(&stored.Event)
			if !ok {
				continue
			}
			if sent, ok := driver.sentAt.LoadAndDelete(sequence); ok {
				if stored.Type != msg.EventUserMessage {
					driver.ingest.add(time.Since(sent.(time.Time)))
				}
				driver.received <- sequence
			}
		}
	}()
	go m.readEvents(driver.process)
	return driver
}

// runTurns plays turns turns. paced waits the measured gaps between frames;
// unpaced hands them over as fast as the pump takes them. Each turn's next
// user message waits until the subscriber has had the turn before it, as a
// person's next message follows the answer.
func (d *sessionDriver) runTurns(b *testing.B, m *Manager, turns int, paced bool) {
	random := rand.New(rand.NewSource(int64(len(d.sessionID)) + int64(turns)))
	for turn := 0; turn < turns; turn++ {
		user := d.factory.userMessage()
		sequence, _ := sequenceOfBenchmarkEvent(&user)
		d.sentAt.Store(sequence, time.Now())
		start := time.Now()
		if _, err := m.BroadcastEvent(&user); err != nil {
			b.Errorf("send the user message of %s: %v", d.sessionID, err)
		}
		d.send.add(time.Since(start))
		sent := 1
		for i := 0; i < benchmarkEventsPerTurn; i++ {
			ev := d.factory.harnessEvent(i == benchmarkEventsPerTurn-1)
			sequence, _ := sequenceOfBenchmarkEvent(&ev)
			if paced {
				if random.Intn(100) < benchmarkBurstGapPercent {
					time.Sleep(benchmarkBurstGap)
				} else {
					time.Sleep(benchmarkCalmGap)
				}
			}
			d.sentAt.Store(sequence, time.Now())
			d.process.ch <- ev
			sent++
		}
		d.waitFor(b, sent)
	}
}

// waitFor waits until the subscriber has had count events.
func (d *sessionDriver) waitFor(b *testing.B, count int) {
	deadline := time.After(10 * time.Minute)
	for i := 0; i < count; i++ {
		select {
		case <-d.received:
		case <-deadline:
			b.Fatalf("%s: the subscriber had %d of %d events after 10 minutes", d.sessionID, i, count)
		}
	}
}

func (d *sessionDriver) stop(m *Manager) {
	close(d.process.ch)
	<-d.done
}

// BenchmarkSessionEventPaths measures every path in benchmarkSessionEventPaths.
// Run it with -benchtime 1x: each part is a fixed workload, not b.N rounds.
func BenchmarkSessionEventPaths(b *testing.B) {
	results := &benchmarkResults{}
	for _, path := range benchmarkSessionEventPaths {
		for _, sessions := range []int{1, 5, 20} {
			b.Run(fmt.Sprintf("%s/ingest_paced_%d_sessions", path.name, sessions), func(b *testing.B) {
				benchmarkPacedIngest(b, results, path, sessions)
			})
		}
		for _, sessions := range []int{1, 20} {
			b.Run(fmt.Sprintf("%s/ingest_unpaced_%d_sessions", path.name, sessions), func(b *testing.B) {
				benchmarkUnpacedIngest(b, results, path, sessions)
			})
		}
		b.Run(path.name+"/reads", func(b *testing.B) { benchmarkReads(b, results, path) })
		b.Run(path.name+"/crash_recovery_20_sessions", func(b *testing.B) { benchmarkCrashRecovery(b, results, path) })
	}
	fmt.Fprintln(os.Stdout, "\n| path | measurement | numbers |\n|---|---|---|")
	for _, line := range results.lines {
		fmt.Fprintln(os.Stdout, line)
	}
}

// pacedTurnsPerSession keeps each run near 1,000 timed frames or more.
func pacedTurnsPerSession(sessions int) int {
	switch {
	case sessions == 1:
		return 10
	case sessions <= 5:
		return 4
	default:
		return 3
	}
}

func benchmarkPacedIngest(b *testing.B, results *benchmarkResults, path sessionEventPath, sessions int) {
	bench := path.build(b)
	ingest, send := &latencies{}, &latencies{}
	drivers := make([]*sessionDriver, sessions)
	for i := range drivers {
		drivers[i] = startSessionDriver(b, bench.manager, bench.bridgeStore, fmt.Sprintf("br_benchmark_paced_%d", i), int64(i+1), ingest, send)
	}
	start := time.Now()
	var wait sync.WaitGroup
	for _, driver := range drivers {
		wait.Add(1)
		go func(driver *sessionDriver) {
			defer wait.Done()
			driver.runTurns(b, bench.manager, pacedTurnsPerSession(sessions), true)
		}(driver)
	}
	wait.Wait()
	elapsed := time.Since(start)
	// What the stream has is not yet all in session_log on the bridge
	// database path: its queue writes behind. Time until it is.
	drainStart := time.Now()
	for _, driver := range drivers {
		bench.manager.FlushLogStoreWrites(driver.sessionID)
	}
	drained := time.Since(drainStart)
	for _, driver := range drivers {
		driver.stop(bench.manager)
	}
	frames := len(ingest.values)
	results.recordLatencies(b, path.name, fmt.Sprintf("ingest, paced, %d sessions", sessions), ingest,
		fmt.Sprintf("rate=%.0f/s session_log_behind_by=%.0fms", float64(frames)/elapsed.Seconds(), milliseconds(drained)))
	results.recordLatencies(b, path.name, fmt.Sprintf("send (user message), paced, %d sessions", sessions), send, "")
	b.ReportMetric(milliseconds(ingest.quantile(0.99)), "p99_ms")
}

func benchmarkUnpacedIngest(b *testing.B, results *benchmarkResults, path sessionEventPath, sessions int) {
	bench := path.build(b)
	ingest, send := &latencies{}, &latencies{}
	turns := 20 / sessions
	if turns < 2 {
		turns = 2
	}
	drivers := make([]*sessionDriver, sessions)
	for i := range drivers {
		drivers[i] = startSessionDriver(b, bench.manager, bench.bridgeStore, fmt.Sprintf("br_benchmark_unpaced_%d", i), int64(100+i), ingest, send)
	}
	start := time.Now()
	var wait sync.WaitGroup
	for _, driver := range drivers {
		wait.Add(1)
		go func(driver *sessionDriver) {
			defer wait.Done()
			driver.runTurns(b, bench.manager, turns, false)
		}(driver)
	}
	wait.Wait()
	elapsed := time.Since(start)
	for _, driver := range drivers {
		bench.manager.FlushLogStoreWrites(driver.sessionID)
	}
	drainedElapsed := time.Since(start)
	for _, driver := range drivers {
		driver.stop(bench.manager)
	}
	frames := len(ingest.values)
	results.record(b, path.name, fmt.Sprintf("ingest, unpaced, %d sessions", sessions),
		fmt.Sprintf("frames=%d streamed=%.0f/s stored_in_session_log=%.0f/s", frames, float64(frames)/elapsed.Seconds(), float64(frames)/drainedElapsed.Seconds()))
}

// populateSession writes turns turns of a session through the manager,
// unpaced. The last turn is left open when leaveLastTurnOpen.
func populateSession(b *testing.B, bench *benchmarkBench, sessionID string, turns int, seed int64, leaveLastTurnOpen bool) {
	ingest, send := &latencies{}, &latencies{}
	driver := startSessionDriver(b, bench.manager, bench.bridgeStore, sessionID, seed, ingest, send)
	sent := 0
	for turn := 0; turn < turns; turn++ {
		if leaveLastTurnOpen && turn == turns-1 {
			user := driver.factory.userMessage()
			if _, err := bench.manager.BroadcastEvent(&user); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < 40; i++ {
				ev := driver.factory.harnessEvent(false)
				sequence, _ := sequenceOfBenchmarkEvent(&ev)
				driver.sentAt.Store(sequence, time.Now())
				driver.process.ch <- ev
				sent++
			}
			continue
		}
		driver.runTurns(b, bench.manager, 1, false)
	}
	driver.waitFor(b, sent)
	bench.manager.FlushLogStoreWrites(sessionID)
	driver.stop(bench.manager)
}

// timeRead runs read repetitions times and records its latencies.
func timeRead(b *testing.B, results *benchmarkResults, path, measurement string, repetitions int, read func() error) {
	values := &latencies{}
	for i := 0; i < repetitions; i++ {
		start := time.Now()
		if err := read(); err != nil {
			b.Fatalf("%s: %v", measurement, err)
		}
		values.add(time.Since(start))
	}
	results.recordLatencies(b, path, measurement, values, "")
}

// readingPage asks session_log for a session's reading page, after the
// stream head, as GET /sessions/{id}/messages does.
func readingPage(bench *benchmarkBench, sessionID string) error {
	if _, err := bench.reads.MaxEventRowID(sessionID); err != nil {
		return err
	}
	bench.manager.FlushLogStoreWrites(sessionID)
	recorder := httptest.NewRecorder()
	bench.sessionLog.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages?limit=30", nil))
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("messages answered %d: %s", recorder.Code, recorder.Body.String())
	}
	return nil
}

const (
	smallSessionTurns = 1
	// largeSessionTurns makes about 13,800 events, the size of the largest
	// real session's 13,776.
	largeSessionTurns    = 120
	activityReadSessions = 50
)

func benchmarkReads(b *testing.B, results *benchmarkResults, path sessionEventPath) {
	bench := path.build(b)
	const small, large = "br_benchmark_small", "br_benchmark_large"
	populateSession(b, bench, small, smallSessionTurns, 7, false)
	populateStart := time.Now()
	populateSession(b, bench, large, largeSessionTurns, 8, true)
	results.record(b, path.name, "populate the large session", fmt.Sprintf("%.1fs", time.Since(populateStart).Seconds()))
	for i := 0; i < activityReadSessions; i++ {
		populateSession(b, bench, fmt.Sprintf("br_benchmark_activity_%d", i), 1, int64(200+i), false)
	}

	head, err := bench.reads.MaxEventRowID(large)
	if err != nil {
		b.Fatal(err)
	}
	replay, err := bench.reads.ListEventsSinceID(large, int(head)-30)
	if err != nil || len(replay) == 0 {
		b.Fatalf("replay read: %d events, %v", len(replay), err)
	}
	timeRead(b, results, path.name, "SSE replay, Last-Event-ID 30 events back, large session", 50, func() error {
		_, err := bench.reads.ListEventsSinceID(large, int(head)-30)
		return err
	})
	timeRead(b, results, path.name, "SSE replay, current turn on connect, large session", 50, func() error {
		_, err := bench.reads.ListCurrentTurnEventsWithIDs(large)
		return err
	})
	for _, session := range []struct{ name, id string }{{"small", small}, {"large", large}} {
		cold := time.Now()
		if err := readingPage(bench, session.id); err != nil {
			b.Fatal(err)
		}
		results.record(b, path.name, "GET messages limit=30, first read, "+session.name+" session", fmt.Sprintf("%.2fms", milliseconds(time.Since(cold))))
		timeRead(b, results, path.name, "GET messages limit=30, "+session.name+" session", 20, func() error {
			return readingPage(bench, session.id)
		})
	}
	timeRead(b, results, path.name, fmt.Sprintf("last activity and wakeup of %d sessions (reaper tick)", activityReadSessions), 20, func() error {
		for i := 0; i < activityReadSessions; i++ {
			if _, err := bench.reads.LastActivityAt(fmt.Sprintf("br_benchmark_activity_%d", i)); err != nil {
				return err
			}
			if _, err := bench.reads.LatestScheduledWakeup(fmt.Sprintf("br_benchmark_activity_%d", i), time.Now().Add(-time.Hour)); err != nil {
				return err
			}
		}
		return nil
	})
	timeRead(b, results, path.name, fmt.Sprintf("stream heads of %d sessions (recent bundle)", activityReadSessions), 20, func() error {
		for i := 0; i < activityReadSessions; i++ {
			if _, err := bench.reads.MaxEventRowID(fmt.Sprintf("br_benchmark_activity_%d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	timeRead(b, results, path.name, "reaper reads (last activity, wakeup), large session", 20, func() error {
		if _, err := bench.reads.LastActivityAt(large); err != nil {
			return err
		}
		_, err := bench.reads.LatestScheduledWakeup(large, time.Now().Add(-time.Hour))
		return err
	})
	timeRead(b, results, path.name, "message id maps on spawn (loadMsgState), large session", 10, func() error {
		bench.manager.mu.Lock()
		defer bench.manager.mu.Unlock()
		bench.manager.loadMsgState(large)
		return nil
	})
	timeRead(b, results, path.name, "background tasks (restart readiness), large session", 20, func() error {
		_, err := bench.reads.LastReportedBackgroundTasks(large)
		return err
	})
	timeRead(b, results, path.name, "renamer turn count, large session", 20, func() error {
		_, err := bench.reads.CountUserTurns(large)
		return err
	})
	timeRead(b, results, path.name, "brief pass current-turn text, large session", 20, func() error {
		_, _, err := bench.reads.CurrentTurnText(large)
		return err
	})
}

// benchmarkCrashRecovery leaves 20 sessions mid-turn, then times what a
// restarted server reads to settle them: the stale-state reset and, per
// session, its last activity, its interrupted turn, its background tasks and
// its message ids.
func benchmarkCrashRecovery(b *testing.B, results *benchmarkResults, path sessionEventPath) {
	bench := path.build(b)
	const sessions = 20
	ids := make([]string, sessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("br_benchmark_crashed_%d", i)
		populateSession(b, bench, ids[i], 3, int64(300+i), true)
	}
	restarted := bench.restart()
	start := time.Now()
	reconciled, err := restarted.ReconcileStaleSessions()
	if err != nil {
		b.Fatal(err)
	}
	reset := time.Since(start)
	for _, id := range ids {
		if _, err := bench.reads.LastActivityAt(id); err != nil {
			b.Fatal(err)
		}
		turn, err := restarted.InterruptedTurn(id)
		if err != nil || turn == nil {
			b.Fatalf("%s: interrupted turn %v, %v", id, turn, err)
		}
		if _, err := bench.reads.LastReportedBackgroundTasks(id); err != nil {
			b.Fatal(err)
		}
		restarted.mu.Lock()
		restarted.loadMsgState(id)
		restarted.mu.Unlock()
	}
	results.record(b, path.name, "crash recovery at start, 20 sessions mid-turn",
		fmt.Sprintf("reset %d sessions in %.1fms, all reads %.1fms", len(reconciled), milliseconds(reset), milliseconds(time.Since(start))))
}
