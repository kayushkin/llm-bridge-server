//go:build unix

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
)

// standInServer is the server's end of the runner connection, driven by the
// test one frame at a time. Each connection the runner makes arrives on
// connections, so a test can close one and wait for the next.
type standInServer struct {
	http        *httptest.Server
	connections chan *websocket.Conn
}

func newStandInServer(t *testing.T) *standInServer {
	t.Helper()
	server := &standInServer{connections: make(chan *websocket.Conn, 4)}
	upgrader := websocket.Upgrader{}
	server.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/ws" {
			http.NotFound(w, r) // the runner's checks for newer binaries
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		server.connections <- connection
	}))
	t.Cleanup(server.http.Close)
	return server
}

// nextConnection waits for the runner to connect and returns the connection
// and the Hello it opened with.
func (server *standInServer) nextConnection(t *testing.T) (*websocket.Conn, *runnerprotocol.RunnerHello) {
	t.Helper()
	select {
	case connection := <-server.connections:
		t.Cleanup(func() { connection.Close() })
		hello := readFrameOfType(t, connection, runnerprotocol.RunnerMsgHello)
		return connection, hello.Hello
	case <-time.After(15 * time.Second):
		t.Fatal("the runner did not connect within 15s")
		return nil, nil
	}
}

func sendFrame(t *testing.T, connection *websocket.Conn, frame *runnerprotocol.RunnerMessage) {
	t.Helper()
	connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := connection.WriteJSON(frame); err != nil {
		t.Fatalf("sending %s: %v", frame.Type, err)
	}
}

// readFrameOfType returns the next frame of the wanted type, passing over the
// ones a runner sends on its own schedule.
func readFrameOfType(t *testing.T, connection *websocket.Conn, wanted runnerprotocol.RunnerMessageType) *runnerprotocol.RunnerMessage {
	t.Helper()
	for {
		connection.SetReadDeadline(time.Now().Add(15 * time.Second))
		var frame runnerprotocol.RunnerMessage
		if err := connection.ReadJSON(&frame); err != nil {
			t.Fatalf("waiting for a %s frame: %v", wanted, err)
		}
		switch frame.Type {
		case wanted:
			return &frame
		case runnerprotocol.RunnerMsgMachineStats, runnerprotocol.RunnerMsgPong, runnerprotocol.RunnerMsgPing, runnerprotocol.RunnerMsgStderr:
			continue
		default:
			t.Fatalf("waiting for a %s frame, got %s: %+v", wanted, frame.Type, frame)
		}
	}
}

func welcomeFrame(hold bool, adopt ...runnerprotocol.RunnerAdoptSession) *runnerprotocol.RunnerMessage {
	return &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgWelcome, Welcome: &runnerprotocol.RunnerWelcome{
		MachineID:                 "m_test",
		MachineName:               "test-machine",
		PingIntervalSecs:          30,
		ProtocolVersion:           runnerprotocol.CurrentProtocolVersion,
		HoldSessionsWithoutServer: hold,
		HoldWithoutServerSeconds:  600,
		Adopt:                     adopt,
	}}
}

