package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// killableProcess is a fakeProcess whose Kill ends its event stream, the way a
// real process's exit does, so readEvents returns and drops the derivation.
type killableProcess struct {
	fakeProcess
	closeOnce sync.Once
}

func (p *killableProcess) Kill() error {
	p.closeOnce.Do(func() { close(p.ch) })
	return nil
}

func seedSessionAt(t *testing.T, m *Manager, bridgeID string, state msg.SessionState) {
	t.Helper()
	if err := m.store.CreateSession(&store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, State: string(state),
		Purpose: "autoworker", Type: msg.SessionTypeAutonomous}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func readSession(t *testing.T, m *Manager, bridgeID string) *store.Session {
	t.Helper()
	sess, err := m.store.GetSession(bridgeID)
	if err != nil || sess == nil {
		t.Fatalf("read session %s: %v", bridgeID, err)
	}
	return sess
}

func TestSessionStateAfterIdleReap(t *testing.T) {
	cases := map[msg.SessionState]msg.SessionState{
		msg.SessionIdle:               msg.SessionCompleted,
		msg.SessionCompleted:          msg.SessionCompleted,
		msg.SessionAwaitingUser:       msg.SessionAwaitingUser,
		msg.SessionAwaitingPermission: msg.SessionAwaitingPermission,
		msg.SessionWaitingApproval:    msg.SessionWaitingApproval,
		msg.SessionError:              msg.SessionError,
		msg.SessionPaused:             msg.SessionAborted,
		msg.SessionStarting:           msg.SessionAborted,
		// Terminal mode is reaped whatever its row says, as before.
		msg.SessionModelGenerating: msg.SessionAborted,
		msg.SessionToolRunning:     msg.SessionAborted,
	}
	for prior, want := range cases {
		if got := sessionStateAfterIdleReap(prior); got != want {
			t.Errorf("reaped from %s: got %s, want %s", prior, got, want)
		}
	}
}

// The race that lost the reason about 95% of the time, reproduced without a
// race: the process has already exited and its derivation is gone when the
// reaper records the state, which is the branch that used to write a bare
// {state, changed_at}.
func TestReapIdleProcessRecordsTheReasonWhenTheDerivationIsGone(t *testing.T) {
	cases := []struct {
		name      string
		turnText  string
		wantState msg.SessionState
	}{
		{"a finished turn reads completed", "All done.", msg.SessionCompleted},
		{"an unanswered question keeps awaiting_user", "Which branch should I use?", msg.SessionAwaitingUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			const bridgeID = "br-reaped"
			seedSessionAt(t, m, bridgeID, msg.SessionIdle)

			proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 8)}
			waitForPumpExit := startEventPump(t, m, proc)
			proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
			proc.ch <- msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
				Result: &msg.ResultEvent{Text: tc.turnText}}
			close(proc.ch)
			waitForPumpExit()

			m.mu.Lock()
			_, derivationLeft := m.derivation[bridgeID]
			m.mu.Unlock()
			if derivationLeft {
				t.Fatal("the derivation outlived the pump; this test needs the branch with none")
			}
			before := readSession(t, m, bridgeID)

			left, err := m.ReapIdleProcess(bridgeID)
			if err != nil {
				t.Fatalf("ReapIdleProcess: %v", err)
			}
			after := readSession(t, m, bridgeID)
			if left != tc.wantState || after.State != string(tc.wantState) {
				t.Fatalf("left %s, row %s; want %s", left, after.State, tc.wantState)
			}
			if after.Status == nil || after.Status.Reason != msg.ReasonIdleReaped {
				t.Fatalf("status = %+v; want reason idle_reaped", after.Status)
			}
			if after.Status.AsOf <= before.Status.AsOf {
				t.Fatalf("as_of went %d → %d; the reap must be the newer status", before.Status.AsOf, after.Status.AsOf)
			}
			if tc.wantState == msg.SessionAwaitingUser && !after.Status.ChangedAt.Equal(before.Status.ChangedAt) {
				t.Fatalf("changed_at moved %s → %s though the state stayed", before.Status.ChangedAt, after.Status.ChangedAt)
			}
		})
	}
}

