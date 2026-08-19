package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// findSubagent returns the single session whose manager_session_id points at
// parentID, failing if there isn't exactly one.
func findSubagent(t *testing.T, m *Manager, parentID string) *store.Session {
	t.Helper()
	sessions, err := m.store.ListSessions()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var found []store.Session
	for _, s := range sessions {
		if s.ManagerSessionID == parentID {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 session managed by %s; got %d", parentID, len(found))
	}
	return &found[0]
}

// seedParent creates the parent session a harness process runs as.
func seedParent(t *testing.T, m *Manager, bridgeID string) {
	t.Helper()
	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
		Purpose:   "chat",
		Type:      msg.SessionTypeInteractive,
	}); err != nil {
		t.Fatalf("seed parent session: %v", err)
	}
}

// taskStarted builds the parent's narration frame that ties a tool_use_id to a
// task_id. Shape taken from a live CC stream-json capture.
func taskStarted(bridgeID, toolUseID, taskID, description string) msg.Event {
	raw, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_started",
		"task_id": taskID, "tool_use_id": toolUseID, "description": description,
		"subagent_type": "Explore", "task_type": "local_agent",
	})
	return msg.Event{
		Type: msg.EventSystem, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, Raw: raw,
		System: &msg.SystemEvent{
			Subtype: "task_started", ToolUseID: toolUseID, TaskID: taskID, Description: description,
			TaskType: msg.TaskTypeLocalAgent, SubagentType: "Explore",
		},
	}
}

// taskStartedOfType builds the same narration for a task kind that is not an
// agent — a backgrounded shell command, say.
func taskStartedOfType(bridgeID, toolUseID, taskID, description, taskType string) msg.Event {
	ev := taskStarted(bridgeID, toolUseID, taskID, description)
	ev.System.TaskType = taskType
	ev.System.SubagentType = ""
	return ev
}

// taskUpdated models what llm-bridge-claudecode emits for a task_updated
// frame. Raw stays verbatim (CC nests the status under "patch"), but the
// router must not read it: TaskStatus is the normalized field the adapter
// populates and the only one this layer is entitled to look at.
func taskUpdated(bridgeID, taskID, status string) msg.Event {
	raw, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_updated",
		"task_id": taskID, "patch": map[string]any{"status": status, "end_time": 1785262846220},
	})
	return msg.Event{
		Type: msg.EventSystem, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, Raw: raw,
		System: &msg.SystemEvent{Subtype: "task_updated", TaskID: taskID, TaskStatus: status},
	}
}

// TestManager_SubagentFramesPromoteToOwnSession is the regression for the gap
// that left every harness subagent unattached: CC emits a Task subagent's
// frames inline on the parent's stream, so before the demux they were stored
// under the parent's session and the only surviving trace of the relationship
// was the parent's own narration. A subagent must land in its own session,
// linked back through manager_session_id.
func TestManager_SubagentFramesPromoteToOwnSession(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-demux"
	const toolUseID = "toolu_01C6u17fXLPviJJdM4tRLSjd"
	const taskID = "a13cb6a91d4c5d3dc"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	feed := []msg.Event{
		// The parent's own work.
		{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode,
			Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "spawning"}}}},
		taskStarted(parentID, toolUseID, taskID, "tiny-probe"),
		// The subagent's own work, distinguishable only by HarnessParentID.
		{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
			Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "subagent working"}}}},
		taskUpdated(parentID, taskID, "completed"),
	}
	for _, ev := range feed {
		proc.ch <- ev
	}
	close(proc.ch)
	waitForPumpExit()

	sub := findSubagent(t, m, parentID)

	if sub.SessionID == parentID {
		t.Fatalf("subagent reused the parent's session id")
	}
	if sub.RootSessionID != parentID {
		t.Errorf("root_session_id = %q, want %q", sub.RootSessionID, parentID)
	}
	if sub.Depth != 1 {
		t.Errorf("depth = %d, want 1", sub.Depth)
	}
	if sub.ControlledBy != "harness" {
		t.Errorf("controlled_by = %q, want harness — nothing may resume or kill a session coupled to a parent process", sub.ControlledBy)
	}
	if sub.Purpose != subagentPurpose {
		t.Errorf("purpose = %q, want %q", sub.Purpose, subagentPurpose)
	}
	// The dedupe key must match the rollout filename the discovery scanner
	// derives, or discovery mints a second row for the same subagent.
	if want := "agent-" + taskID; sub.HarnessSessionID != want {
		t.Errorf("harness_session_id = %q, want %q (must match subagents/agent-<task_id>.jsonl)", sub.HarnessSessionID, want)
	}
	if sub.DisplayName != "tiny-probe" {
		t.Errorf("display_name = %q, want tiny-probe", sub.DisplayName)
	}

	// The subagent's frame must be stored under the subagent, and the parent's
	// under the parent — the split is the whole point.
	assertHasBlockText(t, m, sub.SessionID, "subagent working")
	assertHasBlockText(t, m, parentID, "spawning")
	assertLacksBlockText(t, m, parentID, "subagent working")
	assertLacksBlockText(t, m, sub.SessionID, "spawning")
}