// talkativeHarness is a stand-in harness: it says it is ready, and on the
// input "burst" writes six lines over about a second and a half, long enough
// for a connection to drop and return while it works.
func talkativeHarness(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "talkative-harness")
	script := `#!/bin/sh
read start
echo '{"said":"ready"}'
while read line; do
  case "$line" in
    burst) for n in 1 2 3 4 5 6; do echo "{\"burst\":$n}"; sleep 0.25; done ;;
    *) echo "{\"heard\":\"$line\"}" ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func startRunnerAgainst(t *testing.T, server *standInServer) *Client {
	t.Helper()
	t.Setenv("LLMBRIDGE_RUNNER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	client := NewClient(&Config{ServerURL: server.http.URL, MachineID: "m_test", MachineName: "test-machine", RunnerToken: "tok_test"})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the runner did not stop within 10s")
		}
	})
	return client
}

func spawnFrame(sessionID, spawnID, binary string) *runnerprotocol.RunnerMessage {
	return &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgSpawn, SessionID: sessionID, Spawn: &runnerprotocol.RunnerSpawn{
		Harness:     "mock",
		BinaryPath:  binary,
		StartParams: json.RawMessage(`{}`),
		SpawnID:     spawnID,
	}}
}

func processIsAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAHeldSessionKeepsRunningAndItsOutputArrivesInOrderWhenTheServerReturns(t *testing.T) {
	server := newStandInServer(t)
	client := startRunnerAgainst(t, server)
	const sessionID, spawnID = "br_held", "spawn_1"

	first, hello := server.nextConnection(t)
	if hello.ProtocolVersion != runnerprotocol.CurrentProtocolVersion {
		t.Fatalf("the runner's Hello says protocol version %d, want %d", hello.ProtocolVersion, runnerprotocol.CurrentProtocolVersion)
	}
	if len(hello.HeldSessions) != 0 {
		t.Fatalf("a runner that has just started reports held sessions: %+v", hello.HeldSessions)
	}
	sendFrame(t, first, welcomeFrame(true))
	sendFrame(t, first, spawnFrame(sessionID, spawnID, talkativeHarness(t)))

	ready := readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout)
	if ready.Stdout.Sequence != 1 || ready.Stdout.SpawnID != spawnID || ready.Stdout.Data != `{"said":"ready"}` {
		t.Fatalf("first output frame: %+v", ready.Stdout)
	}
	sendFrame(t, first, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgAck, SessionID: sessionID, Ack: &runnerprotocol.RunnerAck{SpawnID: spawnID, Sequence: 1}})
	pid := client.registry.Get(sessionID).Pid()

	// The harness starts a burst, the server sees its first line, and then
	// the server goes away without acknowledging it.
	sendFrame(t, first, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgStdin, SessionID: sessionID, Stdin: &runnerprotocol.RunnerStdin{Data: "burst"}})
	if second := readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout); second.Stdout.Sequence != 2 {
		t.Fatalf("second output frame has sequence %d, want 2", second.Stdout.Sequence)
	}
	first.Close()

	// The runner reconnects about a second later; the burst goes on meanwhile.
	second, hello := server.nextConnection(t)
	if !processIsAlive(pid) {
		t.Fatal("the harness process was ended when the connection dropped, though the server asked for sessions to be held")
	}
	if got := client.registry.Get(sessionID); got == nil || got.Pid() != pid {
		t.Fatal("the session's process is no longer the one that was started")
	}
	if len(hello.HeldSessions) != 1 || hello.HeldSessions[0].SessionID != sessionID || hello.HeldSessions[0].SpawnID != spawnID || hello.HeldSessions[0].Exited {
		t.Fatalf("the reconnecting Hello lists held sessions %+v, want the one session, still running", hello.HeldSessions)
	}
	if hello.HeldSessions[0].LastSequence < 2 {
		t.Fatalf("the Hello says the log ends at %d, and entry 2 was already sent", hello.HeldSessions[0].LastSequence)
	}

	// The server stored entry 1 only: entry 2 reached it and was lost with it.
	sendFrame(t, second, welcomeFrame(true, runnerprotocol.RunnerAdoptSession{SessionID: sessionID, SpawnID: spawnID, ResumeAfterSequence: 1}))
	for burst := 1; burst <= 6; burst++ {
		frame := readFrameOfType(t, second, runnerprotocol.RunnerMsgStdout)
		wantSequence := uint64(burst + 1)
		wantData := `{"burst":` + string(rune('0'+burst)) + `}`
		if frame.Stdout.Sequence != wantSequence || frame.Stdout.Data != wantData {
			t.Fatalf("after the server returned, got entry %d %s; want entry %d %s: every line, once, in order", frame.Stdout.Sequence, frame.Stdout.Data, wantSequence, wantData)
		}
	}
	sendFrame(t, second, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgAck, SessionID: sessionID, Ack: &runnerprotocol.RunnerAck{SpawnID: spawnID, Sequence: 7}})

	// The same process still answers.
	sendFrame(t, second, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgStdin, SessionID: sessionID, Stdin: &runnerprotocol.RunnerStdin{Data: "still there"}})
	if frame := readFrameOfType(t, second, runnerprotocol.RunnerMsgStdout); frame.Stdout.Sequence != 8 || !strings.Contains(frame.Stdout.Data, "still there") {
		t.Fatalf("the held process answered with entry %d %s", frame.Stdout.Sequence, frame.Stdout.Data)
	}

	// Killing it ends the spawn and removes its log.
	sendFrame(t, second, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgSignal, SessionID: sessionID, Signal: &runnerprotocol.RunnerSignal{Signal: runnerprotocol.RunnerSignalKill}})
	waitUntil(t, "the killed process is gone", func() bool { return !processIsAlive(pid) })
	waitUntil(t, "the killed spawn's log is deleted", func() bool {
		_, err := os.Stat(filepath.Join(sequencedOutputFolder(), sessionID))
		return os.IsNotExist(err) || isEmptyFolder(filepath.Join(sequencedOutputFolder(), sessionID))
	})
	if held := client.registry.HeldSessions(); len(held) != 0 {
		t.Errorf("after the kill the runner still holds %+v", held)
	}
}

func isEmptyFolder(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

func TestAProcessEndsWithTheConnectionUnlessTheServerAskedForItToBeHeld(t *testing.T) {
	server := newStandInServer(t)
	client := startRunnerAgainst(t, server)
	const sessionID = "br_not_held"

	first, _ := server.nextConnection(t)
	sendFrame(t, first, welcomeFrame(false))
	sendFrame(t, first, spawnFrame(sessionID, "spawn_1", talkativeHarness(t)))
	readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout)
	pid := client.registry.Get(sessionID).Pid()
	first.Close()

	_, hello := server.nextConnection(t)
	waitUntil(t, "the process is ended", func() bool { return !processIsAlive(pid) })
	if len(hello.HeldSessions) != 0 {
		t.Errorf("the reconnecting Hello lists held sessions %+v, though the server never asked for holding", hello.HeldSessions)
	}
}

func TestASessionTheServerDoesNotTakeBackIsEnded(t *testing.T) {
	server := newStandInServer(t)
	client := startRunnerAgainst(t, server)
	const sessionID = "br_released"

	first, _ := server.nextConnection(t)
	sendFrame(t, first, welcomeFrame(true))
	sendFrame(t, first, spawnFrame(sessionID, "spawn_1", talkativeHarness(t)))
	readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout)
	pid := client.registry.Get(sessionID).Pid()
	first.Close()

	second, hello := server.nextConnection(t)
	if len(hello.HeldSessions) != 1 {
		t.Fatalf("held sessions in the Hello: %+v, want one", hello.HeldSessions)
	}
	// The server's Welcome adopts nothing: it has no record of this spawn.
	sendFrame(t, second, welcomeFrame(true))
	waitUntil(t, "the process the server did not take back is ended", func() bool { return !processIsAlive(pid) })
	waitUntil(t, "the runner holds nothing", func() bool { return len(client.registry.HeldSessions()) == 0 })
}

func TestASpawnWithNoSpawnIDRunsTheOldWay(t *testing.T) {
	server := newStandInServer(t)
	client := startRunnerAgainst(t, server)
	const sessionID = "br_old_way"

	// A server from before sequenced output: no protocol version, no SpawnID.
	first, _ := server.nextConnection(t)
	sendFrame(t, first, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgWelcome, Welcome: &runnerprotocol.RunnerWelcome{MachineID: "m_test", MachineName: "test-machine", PingIntervalSecs: 30}})
	sendFrame(t, first, spawnFrame(sessionID, "", talkativeHarness(t)))
	frame := readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout)
	if frame.Stdout.Sequence != 0 || frame.Stdout.SpawnID != "" || frame.Stdout.Data != `{"said":"ready"}` {
		t.Fatalf("an old-way spawn's output frame carries %+v, want no sequence and no spawn id", frame.Stdout)
	}
	if client.registry.HoldsSequencedSessions() {
		t.Error("an old-way spawn was given an output log")
	}
	pid := client.registry.Get(sessionID).Pid()
	first.Close()
	waitUntil(t, "the old-way process is ended when the connection drops", func() bool { return !processIsAlive(pid) })
}

func TestAnExitIsTheLastEntryAndItsLogGoesOnceAcknowledged(t *testing.T) {
	server := newStandInServer(t)
	client := startRunnerAgainst(t, server)
	const sessionID, spawnID = "br_exits", "spawn_1"

	// A harness that says two things and ends by itself.
	path := filepath.Join(t.TempDir(), "brief-harness")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nread start\necho '{\"n\":1}'\necho '{\"n\":2}'\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, _ := server.nextConnection(t)
	sendFrame(t, first, welcomeFrame(true))
	sendFrame(t, first, spawnFrame(sessionID, spawnID, path))

	for want := uint64(1); want <= 2; want++ {
		if frame := readFrameOfType(t, first, runnerprotocol.RunnerMsgStdout); frame.Stdout.Sequence != want {
			t.Fatalf("output entry %d arrived where %d belongs", frame.Stdout.Sequence, want)
		}
	}
	exit := readFrameOfType(t, first, runnerprotocol.RunnerMsgExit)
	if exit.Exit.Sequence != 3 || exit.Exit.SpawnID != spawnID || exit.Exit.ExitCode != 3 {
		t.Fatalf("the exit frame is %+v, want entry 3 with exit code 3: after both lines, never ahead of them", exit.Exit)
	}
	// Until the server acknowledges it, the runner keeps the spawn.
	if held := client.registry.HeldSessions(); len(held) != 1 || !held[0].Exited {
		t.Fatalf("before the acknowledgement the runner holds %+v, want the ended spawn", held)
	}
	sendFrame(t, first, &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgAck, SessionID: sessionID, Ack: &runnerprotocol.RunnerAck{SpawnID: spawnID, Sequence: 3}})
	waitUntil(t, "the acknowledged spawn is dropped", func() bool { return len(client.registry.HeldSessions()) == 0 })
}
