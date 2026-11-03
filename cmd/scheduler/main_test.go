// Tests for cmd/scheduler, the cron daemon itself.
//
// This binary is wiring, and every assertion here is about a boot decision:
// which failures stop a boot and which are only a warning, what a failed bus
// connection costs, what runs before the first job can tick, and what is torn
// down on the way out.
//
// Three rules govern the assertions, all inherited from scoring earlier suites
// against deliberate sabotage rather than from reading them:
//
//   - Never reproduce a gate in the fixture. Where a value is a contract with
//     another program — the port every client on this box spells out, the
//     database path, the bus URL — the assertion writes the wire literal.
//     Comparing the code's constant against itself is green through any drift
//     of that constant.
//
//   - Never pass the value under test in from the test. The defaults are
//     internal/config's, and its own tests pin them.
//
//   - Never assert a bare error. runScheduler has five failure sites and they
//     all return a non-nil error, so `err != nil` pins the signature and not the
//     behavior. Each failure test also asserts the discriminating side effect:
//     the handler that must not be built, the port that must not be bound, the
//     connection that must not be closed twice.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/slava/scheduler/internal/config"
	cronpkg "github.com/slava/scheduler/internal/cron"
	"github.com/slava/scheduler/internal/db"
	"github.com/slava/scheduler/internal/executor"
	"github.com/slava/scheduler/internal/logging"
	"github.com/slava/scheduler/internal/runstatus"

	_ "modernc.org/sqlite"
)

// ------------------------------------------------ reconciled runs, reported

type finishEntry struct {
	jobID    int64
	jobName  string
	schedule string
	runID    int64
	status   string
	output   string
	duration time.Duration
}

type recordingLogger struct{ finishes []finishEntry }

func (l *recordingLogger) LogJobFinish(jobID int64, jobName, schedule string, runID int64, status, output string, duration time.Duration) {
	l.finishes = append(l.finishes, finishEntry{jobID, jobName, schedule, runID, status, output, duration})
}

func started(minutesAgo int) time.Time {
	return time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute)
}

// Startup reconciliation does not run through the cron loop, so nothing else
// can report the runs it closes. Before this, they were corrected in SQLite and
// published nowhere: a deploy restart that killed a job mid-flight was visible
// only to somebody reading /api/jobs/<id>/runs by hand.
func TestPublishInterruptedRunsReportsEveryClosedRun(t *testing.T) {
	finishedAt := started(0)
	runs := []db.InterruptedRun{
		{
			RunID: 41, JobID: 7, JobName: "nightly-guard", Schedule: "0 3 * * *",
			Output: db.InterruptedRunOutput, StartedAt: started(90), FinishedAt: finishedAt,
		},
		{
			RunID: 42, JobID: 9, JobName: "every-five", Schedule: "*/5 * * * *",
			Output: "half a log line", StartedAt: started(3), FinishedAt: finishedAt,
		},
	}

	logger := &recordingLogger{}
	publishInterruptedRuns(logger, runs)

	// One entry per run, not one summary. A consumer counting finishes has to
	// see these the way it sees every other finish.
	if len(logger.finishes) != 2 {
		t.Fatalf("published %d finishes for 2 closed runs, want 2", len(logger.finishes))
	}

	first := logger.finishes[0]
	if first.runID != 41 || first.jobID != 7 || first.jobName != "nightly-guard" || first.schedule != "0 3 * * *" {
		t.Errorf("first finish reported run %d job %d %q %q, want run 41 job 7 %q %q",
			first.runID, first.jobID, first.jobName, first.schedule, "nightly-guard", "0 3 * * *")
	}
	if first.output != db.InterruptedRunOutput {
		t.Errorf("first finish published output %q, want the stored explanation %q",
			first.output, db.InterruptedRunOutput)
	}
	// Duration is start until reconciliation noticed. The moment the process
	// actually died is unrecorded, so this is the only honest number available.
	if first.duration != 90*time.Minute {
		t.Errorf("first finish reported duration %v, want %v", first.duration, 90*time.Minute)
	}

	// A second run with different values, so the loop cannot pass by publishing
	// the first run twice.
	second := logger.finishes[1]
	if second.runID != 42 || second.jobID != 9 || second.jobName != "every-five" {
		t.Errorf("second finish reported run %d job %d %q, want run 42 job 9 %q",
			second.runID, second.jobID, second.jobName, "every-five")
	}
	if second.output != "half a log line" {
		t.Errorf("second finish published output %q, want the run's own output", second.output)
	}
	if second.duration != 3*time.Minute {
		t.Errorf("second finish reported duration %v, want %v", second.duration, 3*time.Minute)
	}
}