// TestManager_SubagentSettlesOnTerminalTask guards the state half: a subagent
// emits no result event of its own, so only the parent's task_updated can
// settle it. Without that it sits at "running" forever.
func TestManager_SubagentSettlesOnTerminalTask(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-settle"
	const toolUseID = "toolu_settle"
	const taskID = "task_settle"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	proc.ch <- taskUpdated(parentID, taskID, "completed")
	close(proc.ch)
	waitForPumpExit()

	if got := findSubagent(t, m, parentID).State; got != string(msg.SessionIdle) {
		t.Fatalf("subagent state = %q, want idle after task_updated status=completed", got)
	}
}

// TestManager_AbandonedSubagentSettlesOnProcessExit covers the other stuck-state
// path: the host process dies mid-task (kill, crash, turn watchdog), so no
// terminal task_updated ever arrives and nothing is left alive to settle the row.
func TestManager_AbandonedSubagentSettlesOnProcessExit(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-abandoned"
	const toolUseID = "toolu_abandoned"
	const taskID = "task_abandoned"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	close(proc.ch) // process exits with the task still open
	waitForPumpExit()

	sub := findSubagent(t, m, parentID)
	if sub.State != string(msg.SessionError) {
		t.Fatalf("abandoned subagent state = %q, want error", sub.State)
	}

	// The row alone settles nothing a client can see. Writing it silently was
	// the whole defect: the parent's timeline kept an open spawn forever and the
	// live status line kept the subagent listed as running, because both learn
	// that a task closed from the parent's event stream and nothing was emitted
	// there.
	closed := findStoredTaskEvent(t, m, parentID, "task_updated", taskID)
	if !msg.TaskStatusIsTerminal(closed.TaskStatus) {
		t.Errorf("derived close has status %q, which is not terminal — the spawn stays open", closed.TaskStatus)
	}
	if closed.TaskStatus != msg.TaskStatusCancelled {
		t.Errorf("status = %q, want %q — the task was taken away, it did not report a failure",
			closed.TaskStatus, msg.TaskStatusCancelled)
	}
	if closed.SubagentSessionID != sub.SessionID {
		t.Errorf("derived close names session %q, want %q", closed.SubagentSessionID, sub.SessionID)
	}
	if closed.Message == "" {
		t.Error("derived close carries no reason; a client showing it has nothing to say about why the task ended")
	}
}

// TestManager_UnknownParentToolUseFallsBackToParent is the fail-safe: if a
// subagent frame ever arrives before its task_started (an ordering guarantee CC
// could change), the frame must still be recorded on the parent rather than
// dropped or filed under a fabricated session.
func TestManager_UnknownParentToolUseFallsBackToParent(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-orphan"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode,
		HarnessParentID: "toolu_never_announced",
		Block:           &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "orphan frame"}}}}
	close(proc.ch)
	waitForPumpExit()

	sessions, err := m.store.ListSessions()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("orphan frame minted %d sessions; want only the parent", len(sessions)-1)
	}
	assertHasBlockText(t, m, parentID, "orphan frame")
}

