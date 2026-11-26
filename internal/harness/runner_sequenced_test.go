package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

var testRunnerMachine = &msg.Machine{ID: "m_remote", Name: "remote", Transport: msg.TransportRunner}

// A server in these tests is a Manager over a store. Two managers over one
// store, one after the other, are a server before and after a restart: the
// database is kept and everything in memory is new.
func managerOver(t *testing.T, st *store.Store) *Manager {
	t.Helper()
	return singleCopyStoresOf(t, st).attach(NewManager(st, "http://127.0.0.1:0", "", 0, nil))
}

func storeForRunnerTests(t *testing.T) *store.Store {
	t.Helper()
	return newSingleCopyTestStores(t).bridge
}

// standInRunner is the runner's end of the connection, driven by the test.
type standInRunner struct {
	t          *testing.T
	connection *websocket.Conn
	welcome    *runnerprotocol.RunnerWelcome
}

// connectStandInRunner dials manager's runner endpoint as the test machine,
// says Hello with the given held sessions, and returns once welcomed.
func connectStandInRunner(t *testing.T, manager *Manager, protocolVersion int, held ...runnerprotocol.RunnerHeldSession) *standInRunner {
	t.Helper()
	upgrader := websocket.Upgrader{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = manager.Runners().AcceptRunner(r.Context(), connection, testRunnerMachine, "test-server")
	}))
	t.Cleanup(endpoint.Close)

	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	runner := &standInRunner{t: t, connection: connection}
	runner.send(&runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgHello, Hello: &runnerprotocol.RunnerHello{
		Hostname: "remote", OS: "linux", Arch: "amd64", ProtocolVersion: protocolVersion, HeldSessions: held,
	}})
	runner.welcome = runner.read(runnerprotocol.RunnerMsgWelcome).Welcome
	return runner
}

func (runner *standInRunner) send(frame *runnerprotocol.RunnerMessage) {
	runner.t.Helper()
	runner.connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := runner.connection.WriteJSON(frame); err != nil {
		runner.t.Fatalf("sending %s: %v", frame.Type, err)
	}
}

func (runner *standInRunner) read(wanted runnerprotocol.RunnerMessageType) *runnerprotocol.RunnerMessage {
	runner.t.Helper()
	runner.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	var frame runnerprotocol.RunnerMessage
	if err := runner.connection.ReadJSON(&frame); err != nil {
		runner.t.Fatalf("waiting for a %s frame: %v", wanted, err)
	}
	if frame.Type != wanted {
		runner.t.Fatalf("waiting for a %s frame, got %s: %+v", wanted, frame.Type, frame)
	}
	return &frame
}

// sendLine sends entry sequence of the spawn: a line the harness "wrote",
// carrying text so the stored event can be told apart.
func (runner *standInRunner) sendLine(sessionID, spawnID string, sequence uint64, text string) {
	runner.t.Helper()
	line, err := json.Marshal(msg.Event{Type: msg.EventSystem, System: &msg.SystemEvent{Subtype: "test_line", Message: text}})
	if err != nil {
		runner.t.Fatal(err)
	}
	runner.send(&runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgStdout, SessionID: sessionID,
		Stdout: &runnerprotocol.RunnerStdout{Data: string(line), SpawnID: spawnID, Sequence: sequence}})
}

// readAcknowledgementThrough reads acknowledgements until one covers sequence.
func (runner *standInRunner) readAcknowledgementThrough(spawnID string, sequence uint64) {
	runner.t.Helper()
	for {
		frame := runner.read(runnerprotocol.RunnerMsgAck)
		if frame.Ack.SpawnID != spawnID {
			runner.t.Fatalf("acknowledgement for spawn %q, want %q", frame.Ack.SpawnID, spawnID)
		}
		if frame.Ack.Sequence > sequence {
			runner.t.Fatalf("the server acknowledged entry %d, and only %d were sent", frame.Ack.Sequence, sequence)
		}
		if frame.Ack.Sequence == sequence {
			return
		}
	}
}

// startSessionOnRunner creates a session and starts it on the stand-in
// runner, returning the spawn frame the runner was sent.
func startSessionOnRunner(t *testing.T, manager *Manager, runner *standInRunner, sessionID string) *runnerprotocol.RunnerSpawn {
	t.Helper()
	if err := manager.store.CreateSession(&store.Session{SessionID: sessionID, Harness: msg.HarnessMock, State: string(msg.SessionIdle)}); err != nil {
		t.Fatal(err)
	}
	session, err := manager.store.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	instance := &msg.Instance{ID: "inst-mock-remote", Machine: testRunnerMachine}
	if _, err := manager.StartOnInstance(context.Background(), session, instance, "", nil); err != nil {
		t.Fatalf("StartOnInstance: %v", err)
	}
	spawn := runner.read(runnerprotocol.RunnerMsgSpawn)
	if spawn.SessionID != sessionID {
		t.Fatalf("the spawn frame names session %q", spawn.SessionID)
	}
	return spawn.Spawn
}