// The status is what decides the level. Publishing these as anything the
// vocabulary calls a success would file a killed run at info, which is the
// defect one layer down that runstatus exists to prevent.
func TestPublishInterruptedRunsPublishesAStatusThatEscalates(t *testing.T) {
	logger := &recordingLogger{}
	publishInterruptedRuns(logger, []db.InterruptedRun{
		{RunID: 1, JobID: 1, JobName: "j", StartedAt: started(1), FinishedAt: started(0)},
	})

	if len(logger.finishes) != 1 {
		t.Fatalf("published %d finishes, want 1", len(logger.finishes))
	}
	if got := logger.finishes[0].status; got != runstatus.Interrupted {
		t.Errorf("published status %q, want %q", got, runstatus.Interrupted)
	}
	if !runstatus.TerminalStatusIsFailure(logger.finishes[0].status) {
		t.Errorf("published status %q is filed as a success, so a killed run logs at info level",
			logger.finishes[0].status)
	}
}

// Cry-wolf control: a boot with nothing stranded must stay silent. A reconciler
// that announced itself on every clean start would make the real event
// unfindable.
func TestPublishInterruptedRunsSaysNothingWhenNothingWasStranded(t *testing.T) {
	logger := &recordingLogger{}
	publishInterruptedRuns(logger, nil)
	if len(logger.finishes) != 0 {
		t.Errorf("a clean boot published %d finishes, want 0", len(logger.finishes))
	}
}

// ---------------------------------------------------------------- fixtures

// fakeConnection stands in for the NATS client. Only the shutdown close is
// observable from main, which is exactly the part main owns.
type fakeConnection struct {
	mu     sync.Mutex
	closed int
}

func (c *fakeConnection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
}

func (c *fakeConnection) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// fakeBusService stands in for *bus.Handler: the cron loop's publisher, with a
// lifecycle of its own.
type fakeBusService struct {
	mu        sync.Mutex
	started   int
	closed    int
	published int
	startErr  error
}

func (b *fakeBusService) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started++
	return b.startErr
}

func (b *fakeBusService) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed++
}

func (b *fakeBusService) PublishRunResult(*db.Run, *db.Job, string, string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published++
}

func (b *fakeBusService) publishCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.published
}

func (b *fakeBusService) counts() (started, closed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started, b.closed
}

// recordingEntryPublisher is the sink the harness's logger writes to, so a
// test can read back what the boot published on the bus.
type recordingEntryPublisher struct {
	mu      sync.Mutex
	entries []logging.LogEntry
}

func (p *recordingEntryPublisher) Publish(_ string, data any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, data.(logging.LogEntry))
	return nil
}

// finishesOfRun returns the job finishes published for one run id.
func (p *recordingEntryPublisher) finishesOfRun(runID int64) []logging.LogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var finishes []logging.LogEntry
	for _, e := range p.entries {
		if e.Metadata["action"] == "finish" && e.Metadata["run_id"] == runID {
			finishes = append(finishes, e)
		}
	}
	return finishes
}

// servedCall is one call to the HTTP seam.
type servedCall struct {
	addr    string
	handler http.Handler
}