// TestManager_ParentHarnessIDNotWrittenToSubagent guards the collision that
// would undo the dedupe key: every CC frame carries the parent's session_id,
// including a subagent's, so routing must not let a subagent frame overwrite
// the subagent row's harness_session_id with the parent's UUID.
func TestManager_ParentHarnessIDNotWrittenToSubagent(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-harnessid"
	const toolUseID = "toolu_harnessid"
	const taskID = "task_harnessid"
	const parentUUID = "5c3112cf-ac7b-4215-92de-991dd4628667"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	// The subagent's frame arrives FIRST carrying the parent's harness UUID —
	// the ordering that would poison the subagent row if routing were ignored.
	started := taskStarted(parentID, toolUseID, taskID, "probe")
	started.HarnessSessionID = parentUUID
	proc.ch <- started
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode,
		HarnessParentID: toolUseID, HarnessSessionID: parentUUID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	close(proc.ch)
	waitForPumpExit()

	sub := findSubagent(t, m, parentID)
	if sub.HarnessSessionID == parentUUID {
		t.Fatalf("subagent row took the parent's harness UUID %q; the two sessions now collide on the dedupe key", parentUUID)
	}
	parent, err := m.store.GetSession(parentID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parent.HarnessSessionID != parentUUID {
		t.Errorf("parent harness_session_id = %q, want %q", parent.HarnessSessionID, parentUUID)
	}
}

// startEventPump runs the manager's event pump for proc and returns the
// function that blocks until readEvents has RETURNED. Every test here feeds
// proc, closes its channel, then waits — because a subagent row only reaches
// its final state once the pump has run its exit path to the end.
//
// It replaces a helper called waitForProcessTeardown that waited for nothing.
// That helper polled m.processes until the entry for the parent was absent and
// then slept 20ms, with a comment explaining that readEvents deletes from that
// map just before its final store writes. The explanation was about real code
// — the exit path does delete the entry, then run pending.drop,
// subagents.settleUnfinished (which writes the very rows these assertions
// read) and UpdateSessionPID. But the poll never observed any of it: only
// StartProcess populates m.processes (manager.go:423), and no test in this
// file calls it — they run readEvents directly. So the entry was never there,
// the first read always found it absent, and the loop body ran zero times.
// Measured, not inferred: instrumented to count iterations, it reported 0 on
// every run.
//
// So the old helper was a flat 20ms sleep wearing the name of a
// synchronization primitive, and 20ms is a guess about how long the pump takes
// rather than a signal that it finished. Under -race, where the binary runs
// several times slower, the guess lost often enough to fail six tests
// intermittently — while a plain `go test ./...` stayed green, which is the
// part that made it invisible.
//
// The losses came in two shapes. Assertions read rows settleUnfinished had not
// written yet ("subagent state = running, want error"). And the test body
// ended while those writes were still in flight, so t.Cleanup closed the store
// underneath them — the "sql: database is closed" and SQLITE_BUSY lines that
// accompanied the failures.
//
// Waiting on the goroutine is the honest signal and needs no production
// change. Nothing outside these tests waits on the process map to conclude a
// session has settled — its only non-test reader is the liveness getter at
// manager.go:442 — so there is no ordering guarantee here for readEvents to
// announce. There is just a goroutine the test itself starts, which the test
// can wait for.
func startEventPump(t *testing.T, m *Manager, proc HarnessProcess) (waitForPumpExit func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.readEvents(proc)
	}()
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("readEvents did not return for %s", proc.SessionID())
		}
	}
}

