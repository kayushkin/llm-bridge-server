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
		System: &msg.SystemEvent{Subtype: "task_started", ToolUseID: toolUseID, TaskID: taskID, Description: description},
	}
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
	go m.readEvents(proc)

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
	waitForProcessTeardown(t, m, parentID)

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
	go m.readEvents(proc)

	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	proc.ch <- taskUpdated(parentID, taskID, "completed")
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

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
	go m.readEvents(proc)

	proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	close(proc.ch) // process exits with the task still open
	waitForProcessTeardown(t, m, parentID)

	if got := findSubagent(t, m, parentID).State; got != string(msg.SessionError) {
		t.Fatalf("abandoned subagent state = %q, want error", got)
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
	go m.readEvents(proc)

	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode,
		HarnessParentID: "toolu_never_announced",
		Block:           &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "orphan frame"}}}}
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

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
	go m.readEvents(proc)

	// The subagent's frame arrives FIRST carrying the parent's harness UUID —
	// the ordering that would poison the subagent row if routing were ignored.
	started := taskStarted(parentID, toolUseID, taskID, "probe")
	started.HarnessSessionID = parentUUID
	proc.ch <- started
	proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode,
		HarnessParentID: toolUseID, HarnessSessionID: parentUUID,
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}
	close(proc.ch)
	waitForProcessTeardown(t, m, parentID)

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

// waitForProcessTeardown blocks until readEvents has finished draining and torn
// down the process, which is when subagent rows reach their final state.
func waitForProcessTeardown(t *testing.T, m *Manager, parentID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		_, running := m.processes[parentID]
		m.mu.RUnlock()
		if !running {
			// readEvents deletes from m.processes as its last map operation
			// before the final store writes; give those a moment to land.
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("readEvents did not tear down process for %s", parentID)
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
			go m.readEvents(proc)

			proc.ch <- taskStarted(parentID, toolUseID, taskID, "probe")
			proc.ch <- msg.Event{Type: msg.EventBlock, BridgeSessionID: parentID, Harness: msg.HarnessClaudeCode, HarnessParentID: toolUseID,
				Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "work"}}}}

			ev := taskUpdated(parentID, taskID, msg.TaskStatusCompleted)
			tc.mutate(&ev)
			proc.ch <- ev
			close(proc.ch)
			waitForProcessTeardown(t, m, parentID)

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