func TestReapIdleProcessKeepsAUserMarkedCompletedAndSaysItWasReaped(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-marked-done"
	seedSessionAt(t, m, bridgeID, msg.SessionIdle)
	if _, err := m.ForceSessionState(bridgeID, msg.SessionCompleted, msg.ReasonMarkDoneUser); err != nil {
		t.Fatal(err)
	}

	left, err := m.ReapIdleProcess(bridgeID)
	if err != nil {
		t.Fatalf("ReapIdleProcess: %v", err)
	}
	after := readSession(t, m, bridgeID)
	if left != msg.SessionCompleted || after.State != string(msg.SessionCompleted) {
		t.Fatalf("left %s, row %s; want completed", left, after.State)
	}
	if after.Status == nil || after.Status.Reason != msg.ReasonIdleReaped {
		t.Fatalf("status = %+v; want reason idle_reaped", after.Status)
	}
}

// With the process still registered and its derivation live, the reap kills
// it and the state goes through the derivation.
func TestReapIdleProcessKillsALiveProcessAndGradesFromTheDerivation(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-live-reap"
	seedSessionAt(t, m, bridgeID, msg.SessionIdle)

	proc := &killableProcess{fakeProcess: fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 8)}}
	m.mu.Lock()
	m.processes[bridgeID] = proc
	m.processStartedAt[bridgeID] = time.Now()
	m.mu.Unlock()
	waitForPumpExit := startEventPump(t, m, proc)
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	proc.ch <- msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		Result: &msg.ResultEvent{Text: "Finished."}}
	// The row was seeded idle, so wait for the turn's end, not the state.
	waitForEventType(t, m, bridgeID, msg.EventTurnComplete)
	waitForState(t, m, bridgeID, msg.SessionIdle)

	left, err := m.ReapIdleProcess(bridgeID)
	if err != nil {
		t.Fatalf("ReapIdleProcess: %v", err)
	}
	waitForPumpExit()
	if m.HasProcess(bridgeID) {
		t.Fatal("the process is still registered after the reap")
	}
	after := readSession(t, m, bridgeID)
	if left != msg.SessionCompleted || after.State != string(msg.SessionCompleted) ||
		after.Status == nil || after.Status.Reason != msg.ReasonIdleReaped {
		t.Fatalf("left %s, row %s, status %+v; want completed, idle_reaped", left, after.State, after.Status)
	}
}