// blockTexts returns every text block persisted against a session.
func blockTexts(t *testing.T, m *Manager, sessionID string) []string {
	t.Helper()
	events, err := m.store.ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatalf("list events for %s: %v", sessionID, err)
	}
	var out []string
	for _, e := range events {
		var ev msg.Event
		if json.Unmarshal([]byte(e.Data), &ev) != nil {
			continue
		}
		if ev.Block != nil && ev.Block.Block.Text != nil {
			out = append(out, ev.Block.Block.Text.Text)
		}
	}
	return out
}

// storedSystemEvents returns every SystemEvent persisted against a session, in
// order. The stored bytes are what a client actually receives, so a field that
// is set after the marshal is invisible here — which is the point.
func storedSystemEvents(t *testing.T, m *Manager, sessionID string) []msg.SystemEvent {
	t.Helper()
	events, err := m.store.ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatalf("list events for %s: %v", sessionID, err)
	}
	var out []msg.SystemEvent
	for _, e := range events {
		var ev msg.Event
		if json.Unmarshal([]byte(e.Data), &ev) != nil {
			continue
		}
		if ev.System != nil {
			out = append(out, *ev.System)
		}
	}
	return out
}

// findStoredTaskEvent returns the one stored SystemEvent of the given subtype
// naming taskID.
func findStoredTaskEvent(t *testing.T, m *Manager, sessionID, subtype, taskID string) msg.SystemEvent {
	t.Helper()
	for _, sys := range storedSystemEvents(t, m, sessionID) {
		if sys.Subtype == subtype && sys.TaskID == taskID {
			return sys
		}
	}
	t.Fatalf("session %s has no stored %s for task %s", sessionID, subtype, taskID)
	return msg.SystemEvent{}
}

// TestManager_TaskStartedCarriesSubagentSessionID is the regression for the
// ordering that made the link unreachable: the session used to be minted on the
// subagent's FIRST OWN FRAME, which arrives after task_started has already been
// marshalled, stored and fanned out. A client watching the parent therefore saw
// a subagent announced with no id to follow, and the only join left to it was
// the harness's own task id against a name-shaped harness_session_id.
//
// The id must be on the stored bytes, not merely set on the in-memory event.
func TestManager_TaskStartedCarriesSubagentSessionID(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-stamp"
	const toolUseID = "toolu_stamp"
	const taskID = "task_stamp"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
	go m.readEvents(proc)
	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- taskUpdated(parentID, taskID, msg.TaskStatusCompleted)
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

	sub := findSubagent(t, m, parentID)
	started := findStoredTaskEvent(t, m, parentID, "task_started", taskID)
	if started.SubagentSessionID != sub.SessionID {
		t.Errorf("task_started subagent_session_id = %q, want %q — the announcement is the event a client links from",
			started.SubagentSessionID, sub.SessionID)
	}
	// The closing frame names the same session, so a client that renders the
	// finish apart from the spawn can link from either row.
	finished := findStoredTaskEvent(t, m, parentID, "task_updated", taskID)
	if finished.SubagentSessionID != sub.SessionID {
		t.Errorf("task_updated subagent_session_id = %q, want %q", finished.SubagentSessionID, sub.SessionID)
	}
}

// TestManager_SubagentWithNoFramesStillGetsASessionAndSettles covers the hole
// lazy minting left open. A task that dies before emitting a single frame never
// reached route(), so no session was minted; settle and settleUnfinished both
// skip a task with no session, so its terminal status went nowhere. The row
// then reappeared later through discovery with no manager_session_id — the
// bulk of the orphan backlog on this host.
func TestManager_SubagentWithNoFramesStillGetsASessionAndSettles(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-frameless"
	const toolUseID = "toolu_frameless"
	const taskID = "task_frameless"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
	go m.readEvents(proc)
	// Announced, then reported failed. The subagent itself never speaks.
	proc.ch <- taskStarted(parentID, toolUseID, taskID, "dies immediately")
	proc.ch <- taskUpdated(parentID, taskID, msg.TaskStatusFailed)
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

	sub := findSubagent(t, m, parentID)
	if sub.State != string(msg.SessionError) {
		t.Errorf("state = %q, want %q — a failed subagent that never spoke must still settle", sub.State, msg.SessionError)
	}
}