// harness assembles a config and a full set of seams, and records what the run
// asked of each one.
type harness struct {
	t        *testing.T
	cfg      config.Config
	settings *servicesettings.Registry

	conn      *fakeConnection
	bus       *fakeBusService
	published *recordingEntryPublisher
	// eventTriggerSubscriptions counts calls to subscribeEventTriggers.
	eventTriggerSubscriptions int

	mu           sync.Mutex
	connectCalls int
	connectNames []string
	connectErr   error
	// connectKeepsConnOnError makes the seam hand back a connection AND an
	// error, which is how a half-open client reports itself.
	connectKeepsConnOnError bool
	agentExecutors          int
	// loggerBuilds counts calls to the newLogger seam, and
	// connectCallsWhenLoggerBuilt records how many times connectNATS had been
	// called at the moment of the FIRST of them. Production's newLogger closes
	// over a *natsbus.Client that only connectNATS assigns, so a logger built
	// first is bound to a nil client and Logger.publish then returns silently —
	// every scheduler log line vanishes from the bus with no error anywhere.
	// The order is the whole of that contract and nothing else here records it.
	loggerBuilds                int
	connectCallsWhenLoggerBuilt int
	busHandlerURLs              []string
	busHandlerErr               error
	openStoreErr                error
	served                      []servedCall
	serveErr                    error
	logs                        []string

	// store, when set, is handed to the run instead of a fresh temp database.
	store *db.Store

	listening chan struct{}
	release   chan struct{}
	signals   chan os.Signal
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		conn:      &fakeConnection{},
		bus:       &fakeBusService{},
		listening: make(chan struct{}, 1),
		release:   make(chan struct{}),
		signals:   make(chan os.Signal, 1),
	}
	h.published = &recordingEntryPublisher{}
	h.cfg = config.Config{
		Port:         9999,
		DatabasePath: filepath.Join(t.TempDir(), "scheduler.db"),
		Token:        "tok",
		NATSURL:      "nats://fixture:4222",
		BridgeURL:    "http://bridge.fixture:1",
		NoteboardURL: "http://noteboard.fixture:2",
	}
	settings, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"SCHEDULER_DB": h.cfg.DatabasePath,
	}))
	if err != nil {
		t.Fatal(err)
	}
	h.settings = settings
	// A serving seam that never returns on its own, the way ListenAndServe does
	// not. Released at cleanup so no goroutine outlives the test.
	t.Cleanup(func() { close(h.release) })
	return h
}

func (h *harness) deps() deps {
	return deps{
		connectNATS: func(url, name string) (busConnection, error) {
			h.mu.Lock()
			h.connectCalls++
			h.connectNames = append(h.connectNames, name)
			err := h.connectErr
			keep := h.connectKeepsConnOnError
			h.mu.Unlock()
			if err != nil {
				if keep {
					return h.conn, err
				}
				return nil, err
			}
			return h.conn, nil
		},
		subscribeEventTriggers: func(store *db.Store, scheduler *cronpkg.Scheduler) error {
			h.mu.Lock()
			h.eventTriggerSubscriptions++
			h.mu.Unlock()
			return nil
		},
		newLogger: func() *logging.Logger {
			h.mu.Lock()
			if h.loggerBuilds == 0 {
				h.connectCallsWhenLoggerBuilt = h.connectCalls
			}
			h.loggerBuilds++
			h.mu.Unlock()
			// ⚠️ This double publishes whichever order it was built in, where
			// production's would be bound to a nil client and publish nothing.
			// It therefore cannot tell a correctly-ordered boot from a broken
			// one by what it published — only by the ORDER, above.
			return logging.NewPublishingTo(h.published)
		},
		newAgentExecutor: func() *executor.AgentExecutor {
			h.mu.Lock()
			h.agentExecutors++
			h.mu.Unlock()
			return nil
		},
		openStore: func(path string) (*db.Store, error) {
			h.mu.Lock()
			err := h.openStoreErr
			store := h.store
			h.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if store != nil {
				return store, nil
			}
			return db.New(path)
		},
		newBusHandler: func(natsURL string, _ *db.Store, _ *cronpkg.Scheduler, _ *logging.Logger) (busService, error) {
			h.mu.Lock()
			h.busHandlerURLs = append(h.busHandlerURLs, natsURL)
			err := h.busHandlerErr
			h.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return h.bus, nil
		},
		listenAndServe: func(addr string, handler http.Handler) error {
			h.mu.Lock()
			h.served = append(h.served, servedCall{addr: addr, handler: handler})
			err := h.serveErr
			h.mu.Unlock()
			select {
			case h.listening <- struct{}{}:
			default:
			}
			if err != nil {
				return err
			}
			<-h.release
			return http.ErrServerClosed
		},
		signals: h.signals,
		logf: func(format string, v ...any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.logs = append(h.logs, fmt.Sprintf(format, v...))
		},
	}
}

// run starts the daemon in the background and hands back the channel its exit
// arrives on.
func (h *harness) run() <-chan error {
	done := make(chan error, 1)
	go func() { done <- runScheduler(h.cfg, h.settings, h.deps()) }()
	return done
}

// waitListening blocks until the run reached the HTTP seam, which is the last
// thing a successful boot does.
func (h *harness) waitListening() {
	h.t.Helper()
	select {
	case <-h.listening:
	case <-time.After(patience):
		h.t.Fatal("the run never reached the HTTP server")
	}
}