// storedTestLines lists the texts of the session's stored test lines, in order.
func storedTestLines(t *testing.T, st *store.Store, sessionID string) []string {
	t.Helper()
	events, err := singleCopyStoresOf(t, st).singleCopy.ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, stored := range events {
		var event msg.Event
		if err := json.Unmarshal([]byte(stored.Data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == msg.EventSystem && event.System != nil && event.System.Subtype == "test_line" {
			lines = append(lines, event.System.Message)
		}
	}
	return lines
}

func waitUntilTrue(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestARunnerThatNumbersOutputIsGivenASpawnIDAndEachEntryIsStoredAndAcknowledged(t *testing.T) {
	st := storeForRunnerTests(t)
	manager := managerOver(t, st)
	runner := connectStandInRunner(t, manager, runnerprotocol.CurrentProtocolVersion)
	if runner.welcome.ProtocolVersion != runnerprotocol.CurrentProtocolVersion {
		t.Fatalf("the Welcome says protocol version %d", runner.welcome.ProtocolVersion)
	}
	if runner.welcome.HoldSessionsWithoutServer {
		t.Fatal("the Welcome asks for sessions to be held, though nothing switched holding on")
	}

	spawn := startSessionOnRunner(t, manager, runner, "br_sequenced")
	if spawn.SpawnID == "" {
		t.Fatal("a runner that numbers output was sent a spawn with no SpawnID")
	}
	recorded, err := st.RunnerSpawnOfSession("br_sequenced")
	if err != nil || recorded == nil || recorded.SpawnID != spawn.SpawnID || recorded.MachineID != testRunnerMachine.ID || recorded.Ended {
		t.Fatalf("the recorded spawn is %+v (err %v), want spawn %s on %s, open", recorded, err, spawn.SpawnID, testRunnerMachine.ID)
	}

	for sequence := uint64(1); sequence <= 3; sequence++ {
		runner.sendLine("br_sequenced", spawn.SpawnID, sequence, fmt.Sprintf("line %d", sequence))
	}
	runner.readAcknowledgementThrough(spawn.SpawnID, 3)
	if lines := storedTestLines(t, st, "br_sequenced"); fmt.Sprint(lines) != "[line 1 line 2 line 3]" {
		t.Fatalf("stored %v, want line 1 to line 3", lines)
	}
	if recorded, _ := st.RunnerSpawnOfSession("br_sequenced"); recorded.LastSequence != 3 {
		t.Errorf("the spawn is recorded as stored through %d, want 3", recorded.LastSequence)
	}
}

func TestARunnerFromBeforeNumberedOutputIsSpawnedTheOldWay(t *testing.T) {
	st := storeForRunnerTests(t)
	manager := managerOver(t, st)
	runner := connectStandInRunner(t, manager, 0) // says no protocol version
	spawn := startSessionOnRunner(t, manager, runner, "br_old_runner")
	if spawn.SpawnID != "" {
		t.Fatalf("an old runner was sent SpawnID %q, which it would ignore while the server waited for numbers", spawn.SpawnID)
	}
	if recorded, _ := st.RunnerSpawnOfSession("br_old_runner"); recorded != nil {
		t.Errorf("a spawn was recorded for a runner that keeps nothing: %+v", recorded)
	}
	// Its unnumbered output is still stored.
	line, _ := json.Marshal(msg.Event{Type: msg.EventSystem, System: &msg.SystemEvent{Subtype: "test_line", Message: "old way"}})
	runner.send(&runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgStdout, SessionID: "br_old_runner", Stdout: &runnerprotocol.RunnerStdout{Data: string(line)}})
	waitUntilTrue(t, "the old runner's line is stored", func() bool { return len(storedTestLines(t, st, "br_old_runner")) == 1 })
}

func TestARestartedServerTakesAHeldSessionBackAndStoresEveryEntryOnce(t *testing.T) {
	st := storeForRunnerTests(t)
	const sessionID = "br_survives"

	// Before the restart: the session starts and three entries are stored.
	before := managerOver(t, st)
	firstConnection := connectStandInRunner(t, before, runnerprotocol.CurrentProtocolVersion)
	spawn := startSessionOnRunner(t, before, firstConnection, sessionID)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		firstConnection.sendLine(sessionID, spawn.SpawnID, sequence, fmt.Sprintf("line %d", sequence))
	}
	firstConnection.readAcknowledgementThrough(spawn.SpawnID, 3)
	stateBefore, err := st.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	firstConnection.connection.Close()

	// The restart: everything in memory is gone, the database is kept. The
	// runner kept the process, which wrote entries 4 to 6 meanwhile.
	after := managerOver(t, st)
	secondConnection := connectStandInRunner(t, after, runnerprotocol.CurrentProtocolVersion,
		runnerprotocol.RunnerHeldSession{SessionID: sessionID, SpawnID: spawn.SpawnID, LastSequence: 6})

	if len(secondConnection.welcome.Adopt) != 1 {
		t.Fatalf("the Welcome adopts %+v, want the one held session", secondConnection.welcome.Adopt)
	}
	adopted := secondConnection.welcome.Adopt[0]
	if adopted.SessionID != sessionID || adopted.SpawnID != spawn.SpawnID || adopted.ResumeAfterSequence != 3 {
		t.Fatalf("adopted %+v, want this spawn resumed after entry 3", adopted)
	}
	if !after.HasProcess(sessionID) {
		t.Fatal("the restarted server has no handle on the session's process after taking it back")
	}
	stateAfter, err := st.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter.State != stateBefore.State {
		t.Errorf("taking the session back changed its state from %q to %q; nothing was started", stateBefore.State, stateAfter.State)
	}

	// The runner sends from entry 4. Entry 3 arrives once more first, as a
	// frame left over from the old connection would.
	secondConnection.sendLine(sessionID, spawn.SpawnID, 3, "line 3")
	for sequence := uint64(4); sequence <= 6; sequence++ {
		secondConnection.sendLine(sessionID, spawn.SpawnID, sequence, fmt.Sprintf("line %d", sequence))
	}
	secondConnection.readAcknowledgementThrough(spawn.SpawnID, 6)
	if lines := storedTestLines(t, st, sessionID); fmt.Sprint(lines) != "[line 1 line 2 line 3 line 4 line 5 line 6]" {
		t.Fatalf("after the restart the session holds %v; want each of line 1 to line 6 once, in order", lines)
	}

	// The handle works: input reaches the same process.
	if err := after.Send(sessionID, "are you still there", nil); err != nil {
		t.Fatalf("sending to the session taken back: %v", err)
	}
	if stdin := secondConnection.read(runnerprotocol.RunnerMsgStdin); stdin.SessionID != sessionID || !strings.Contains(stdin.Stdin.Data, "are you still there") {
		t.Fatalf("the runner was sent %+v", stdin)
	}

	// The process ends: its exit is entry 7, and the spawn is closed.
	secondConnection.send(&runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgExit, SessionID: sessionID,
		Exit: &runnerprotocol.RunnerExit{SpawnID: spawn.SpawnID, Sequence: 7}})
	secondConnection.readAcknowledgementThrough(spawn.SpawnID, 7)
	waitUntilTrue(t, "the ended session has no process", func() bool { return !after.HasProcess(sessionID) })
	if recorded, _ := st.RunnerSpawnOfSession(sessionID); recorded == nil || !recorded.Ended || recorded.LastSequence != 7 {
		t.Errorf("after the exit the spawn is recorded as %+v, want ended and stored through 7", recorded)
	}
}