// TestManager_NonAgentTaskCarriesNoSubagentSessionID pins the empty case as a
// real answer. A backgrounded shell gets the same task frames a subagent does
// and deliberately gets no session, so the field must stay empty rather than be
// filled with the parent's id or the harness's task id.
func TestManager_NonAgentTaskCarriesNoSubagentSessionID(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-bash"
	const taskID = "task_bash"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
	go m.readEvents(proc)
	proc.ch <- taskStartedOfType(parentID, "toolu_bash", taskID, "sleep 2", msg.TaskTypeLocalBash)
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

	started := findStoredTaskEvent(t, m, parentID, "task_started", taskID)
	if started.SubagentSessionID != "" {
		t.Errorf("subagent_session_id = %q, want empty — a backgrounded shell has no session", started.SubagentSessionID)
	}
}

// taskNotification models the frame that actually closes a backgrounded shell.
// Taken from a live capture (log-store events 1770747/1770748): a local_bash
// task_started, then a task_notification naming the same task_id and tool_use_id
// and carrying status "completed". A shell is never closed by a task_updated,
// which is why the fixture is not the one the subagent tests use.
func taskNotification(bridgeID, toolUseID, taskID, status string) msg.Event {
	raw, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_notification",
		"task_id": taskID, "tool_use_id": toolUseID, "status": status,
	})
	return msg.Event{
		Type: msg.EventSystem, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, Raw: raw,
		System: &msg.SystemEvent{
			Subtype: "task_notification", TaskID: taskID, ToolUseID: toolUseID, TaskStatus: status,
		},
	}
}

// countStoredTaskEvents counts stored SystemEvents naming taskID with a terminal
// status — how many times a client is told this task ended.
func countStoredTaskCloses(t *testing.T, m *Manager, sessionID, taskID string) int {
	t.Helper()
	var n int
	for _, sys := range storedSystemEvents(t, m, sessionID) {
		if sys.TaskID == taskID && msg.TaskStatusIsTerminal(sys.TaskStatus) {
			n++
		}
	}
	return n
}

// TestManager_AbandonedUnpromotedTaskIsClosedOnProcessExit is the regression for
// the half settleUnfinished did not cover. A backgrounded shell gets the same
// task_started a subagent does and deliberately gets no session — so it is
// absent from byToolUse, which settleUnfinished iterates, and when the process
// died underneath it nothing ever sent a terminal status. The parent's timeline
// kept the spawn row open forever and the live status line kept listing a shell
// that had been dead since the process was.
//
// Measured on this host: 8% of all bash spawns never terminated, and 12 of 20
// unfinished spawns in the 40 most recent sessions were followed by a fresh
// init — the process died, it did not keep running.
//
// The kinds are a table because promotion declines for every non-agent kind, not
// only local_bash, and an unknown kind draws the same spawn row.
func TestManager_AbandonedUnpromotedTaskIsClosedOnProcessExit(t *testing.T) {
	for _, taskType := range []string{msg.TaskTypeLocalBash, "a_kind_this_build_has_never_seen"} {
		t.Run(taskType, func(t *testing.T) {
			m := newTestManager(t)
			parentID := "br-parent-unpromoted-" + taskType
			const toolUseID = "toolu_unpromoted"
			const taskID = "task_unpromoted"
			seedParent(t, m, parentID)

			proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
			go m.readEvents(proc)
			proc.ch <- taskStartedOfType(parentID, toolUseID, taskID, "sleep 2", taskType)
			close(proc.ch) // the process dies with the shell still open
			waitForProcessTeardown(t, m, parentID)

			closed := findStoredTaskEvent(t, m, parentID, "task_updated", taskID)
			if !msg.TaskStatusIsTerminal(closed.TaskStatus) {
				t.Errorf("derived close has status %q, which is not terminal — the spawn row stays open", closed.TaskStatus)
			}
			if closed.TaskStatus != msg.TaskStatusCancelled {
				t.Errorf("status = %q, want %q — the task was taken away, it did not report a failure",
					closed.TaskStatus, msg.TaskStatusCancelled)
			}
			// The tool_use_id is the join back to the Bash call the row was
			// drawn from; a close without it closes nothing a client can find.
			if closed.ToolUseID != toolUseID {
				t.Errorf("derived close names tool_use_id %q, want %q", closed.ToolUseID, toolUseID)
			}
			if closed.Message == "" {
				t.Error("derived close carries no reason; a client showing it has nothing to say about why the task ended")
			}
			// An unpromoted task has no session, and inventing one here would
			// undo exactly what declining to promote it achieved.
			if closed.SubagentSessionID != "" {
				t.Errorf("derived close names session %q; a backgrounded shell has none", closed.SubagentSessionID)
			}
			if sessions, err := m.store.ListSessions(); err != nil {
				t.Fatalf("list sessions: %v", err)
			} else if len(sessions) != 1 {
				t.Fatalf("closing the task minted %d extra sessions; want only the parent", len(sessions)-1)
			}
		})
	}
}