// patience bounds every wait in this file. The paths under test are in-process
// and finish in tens of milliseconds, so this is a deadlock detector rather
// than a real deadline — and it is deliberately short, because the sabotage
// scorer runs this suite once per case and each edit that removes a gate makes
// several tests wait the whole of it.
const patience = 2 * time.Second

// runToFailure starts the daemon and waits for it to give up. A boot that keeps
// going is the failure this reports: without the timeout, a sabotage that
// removes an abort leaves the run parked in the serving seam until the whole
// test binary times out ten minutes later.
func (h *harness) runToFailure() error {
	h.t.Helper()
	select {
	case err := <-h.run():
		return err
	case <-time.After(patience):
		h.t.Fatal("the run kept booting past a failure that should have stopped it")
		return nil
	}
}

// shutdown delivers the signal and waits for the run to return.
func (h *harness) shutdown(done <-chan error) error {
	h.t.Helper()
	h.signals <- os.Interrupt
	select {
	case err := <-done:
		return err
	case <-time.After(patience):
		h.t.Fatal("the run did not return after a shutdown signal")
		return nil
	}
}

func (h *harness) servedCalls() []servedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]servedCall(nil), h.served...)
}

func (h *harness) busURLs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.busHandlerURLs...)
}

func (h *harness) connectNamesUsed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.connectNames...)
}

func (h *harness) agentExecutorCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agentExecutors
}