func waitForState(t *testing.T, m *Manager, bridgeID string, want msg.SessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readSession(t, m, bridgeID).State == string(want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s never reached %s; it reads %s", bridgeID, want, readSession(t, m, bridgeID).State)
}

// br_1792020662192496685: a stop landed while the batch derive() had built for
// the turn's result was being written, and the batch's idle status, written
// after the forced aborted, carried the larger as_of and won. The test holds
// the publish lock the way deriveAndBroadcast does while it writes that batch,
// and forces aborted from another goroutine in the gap.
func TestForcedEndIsNotOverwrittenByABatchBuiltBeforeIt(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-stop-during-batch"
	seedSessionAt(t, m, bridgeID, msg.SessionIdle)

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 8)}
	waitForPumpExit := startEventPump(t, m, proc)
	defer func() { close(proc.ch); waitForPumpExit() }()
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	waitForState(t, m, bridgeID, msg.SessionModelGenerating)

	m.mu.Lock()
	d := m.derivation[bridgeID]
	m.mu.Unlock()

	d.publishMu.Lock()
	batch := d.derive(&msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		Result: &msg.ResultEvent{Text: "Done."}})
	forced := make(chan error, 1)
	go func() {
		_, err := m.ForceSessionState(bridgeID, msg.SessionAborted, msg.ReasonKilled)
		forced <- err
	}()
	select {
	case err := <-forced:
		t.Fatalf("the forced state was written while a batch was being published (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.publishDerived(bridgeID, batch)
	d.publishMu.Unlock()
	if err := <-forced; err != nil {
		t.Fatalf("ForceSessionState: %v", err)
	}

	row := readSession(t, m, bridgeID)
	if row.State != string(msg.SessionAborted) || row.Status == nil || row.Status.Reason != msg.ReasonKilled {
		t.Fatalf("row %s, status %+v; want aborted, killed", row.State, row.Status)
	}
}

// A runner's frames keep arriving after the server has forced an end — the
// rest of the dying process's output, buffered on the connection. They are
// not new activity, and only a new turn may move the state again.
func TestLateFramesDoNotMoveAForcedEnd(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-late-runner-frames"
	seedSessionAt(t, m, bridgeID, msg.SessionIdle)

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 8)}
	waitForPumpExit := startEventPump(t, m, proc)
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	waitForState(t, m, bridgeID, msg.SessionModelGenerating)

	if _, err := m.ForceSessionState(bridgeID, msg.SessionAborted, msg.ReasonKilled); err != nil {
		t.Fatal(err)
	}
	proc.ch <- msg.Event{Type: msg.EventToolCall, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}
	proc.ch <- msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		Result: &msg.ResultEvent{Text: "Done."}}
	// A marker the pump handles after the two late frames, so the row is read
	// once they have been derived.
	proc.ch <- msg.Event{Type: msg.EventThinking, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	waitForEventType(t, m, bridgeID, msg.EventThinking)

	row := readSession(t, m, bridgeID)
	if row.State != string(msg.SessionAborted) || row.Status == nil || row.Status.Reason != msg.ReasonKilled {
		t.Fatalf("after late frames: row %s, status %+v; want aborted, killed", row.State, row.Status)
	}

	// A new turn is new activity.
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-2"}
	waitForState(t, m, bridgeID, msg.SessionModelGenerating)
	if row := readSession(t, m, bridgeID); row.Status == nil || row.Status.Reason != msg.ReasonTurnStarted {
		t.Fatalf("after a new turn: status %+v; want reason turn_started", row.Status)
	}
	close(proc.ch)
	waitForPumpExit()
}