// TestManager_ClosedUnpromotedTaskIsNotClosedTwice is the other side of it. A
// shell that the harness closed for itself must not collect a second,
// contradicting close at process exit: the timeline draws one finish row per
// task and moves it to the LATEST close, so a spurious cancelled would overwrite
// a real completed and report every backgrounded shell as killed.
//
// Both closing subtypes are tested. A live shell closes with task_notification;
// task_updated is what the promoted path is written against, and a rule that
// recognized only one of them would pass on the fixture its author happened to
// pick.
func TestManager_ClosedUnpromotedTaskIsNotClosedTwice(t *testing.T) {
	closers := map[string]func(bridgeID, toolUseID, taskID, status string) msg.Event{
		"task_notification": taskNotification,
		"task_updated": func(bridgeID, toolUseID, taskID, status string) msg.Event {
			ev := taskUpdated(bridgeID, taskID, status)
			ev.System.ToolUseID = toolUseID
			return ev
		},
	}
	for subtype, build := range closers {
		t.Run(subtype, func(t *testing.T) {
			m := newTestManager(t)
			parentID := "br-parent-closed-" + subtype
			const toolUseID = "toolu_closed"
			const taskID = "task_closed"
			seedParent(t, m, parentID)

			proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
			go m.readEvents(proc)
			proc.ch <- taskStartedOfType(parentID, toolUseID, taskID, "sleep 2", msg.TaskTypeLocalBash)
			proc.ch <- build(parentID, toolUseID, taskID, msg.TaskStatusCompleted)
			close(proc.ch)
			waitForProcessTeardown(t, m, parentID)

			if got := countStoredTaskCloses(t, m, parentID, taskID); got != 1 {
				t.Fatalf("task %s was closed %d times, want 1 — the harness already closed it", taskID, got)
			}
			if got := findStoredTaskEvent(t, m, parentID, subtype, taskID).TaskStatus; got != msg.TaskStatusCompleted {
				t.Errorf("the harness's own close now reads %q, want %q", got, msg.TaskStatusCompleted)
			}
		})
	}
}