func (h *harness) logged(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// storeBrokenBy returns a live store after running one schema edit on it, so
// exactly one of the boot's queries fails and the others still work. A blanket
// break — a closed database, PRAGMA query_only — reddens every query at once
// and cannot say which site was reached.
func storeBrokenBy(t *testing.T, schemaEdit string) *db.Store {
	t.Helper()
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	store, err := db.NewWithDB(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(schemaEdit); err != nil {
		t.Fatal(err)
	}
	return store
}

// ------------------------------------------------------------ the boot path

func TestRunScheduler_ServesTheConfiguredPortAndShutsDownCleanly(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()

	served := h.servedCalls()
	if len(served) != 1 {
		t.Fatalf("bound %d addresses, want exactly 1", len(served))
	}
	if served[0].addr != ":9999" {
		t.Errorf("listening on %q, want %q", served[0].addr, ":9999")
	}
	if served[0].handler == nil {
		t.Error("served a nil handler")
	}

	if err := h.shutdown(done); err != nil {
		t.Fatalf("an ordinary shutdown returned %v, want nil", err)
	}
	if started, closed := h.bus.counts(); started != 1 || closed != 1 {
		t.Errorf("bus handler started %d / closed %d, want 1 / 1", started, closed)
	}
	if got := h.conn.closes(); got != 1 {
		t.Errorf("closed the NATS connection %d times, want 1", got)
	}
}

// The API server is the handler that gets bound, not some other mux: a run that
// binds the port and serves nothing is indistinguishable from a healthy one
// until somebody calls it.
func TestRunScheduler_TheBoundHandlerIsTheJobAPI(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	req, err := http.NewRequest(http.MethodGet, "/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.servedCalls()[0].handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health on the bound handler answered %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("GET /health on the bound handler said %q, want a health payload", rec.Body.String())
	}
}

// A failed connect is only a warning at the connect site. It costs the agent
// executor, and the bus handler is still handed the configured address: 6be744c
// chose that a scheduler does not run without NATS, and the real handler dials
// that address and stops the boot when it cannot (see
// TestRunScheduler_ABusHandlerThatWillNotBuildStopsTheBoot). This fake handler
// builds, so the boot here goes on and the test can read what it was handed.
func TestRunScheduler_AFailedBusConnectionCostsTheAgentExecutorAndStillHandsTheBusHandlerTheAddress(t *testing.T) {
	h := newHarness(t)
	h.connectErr = errors.New("no route to host")
	done := h.run()
	h.waitListening()

	if got := h.agentExecutorCount(); got != 0 {
		t.Errorf("built %d agent executors with no bus, want 0 — an agent job would have nowhere to send its request", got)
	}
	if urls := h.busURLs(); len(urls) != 1 || urls[0] != "nats://fixture:4222" {
		t.Errorf("bus handler got %q, want the configured address, so that a bus that is down stops the boot there", urls)
	}
	if !h.logged("NATS connect failed") {
		t.Error("a failed bus connection was not reported to the operator log")
	}

	if err := h.shutdown(done); err != nil {
		t.Fatalf("shutdown returned %v, want nil", err)
	}
	// Nothing to close, and closing a connection that was never opened is a
	// panic in the real client.
	if got := h.conn.closes(); got != 0 {
		t.Errorf("closed a connection that was never opened (%d times)", got)
	}
}

func TestRunScheduler_ABusConnectionWiresTheAgentExecutorAndItsURL(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	if got := h.agentExecutorCount(); got != 1 {
		t.Errorf("built %d agent executors with a live bus, want exactly 1", got)
	}
	if urls := h.busURLs(); len(urls) != 1 || urls[0] != "nats://fixture:4222" {
		t.Errorf("bus handler got %q, want the connected URL", urls)
	}
}

// ---------------------------------------------- what runs before the first tick

// Runs left "running" by a process that died are closed out at startup, before
// the cron loop can create new ones. Nothing else ever clears them.
func TestRunScheduler_ClosesOutRunsStrandedByAPreviousProcess(t *testing.T) {
	h := newHarness(t)

	seed, err := db.New(h.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	job, err := seed.CreateJob("stranded", "0 0 31 2 *", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := seed.CreateRun(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	done := h.run()
	h.waitListening()
	if err := h.shutdown(done); err != nil {
		t.Fatalf("shutdown returned %v, want nil", err)
	}

	after, err := db.New(h.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	runs, err := after.ListRuns(job.ID, db.MaxRunsListed)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != orphan.ID {
		t.Fatalf("found %d runs, want the one stranded run", len(runs))
	}
	if runs[0].Status != "interrupted" {
		t.Errorf("the stranded run is %q after a boot, want %q", runs[0].Status, "interrupted")
	}
	// The closed run is published, once, at error level. publishInterruptedRuns
	// has tests of its own; this pins that the boot calls it with what the
	// store closed.
	finishes := h.published.finishesOfRun(orphan.ID)
	if len(finishes) != 1 {
		t.Fatalf("published %d finishes for the stranded run, want 1", len(finishes))
	}
	if finishes[0].Level != "error" || finishes[0].Metadata["status"] != "interrupted" {
		t.Errorf("the stranded run was published at %q with status %v, want %q and %q",
			finishes[0].Level, finishes[0].Metadata["status"], "error", "interrupted")
	}
	// The count comes from the store, not from a constant: a reconciler that
	// reports a number it did not measure is the bug this line exists for.
	if !h.logged("reconciled 1 orphaned run(s)") {
		t.Error("the boot did not report how many runs it reconciled")
	}
}

func TestRunScheduler_ASilentBootReportsNoReconciliation(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	if err := h.shutdown(done); err != nil {
		t.Fatal(err)
	}
	if h.logged("orphaned run") {
		t.Error("a boot with nothing stranded still reported a reconciliation")
	}
}

// ------------------------------------------------------- failures that abort

func TestRunScheduler_ADatabaseThatWillNotOpenStopsTheBoot(t *testing.T) {
	h := newHarness(t)
	h.openStoreErr = errors.New("permission denied")

	err := h.runToFailure()

	if err == nil || !strings.Contains(err.Error(), "db:") {
		t.Fatalf("runScheduler = %v, want an error naming the database", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the error %q dropped the reason the database would not open", err)
	}
	if got := h.busURLs(); len(got) != 0 {
		t.Errorf("built a bus handler over a database that would not open (%q)", got)
	}
	if got := h.servedCalls(); len(got) != 0 {
		t.Errorf("bound %q with no database behind it — the API would answer every job query with a 500", got[0].addr)
	}
}

func TestRunScheduler_AFailedReconciliationStopsTheBoot(t *testing.T) {
	h := newHarness(t)
	h.store = storeBrokenBy(t, "DROP TABLE runs")

	err := h.runToFailure()

	if err == nil || !strings.Contains(err.Error(), "reconcile orphaned runs") {
		t.Fatalf("runScheduler = %v, want an error naming the reconciliation", err)
	}
	if got := h.servedCalls(); len(got) != 0 {
		t.Error("served the API after failing to reconcile — the stranded runs would read as live duplicates")
	}
	if started, _ := h.bus.counts(); started != 0 {
		t.Error("started the bus handler after failing to reconcile")
	}
}

func TestRunScheduler_ABusHandlerThatWillNotBuildStopsTheBoot(t *testing.T) {
	h := newHarness(t)
	h.busHandlerErr = errors.New("bad subject")

	err := h.runToFailure()

	if err == nil || !strings.Contains(err.Error(), "bus:") {
		t.Fatalf("runScheduler = %v, want an error naming the bus", err)
	}
	if got := h.servedCalls(); len(got) != 0 {
		t.Error("bound the port with no bus handler behind the cron loop")
	}
	if started, _ := h.bus.counts(); started != 0 {
		t.Error("started a bus handler that failed to build")
	}
}

// Scheduler.Start reads the job table. A boot that cannot read it has no jobs
// and must not pretend to be a scheduler. The break is one column that ListJobs
// reads and reconciliation does not: reconciliation joins jobs for the name and
// schedule, so dropping the whole table would stop the boot one site earlier.
func TestRunScheduler_ASchedulerThatWillNotStartStopsTheBoot(t *testing.T) {
	h := newHarness(t)
	h.store = storeBrokenBy(t, "ALTER TABLE jobs DROP COLUMN description")

	err := h.runToFailure()

	if err == nil || !strings.Contains(err.Error(), "cron:") {
		t.Fatalf("runScheduler = %v, want an error naming the cron loop", err)
	}
	if got := h.servedCalls(); len(got) != 0 {
		t.Error("served the API for a scheduler that never started — every job would silently never run")
	}
}

// ------------------------------------------------- failures that do NOT abort

// Subscribing to the bus is not worth a boot: jobs still tick and the API still
// answers. This is the one Start error in the file that is only a warning.
func TestRunScheduler_ABusHandlerThatWillNotSubscribeIsOnlyAWarning(t *testing.T) {
	h := newHarness(t)
	h.bus.startErr = errors.New("subscribe: no responders")

	done := h.run()
	h.waitListening()

	if !h.logged("bus handler failed to start") {
		t.Error("a bus handler that would not subscribe was not reported to the operator log")
	}
	if err := h.shutdown(done); err != nil {
		t.Fatalf("shutdown returned %v, want nil", err)
	}
	if _, closed := h.bus.counts(); closed != 1 {
		t.Error("a bus handler that failed to subscribe was not closed on the way out")
	}
}

// ------------------------------------------------------------- the serving end

// A port already in use is the common one, and it has to end the process: a
// scheduler nobody can reach is worse than one that is plainly down.
func TestRunScheduler_AServerThatCannotBindEndsTheRun(t *testing.T) {
	h := newHarness(t)
	h.serveErr = errors.New("address already in use")

	err := h.runToFailure()

	if err == nil || !strings.Contains(err.Error(), "http:") {
		t.Fatalf("runScheduler = %v, want an error naming the HTTP server", err)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("the error %q dropped the reason the port could not be bound", err)
	}
	// The hoist out of main() bought this: a serving failure now unwinds the
	// shutdown instead of exiting from inside the goroutine.
	if _, closed := h.bus.counts(); closed != 1 {
		t.Error("a failed bind left the bus handler open")
	}
	if got := h.conn.closes(); got != 1 {
		t.Errorf("a failed bind closed the NATS connection %d times, want 1", got)
	}
}

// ---------------------------------------------------------------- shutdown

func TestRunScheduler_ShutdownClosesTheBusHandlerAndTheConnectionOnce(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()

	if started, closed := h.bus.counts(); started != 1 || closed != 0 {
		t.Fatalf("before shutdown: bus started %d / closed %d, want 1 / 0", started, closed)
	}
	if got := h.conn.closes(); got != 0 {
		t.Fatalf("closed the connection %d times before shutdown", got)
	}

	if err := h.shutdown(done); err != nil {
		t.Fatalf("shutdown returned %v, want nil", err)
	}
	if _, closed := h.bus.counts(); closed != 1 {
		t.Errorf("bus handler closed %d times, want exactly 1", closed)
	}
	if got := h.conn.closes(); got != 1 {
		t.Errorf("connection closed %d times, want exactly 1", got)
	}
	if !h.logged("shutting down") {
		t.Error("the shutdown was not reported to the operator log")
	}
}

// The token reaches the API server. A daemon that drops it serves every job
// mutation on this box to anything that can route to the port.
func TestRunScheduler_TheConfiguredTokenGatesTheAPI(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	handler := h.servedCalls()[0].handler

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated GET /api/jobs answered %d, want 401", rec.Code)
	}

	authed := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	authed.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, authed)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/jobs with the configured token answered %d, want 200", rec.Code)
	}
}

// The name this process announces on the bus is what a human reads in a NATS
// connection list, and it is the only identity the connection carries.
func TestRunScheduler_AnnouncesItselfOnTheBusByName(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	names := h.connectNamesUsed()
	if len(names) != 1 || names[0] != "scheduler" {
		t.Errorf("connected as %q, want one connection named %q", names, "scheduler")
	}
}

// A client that reports both a connection and an error is not connected. Taking
// the connection anyway would wire an agent executor onto a dead client and
// close it at shutdown as if it had ever been open.
func TestRunScheduler_AConnectionReturnedWithAnErrorIsNotUsed(t *testing.T) {
	h := newHarness(t)
	h.connectErr = errors.New("half-open")
	h.connectKeepsConnOnError = true

	done := h.run()
	h.waitListening()

	if got := h.agentExecutorCount(); got != 0 {
		t.Errorf("built %d agent executors over a failed connection, want 0", got)
	}
	if urls := h.busURLs(); len(urls) != 1 || urls[0] != "nats://fixture:4222" {
		t.Errorf("bus handler got %q over a failed connection, want the configured address", urls)
	}
	if err := h.shutdown(done); err != nil {
		t.Fatal(err)
	}
	if got := h.conn.closes(); got != 0 {
		t.Errorf("closed a connection that never opened (%d times)", got)
	}
}

// The cron loop publishes its run results through the bus handler, and it only
// has one because this binary hands it over. Asserted end to end — run a job
// through the bound API and watch the result arrive at the handler — because
// the wiring call has no other observable effect.
func TestRunScheduler_RunResultsReachTheBusHandler(t *testing.T) {
	h := newHarness(t)

	seed, err := db.New(h.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	// 31 February: a schedule the cron loop accepts and never fires, so the only
	// run in this test is the one the API asks for.
	job, err := seed.CreateJob("never", "0 0 31 2 *", "echo scheduler-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/jobs/%d/run", job.ID), nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.servedCalls()[0].handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("running the job through the API answered %d: %s", rec.Code, rec.Body.String())
	}

	if got := h.bus.publishCount(); got != 1 {
		t.Errorf("the bus handler was told about %d run results, want 1 — the cron loop has no publisher", got)
	}
}

// The daemon connects to NATS once. A second connection would double every
// subscription the bus handler makes.
func TestRunScheduler_ConnectsToTheBusExactlyOnce(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	h.mu.Lock()
	got := h.connectCalls
	h.mu.Unlock()
	if got != 1 {
		t.Errorf("connected %d times, want exactly 1", got)
	}
}

// TestRunScheduler_TheLoggerIsBuiltAfterTheBusConnection pins the one ordering
// constraint productionDeps introduces and runScheduler satisfies only by line
// order.
//
// productionDeps returns a deps whose newLogger and newAgentExecutor close over
// a *natsbus.Client that is assigned inside connectNATS. Build the logger first
// and it is bound to a nil client; Logger.publish returns silently on a nil
// client, so the scheduler's whole lifecycle log disappears from the bus with no
// error raised anywhere and no test failing.
//
// It was measured MISSED before this test existed: hoisting `logger :=
// d.newLogger()` above the connectNATS block left all 21 tests green, while two
// control mutations of the same shape on the same tree came back CAUGHT.
func TestRunScheduler_TheLoggerIsBuiltAfterTheBusConnection(t *testing.T) {
	h := newHarness(t)
	done := h.run()
	h.waitListening()
	defer h.shutdown(done)

	h.mu.Lock()
	builds := h.loggerBuilds
	connectsFirst := h.connectCallsWhenLoggerBuilt
	h.mu.Unlock()

	if builds == 0 {
		t.Fatal("newLogger was never called, so this test pins nothing")
	}
	if connectsFirst < 1 {
		t.Errorf("the logger was built after %d connectNATS call(s), want at least 1: "+
			"production's newLogger closes over the client connectNATS assigns, so this "+
			"order binds the logger to a nil client and silently drops every log line",
			connectsFirst)
	}
}