func TestHeldSessionsTheServerCannotTakeBackAreLeftOutOfItsAnswer(t *testing.T) {
	st := storeForRunnerTests(t)
	before := managerOver(t, st)
	firstConnection := connectStandInRunner(t, before, runnerprotocol.CurrentProtocolVersion)

	restarted := startSessionOnRunner(t, before, firstConnection, "br_restarted_since")
	givenUp := startSessionOnRunner(t, before, firstConnection, "br_given_up")
	lostOutput := startSessionOnRunner(t, before, firstConnection, "br_runner_lost_output")
	for sequence := uint64(1); sequence <= 4; sequence++ {
		firstConnection.sendLine("br_runner_lost_output", lostOutput.SpawnID, sequence, "line")
	}
	firstConnection.readAcknowledgementThrough(lostOutput.SpawnID, 4)
	// The server started one session again on a new spawn, and killed another.
	if err := st.RecordRunnerSpawn("br_restarted_since", "spawn_newer", testRunnerMachine.ID); err != nil {
		t.Fatal(err)
	}
	if err := before.Kill("br_given_up"); err != nil {
		t.Fatal(err)
	}
	firstConnection.connection.Close()

	after := managerOver(t, st)
	secondConnection := connectStandInRunner(t, after, runnerprotocol.CurrentProtocolVersion,
		runnerprotocol.RunnerHeldSession{SessionID: "br_restarted_since", SpawnID: restarted.SpawnID, LastSequence: 9},
		runnerprotocol.RunnerHeldSession{SessionID: "br_given_up", SpawnID: givenUp.SpawnID, LastSequence: 9},
		runnerprotocol.RunnerHeldSession{SessionID: "br_runner_lost_output", SpawnID: lostOutput.SpawnID, LastSequence: 2},
		runnerprotocol.RunnerHeldSession{SessionID: "br_never_heard_of", SpawnID: "spawn_unknown", LastSequence: 9},
	)
	if len(secondConnection.welcome.Adopt) != 0 {
		t.Fatalf("the Welcome adopts %+v, want none: one was started again, one given up, one's log ends before what was stored, one is unknown", secondConnection.welcome.Adopt)
	}
	for _, sessionID := range []string{"br_restarted_since", "br_given_up", "br_runner_lost_output"} {
		if after.HasProcess(sessionID) {
			t.Errorf("the server holds a process handle for %s, which it did not take back", sessionID)
		}
	}
	// The session's current spawn is still the newer one: refusing the old
	// spawn did not put it back. It is marked ended for its own reason: the
	// runner did not report it, so its process is gone.
	if recorded, _ := st.RunnerSpawnOfSession("br_restarted_since"); recorded == nil || recorded.SpawnID != "spawn_newer" {
		t.Errorf("after refusing the old spawn the session's current one is %+v, want spawn_newer", recorded)
	}
}