// TestManager_UnpromotedTaskWithNonTerminalStatusIsStillClosed pins the rule
// forgetClosedUnpromoted shares with settle(): a status this build does not
// recognize is not a close. Forgetting the task on one would drop the only
// record that its spawn row is still open, and the row would then hang exactly
// as it did before — a fix that reads as present and fires on nothing.
func TestManager_UnpromotedTaskWithNonTerminalStatusIsStillClosed(t *testing.T) {
	for _, status := range []string{msg.TaskStatusInProgress, "quiesced", ""} {
		name := status
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t)
			parentID := "br-parent-nonterminal-" + name
			const toolUseID = "toolu_nonterminal"
			const taskID = "task_nonterminal"
			seedParent(t, m, parentID)

			proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 8)}
			go m.readEvents(proc)
			proc.ch <- taskStartedOfType(parentID, toolUseID, taskID, "sleep 2", msg.TaskTypeLocalBash)
			proc.ch <- taskNotification(parentID, toolUseID, taskID, status)
			close(proc.ch)
			waitForProcessTeardown(t, m, parentID)

			if got := countStoredTaskCloses(t, m, parentID, taskID); got != 1 {
				t.Fatalf("task %s has %d terminal closes, want 1: status %q was treated as a close it is not",
					taskID, got, status)
			}
		})
	}
}

func assertHasBlockText(t *testing.T, m *Manager, sessionID, want string) {
	t.Helper()
	for _, got := range blockTexts(t, m, sessionID) {
		if got == want {
			return
		}
	}
	t.Errorf("session %s is missing block text %q", sessionID, want)
}

func assertLacksBlockText(t *testing.T, m *Manager, sessionID, unwanted string) {
	t.Helper()
	for _, got := range blockTexts(t, m, sessionID) {
		if got == unwanted {
			t.Errorf("session %s wrongly holds block text %q", sessionID, unwanted)
		}
	}
}

// TestManager_SubagentIgnoresRawStatusAndUnknownStatuses pins two properties of
// settling that are easy to lose.
//
// First: this layer must not parse the harness's wire format. It used to
// json.Unmarshal ev.Raw to find the status, which made bridge-server a second
// reader of Claude Code's frame shape and would have broken silently the day CC
// renamed the field. A frame whose Raw says "completed" but whose normalized
// TaskStatus is empty must NOT settle anything — if it does, the re-parse is
// back.
//
// Second: an unrecognized status must be treated as non-terminal. A harness
// that adds a status later must not be able to park a subagent that is still
// working.
func TestManager_SubagentIgnoresRawStatusAndUnknownStatuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*msg.Event)
	}{
		{
			name: "status only in Raw, not normalized",
			// Exactly the shape the old re-parse keyed on.
			mutate: func(e *msg.Event) { e.System.TaskStatus = "" },
		},
		{
			name:   "a status this build has never seen",
			mutate: func(e *msg.Event) { e.System.TaskStatus = "quiesced" },
		},
		{
			name:   "in_progress is explicitly not terminal",
			mutate: func(e *msg.Event) { e.System.TaskStatus = msg.TaskStatusInProgress },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			parentID := "br-parent-" + t.Name()
			const toolUseID = "toolu_noraw"
			const taskID = "task_noraw"
			seedParent(t, m, parentID)

			proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
			waitForPumpExit := startEventPump(t, m, proc)

			proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
			proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
				Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}

			ev := taskUpdated(parentID, taskID, msg.TaskStatusCompleted)
			tc.mutate(&ev)
			proc.ch <- ev
			close(proc.ch)
			waitForPumpExit()

			// settle() must have declined. The process then exits with the task
			// still open, so settleUnfinished closes it out as error — which is
			// the honest record of "nobody ever said this finished", and is
			// distinguishable from the idle a real completion produces.
			if got := findSubagent(t, m, parentID).State; got != string(msg.SessionError) {
				t.Fatalf("subagent state = %q, want %q: settle() accepted a status it should have refused", got, msg.SessionError)
			}
		})
	}
}