func waitForEventType(t *testing.T, m *Manager, bridgeID string, eventType msg.EventType) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.store.ListEventsSinceID(bridgeID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range events {
			var ev msg.Event
			if json.Unmarshal(row.Data, &ev) == nil && ev.Type == eventType {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event stored for %s", eventType, bridgeID)
}

// A derived transition writes its reason onto the status, and so does a
// spawn failure, with the error as the detail.
func TestStatusCarriesTheReasonForItsState(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-reasons"
	seedSessionAt(t, m, bridgeID, msg.SessionIdle)

	if _, err := m.ForceSessionStateWithDetail(bridgeID, msg.SessionError, msg.ReasonSpawnFailed, "instance uses the runner transport"); err != nil {
		t.Fatal(err)
	}
	row := readSession(t, m, bridgeID)
	if row.Status == nil || row.Status.Reason != msg.ReasonSpawnFailed || row.Status.ReasonDetail != "instance uses the runner transport" {
		t.Fatalf("status %+v; want spawn_failed with its cause", row.Status)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 8)}
	waitForPumpExit := startEventPump(t, m, proc)
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	proc.ch <- msg.Event{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
		Result: &msg.ResultEvent{Text: "Done."}}
	close(proc.ch)
	waitForPumpExit()

	row = readSession(t, m, bridgeID)
	if row.State != string(msg.SessionIdle) || row.Status == nil || row.Status.Reason != msg.ReasonTurnComplete || row.Status.ReasonDetail != "" {
		t.Fatalf("row %s, status %+v; want idle, turn_complete, no detail", row.State, row.Status)
	}
}

// Every reason the derivation and the server write is in the vocabulary
// msg.SessionStateReasons serves.
func TestReasonsWrittenAreInTheVocabulary(t *testing.T) {
	for _, reason := range []msg.SessionStateReason{
		msg.ReasonTurnStarted, msg.ReasonToolCalled, msg.ReasonTurnComplete, msg.ReasonIdleReaped,
		msg.ReasonTaskCompleted, msg.ReasonTaskFailed, msg.ReasonTaskCancelled, msg.ReasonParentProcessExited,
		msg.ReasonSpawnFailed, msg.ReasonKilled, msg.ReasonStartupReconcile,
	} {
		if !msg.ValidSessionStateReason(reason) {
			t.Errorf("%s is written but not in msg.SessionStateReasons", reason)
		}
	}
}

// A terminal-mode process the reaper kills exits, and its exit watcher used to
// record completed with pty_exited over whatever the reaper wrote, so the
// reason was lost there too.
func TestReapIdleProcessOfATerminalSessionKeepsIdleReaped(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	m := newManagerForWorkingDirTest(t)
	const bridgeID = "br-pty-reaped"
	sess := &store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, Mode: msg.SessionModePTY, State: string(msg.SessionIdle)}
	if err := m.store.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	inst := &msg.Instance{ID: "inst-cc-local", WorkingDir: t.TempDir(), Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal}}
	if _, err := m.StartOnInstance(context.Background(), sess, inst, "", nil); err != nil {
		t.Fatalf("StartOnInstance (pty): %v", err)
	}
	waitForRecordedCwd(t, recordTo)
	// Terminal mode derives no state; the row says whatever was last written.
	if _, err := m.ForceSessionState(bridgeID, msg.SessionIdle, msg.ReasonTurnComplete); err != nil {
		t.Fatal(err)
	}

	left, err := m.ReapIdleProcess(bridgeID)
	if err != nil || left != msg.SessionCompleted {
		t.Fatalf("ReapIdleProcess = %s, %v; want completed", left, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for readSession(t, m, bridgeID).PID != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the exit watcher never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	row := readSession(t, m, bridgeID)
	if row.State != string(msg.SessionCompleted) || row.Status == nil || row.Status.Reason != msg.ReasonIdleReaped {
		t.Fatalf("row %s, status %+v; want completed, idle_reaped", row.State, row.Status)
	}
}

// The order the test above cannot force: the reaper has written its state,
// and the exit watcher runs after it. The watcher must leave that state alone.
func TestTerminalExitAfterAReapLeavesTheReapersState(t *testing.T) {
	recordTo := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeHarnessOnPath(t, msg.HarnessClaudeCode, recordTo)
	m := newManagerForWorkingDirTest(t)
	const bridgeID = "br-pty-reaped-then-exited"
	sess := &store.Session{SessionID: bridgeID, Harness: msg.HarnessClaudeCode, Mode: msg.SessionModePTY, State: string(msg.SessionIdle)}
	if err := m.store.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	inst := &msg.Instance{ID: "inst-cc-local", WorkingDir: t.TempDir(), Machine: &msg.Machine{ID: "m_localhost", Transport: msg.TransportLocal}}
	proc, err := m.StartOnInstance(context.Background(), sess, inst, "", nil)
	if err != nil {
		t.Fatalf("StartOnInstance (pty): %v", err)
	}
	waitForRecordedCwd(t, recordTo)

	// What ReapIdleProcess does before and after its kill, with the kill
	// itself held back until the state is written.
	m.mu.Lock()
	m.ptyReapedByIdleReaper[bridgeID] = true
	m.mu.Unlock()
	if _, err := m.ForceSessionState(bridgeID, msg.SessionAwaitingUser, msg.ReasonIdleReaped); err != nil {
		t.Fatal(err)
	}
	_ = proc.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for readSession(t, m, bridgeID).PID != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the exit watcher never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	row := readSession(t, m, bridgeID)
	if row.State != string(msg.SessionAwaitingUser) || row.Status == nil || row.Status.Reason != msg.ReasonIdleReaped {
		t.Fatalf("row %s, status %+v; want the reaper's awaiting_user, idle_reaped", row.State, row.Status)
	}
}