func TestASpawnTheReturningRunnerDoesNotReportIsEnded(t *testing.T) {
	st := storeForRunnerTests(t)
	before := managerOver(t, st)
	firstConnection := connectStandInRunner(t, before, runnerprotocol.CurrentProtocolVersion)
	spawn := startSessionOnRunner(t, before, firstConnection, "br_process_gone")
	firstConnection.connection.Close()

	// The runner comes back holding nothing: it was not asked to hold, or it
	// was restarted itself.
	after := managerOver(t, st)
	connectStandInRunner(t, after, runnerprotocol.CurrentProtocolVersion)
	recorded, err := st.RunnerSpawnOfSession("br_process_gone")
	if err != nil || recorded == nil || recorded.SpawnID != spawn.SpawnID || !recorded.Ended {
		t.Fatalf("the spawn is recorded as %+v (err %v), want ended: its process is gone", recorded, err)
	}
}

func TestAwaitingRunnerHoldsOnlyWhileAHoldingRunnerMayStillReturn(t *testing.T) {
	st := storeForRunnerTests(t)
	before := managerOver(t, st)
	firstConnection := connectStandInRunner(t, before, runnerprotocol.CurrentProtocolVersion)
	startSessionOnRunner(t, before, firstConnection, "br_waiting")
	firstConnection.connection.Close()

	after := managerOver(t, st)
	if after.AwaitingRunner("br_waiting") {
		t.Error("with holding off, the session is said to await its runner; the runner ended its process when the connection dropped")
	}
	after.SetRunnerHoldPolicy(func() (bool, time.Duration) { return true, 30 * time.Minute })
	if !after.AwaitingRunner("br_waiting") {
		t.Error("with holding on and the runner away, the session must be left for the runner's return")
	}
	if after.AwaitingRunner("br_no_such_session") {
		t.Error("a session with no recorded spawn awaits nothing")
	}
	// Past the time a held process may work alone, plus the margin, waiting ends.
	after.runners.mu.Lock()
	after.runners.startedAt = time.Now().Add(-(30*time.Minute + runnerReturnMargin + time.Second))
	after.runners.mu.Unlock()
	if after.AwaitingRunner("br_waiting") {
		t.Error("the runner has been away longer than a held process may work alone; the session must stop waiting")
	}
}

func TestTheWelcomeCarriesTheHoldPolicy(t *testing.T) {
	st := storeForRunnerTests(t)
	manager := managerOver(t, st)
	manager.SetRunnerHoldPolicy(func() (bool, time.Duration) { return true, 30 * time.Minute })
	runner := connectStandInRunner(t, manager, runnerprotocol.CurrentProtocolVersion)
	if !runner.welcome.HoldSessionsWithoutServer || runner.welcome.HoldWithoutServerSeconds != 1800 {
		t.Fatalf("the Welcome says hold=%v for %d seconds, want true for 1800", runner.welcome.HoldSessionsWithoutServer, runner.welcome.HoldWithoutServerSeconds)
	}
}