// TestManager_OnlyAgentTasksBecomeSessions is the regression for a defect that
// reached production: Claude Code backgrounds a shell command through the same
// task_started / task_notification frames it uses for a subagent, so promoting
// every task minted a linked "subagent" session for `sleep 2` — really observed,
// as agent-bhrfxpye5, display name "Trigger on-demand discovery and watch for
// links".
//
// An unrecognized kind must also decline. Promoting one invents a session and
// links it into the management tree; declining only omits a session that the
// harness's own on-disk record can still supply.
func TestManager_OnlyAgentTasksBecomeSessions(t *testing.T) {
	cases := []struct {
		taskType string
		promote  bool
	}{
		{msg.TaskTypeLocalAgent, true},
		{msg.TaskTypeLocalWorkflow, true},
		{msg.TaskTypeRemoteAgent, true},
		{msg.TaskTypeLocalBash, false},
		{"a_kind_this_build_has_never_seen", false},
		{"", false},
	}

	for _, tc := range cases {
		name := tc.taskType
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t)
			parentID := "br-parent-" + name
			const toolUseID = "toolu_kind"
			const taskID = "task_kind"
			seedParent(t, m, parentID)

			proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
			waitForPumpExit := startEventPump(t, m, proc)

			proc.ch <- taskStartedOfType(parentID, toolUseID, taskID, "probe", tc.taskType)
			proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
				Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
			proc.ch <- taskUpdated(parentID, taskID, msg.TaskStatusCompleted)
			close(proc.ch)
			waitForPumpExit()

			sessions, err := m.store.ListSessions()
			if err != nil {
				t.Fatalf("list sessions: %v", err)
			}
			var promoted int
			for _, s := range sessions {
				if s.ManagerSessionID == parentID {
					promoted++
				}
			}
			if tc.promote && promoted != 1 {
				t.Fatalf("task_type %q produced %d sessions, want 1", tc.taskType, promoted)
			}
			if !tc.promote {
				if promoted != 0 {
					t.Fatalf("task_type %q produced %d sessions; only an agent gets one", tc.taskType, promoted)
				}
				// Its frames must still reach the parent rather than vanish.
				assertHasBlockText(t, m, parentID, "work")
			}
		})
	}
}

// TestSubagentIsMarkedNotBridgeControlled pins the marking the resume gate reads.
//
// A promoted subagent has no process of its own — it ran inside its parent — so
// "running with no process" is its normal state, not a fault. The watchdog read
// it as a fault and resumed it, and because bridge-server cannot resume what it
// did not spawn, the harness refused `--resume agent-<task_id>` (not a Claude
// Code UUID) and started a FRESH agent, which replayed the turn, ran tools
// unsupervised, and overwrote the row's harness_session_id with its own new
// UUID — destroying the dedupe key discovery uses to converge on that row.
//
// TEAM-ORCHESTRATION §21.6 predicted exactly this: controlled_by was written
// and read by nothing, "load-bearing the moment one does". The gate lives in
// server.autoResume / handleResumeSession / handleSendMessage; this test pins
// the marking those gates depend on, and that the dedupe key survives.
func TestSubagentIsMarkedNotBridgeControlled(t *testing.T) {
	m := newTestManager(t)
	const parentID = "br-parent-controlled"
	const toolUseID = "toolu_controlled"
	const taskID = "a565f4c108cbf251c"
	seedParent(t, m, parentID)

	proc := &fakeProcess{sid: parentID, ch: make(chan msg.Event, 16)}
	waitForPumpExit := startEventPump(t, m, proc)

	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	proc.ch <- taskUpdated(parentID, taskID, msg.TaskStatusCompleted)
	close(proc.ch)
	waitForPumpExit()

	sub := findSubagent(t, m, parentID)
	if sub.ControlledBy != msg.ControlledByHarness {
		t.Fatalf("controlled_by = %q, want %q — every resume/message/kill path gates on this, and without it the watchdog starts a rogue agent",
			sub.ControlledBy, msg.ControlledByHarness)
	}
	// The dedupe key is what a resume destroyed. It must be the rollout name.
	if want := "agent-" + taskID; sub.HarnessSessionID != want {
		t.Fatalf("harness_session_id = %q, want %q — discovery converges on this key, and a resume overwrites it with a fresh Claude UUID",
			sub.HarnessSessionID, want)
	}
}
