package harness

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeProcess is a HarnessProcess whose Events channel is owned by the
// test. Drives the Manager.readEvents fan-out + derivation path
// without spawning a subprocess or hitting a network.
type fakeProcess struct {
	sid string
	ch  chan msg.Event
}

func (f *fakeProcess) PID() int                                                  { return 0 }
func (f *fakeProcess) SessionID() string                                         { return f.sid }
func (f *fakeProcess) Events() <-chan msg.Event                                  { return f.ch }
func (f *fakeProcess) Send(message string, blocks []msg.ContentBlock) error     { return nil }
func (f *fakeProcess) SendCommand(cmd string) error                              { return nil }
func (f *fakeProcess) SendJSONRPC(method string, params json.RawMessage) error   { return nil }
func (f *fakeProcess) Interrupt() error                                          { return nil }
func (f *fakeProcess) Kill() error                                               { return nil }

// newTestManager returns a Manager backed by a temp SQLite store and
// a non-routable log-store URL. The log-store push fails silently
// (errors are logged, not propagated), so unit tests don't need a live
// log-store.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewManager(st, "http://127.0.0.1:0", "http://127.0.0.1:0", 0, nil)
}

// recvWithin reads up to want events from ch, failing the test if
// they don't all arrive within timeout.
func recvWithin(t *testing.T, ch chan StoredEvent, want int, timeout time.Duration) []StoredEvent {
	t.Helper()
	var got []StoredEvent
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscriber channel closed after %d events; wanted %d", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timeout waiting for %d events; received %d", want, len(got))
		}
	}
	return got
}

// TestManager_DerivesAgentStateAfterRawEvent walks one full turn
// (user_message → tool_call → tool_result → result) through the
// real readEvents path and asserts that:
//   1. raw events arrive at subscribers in order
//   2. each raw event that triggers a transition is followed by an
//      agent_state derived event whose Previous matches the prior
//      state and whose State matches the new one
//   3. the derived event is persisted (has a non-zero RowID)
func TestManager_DerivesAgentStateAfterRawEvent(t *testing.T) {
	m := newTestManager(t)

	const bridgeID = "br-derivation-test"

	// Seed a session row so UpdateSessionPID/UpdateSessionState
	// inside readEvents have something to update. The store's
	// methods are tolerant of unknown ids (they no-op), but
	// seeding keeps the test honest.
	if err := m.store.CreateSession(&store.Session{
		BridgeID: bridgeID,
		Harness:  msg.HarnessClaudeCode,
		State:    string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 16)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	feed := []msg.Event{
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"},
		{Type: msg.EventToolCall, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
			ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}},
		{Type: msg.EventToolResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
			ToolResult: &msg.ToolResultEvent{ToolID: "t1"}},
		{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
			Result: &msg.ResultEvent{Text: "done"}},
	}
	for _, ev := range feed {
		proc.ch <- ev
	}
	close(proc.ch)

	// Expected ordering on the subscriber channel:
	//   user_message
	//   agent_state(idle → tool_running)
	//   tool_call           (no transition)
	//   tool_result         (no transition)
	//   result
	//   agent_state(tool_running → idle)
	//   usage_total          (emitted on every result; child 3)
	//   turn_complete        (emitted after usage_total on terminator; child 4)
	got := recvWithin(t, sub, 8, 2*time.Second)

	wantOrder := []msg.EventType{
		msg.EventUserMessage,
		msg.EventAgentState,
		msg.EventToolCall,
		msg.EventToolResult,
		msg.EventResult,
		msg.EventAgentState,
		msg.EventUsageTotal,
		msg.EventTurnComplete,
	}
	for i, ev := range got {
		if ev.Type != wantOrder[i] {
			t.Fatalf("event %d: got type %q; want %q", i, ev.Type, wantOrder[i])
		}
	}

	// The two agent_state derived events should reflect
	// idle→tool_running and tool_running→idle in that order.
	first := got[1]
	if first.AgentState == nil || first.AgentState.Previous != msg.AgentStateIdle || first.AgentState.State != msg.AgentStateToolRunning {
		t.Fatalf("first agent_state body = %+v; want idle→tool_running", first.AgentState)
	}
	if first.RowID == 0 {
		t.Fatalf("first derived event has zero RowID — not persisted")
	}
	if first.BridgeSessionID != bridgeID {
		t.Fatalf("derived event missing bridge_session_id stamp: %+v", first.Event)
	}

	idleTransition := got[5]
	if idleTransition.AgentState == nil || idleTransition.AgentState.Previous != msg.AgentStateToolRunning || idleTransition.AgentState.State != msg.AgentStateIdle {
		t.Fatalf("idle agent_state body = %+v; want tool_running→idle", idleTransition.AgentState)
	}
	if idleTransition.RowID == 0 {
		t.Fatalf("idle agent_state derived event has zero RowID — not persisted")
	}

	usageTotal := got[6]
	if usageTotal.UsageTotal == nil || usageTotal.UsageTotal.Turns != 1 {
		t.Fatalf("usage_total body = %+v; want turns=1", usageTotal.UsageTotal)
	}
	if usageTotal.RowID == 0 {
		t.Fatalf("usage_total derived event has zero RowID — not persisted")
	}
	if usageTotal.BridgeSessionID != bridgeID {
		t.Fatalf("usage_total missing bridge_session_id stamp: %+v", usageTotal.Event)
	}

	turnComplete := got[7]
	if turnComplete.TurnComplete == nil || turnComplete.TurnComplete.TurnID != "turn-1" {
		t.Fatalf("turn_complete body = %+v; want turn_id=turn-1", turnComplete.TurnComplete)
	}
	if turnComplete.TurnComplete.FinalMessage != "done" {
		t.Fatalf("turn_complete final_message = %q; want %q", turnComplete.TurnComplete.FinalMessage, "done")
	}
	if len(turnComplete.TurnComplete.ToolCalls) != 1 || turnComplete.TurnComplete.ToolCalls[0].Tool != "Bash" {
		t.Fatalf("turn_complete tool_calls = %+v; want one Bash entry", turnComplete.TurnComplete.ToolCalls)
	}
	if turnComplete.RowID == 0 {
		t.Fatalf("turn_complete derived event has zero RowID — not persisted")
	}
	if turnComplete.BridgeSessionID != bridgeID {
		t.Fatalf("turn_complete missing bridge_session_id stamp: %+v", turnComplete.Event)
	}
}

// TestManager_UsageTotalSnapshotsAcrossTurns drives a recorded
// multi-turn session through readEvents and asserts that:
//  1. one usage_total event is emitted per turn
//  2. each emission carries the running session-cumulative totals
//     (not just the per-turn delta)
//  3. cost is summed across priced turns; mixed priced/unpriced
//     turns produce a usage_total whose cost reflects only the
//     priced subset (spec [OPEN] resolved to lean (a))
//  4. context_tokens is last-value-wins, not summed
//
// This is the "conformance fixture" the spec calls for: a recorded
// fixture replayed through the real manager fan-out path with
// per-result snapshots asserted.
func TestManager_UsageTotalSnapshotsAcrossTurns(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-usage-total-test"

	if err := m.store.CreateSession(&store.Session{
		BridgeID: bridgeID,
		Harness:  msg.HarnessClaudeCode,
		State:    string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 16)}
	sub := m.Subscribe(bridgeID)
	go m.readEvents(proc)

	// Turn 1: priced.
	// Turn 2: unpriced (cost stays at the turn-1 priced subtotal).
	// Turn 3: priced (cost grows; context_tokens drops, last-value-wins).
	feed := []msg.Event{
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"},
		{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1",
			Result: &msg.ResultEvent{
				Text:  "ok",
				Usage: msg.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, ContextTokens: 1000, ContextLimit: 200000},
				Cost:  &msg.Cost{TotalUSD: 0.10, InputUSD: 0.04, OutputUSD: 0.06},
			}},
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-2"},
		{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-2",
			Result: &msg.ResultEvent{
				Text:  "ok",
				Usage: msg.TokenUsage{InputTokens: 30, OutputTokens: 15, TotalTokens: 45},
				// Unpriced.
			}},
		{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-3"},
		{Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-3",
			Result: &msg.ResultEvent{
				Text:  "ok",
				Usage: msg.TokenUsage{InputTokens: 40, OutputTokens: 20, TotalTokens: 60, ContextTokens: 800, ContextLimit: 200000},
				Cost:  &msg.Cost{TotalUSD: 0.05, InputUSD: 0.02, OutputUSD: 0.03},
			}},
	}
	for _, ev := range feed {
		proc.ch <- ev
	}
	close(proc.ch)

	// Per turn we receive 6 events: user_message, agent_state
	// (idle→tool_running), result, agent_state (tool_running→idle),
	// usage_total, turn_complete. Three turns: 18 total.
	all := recvWithin(t, sub, 18, 3*time.Second)

	var totals []*msg.UsageTotalEvent
	for _, ev := range all {
		if ev.Type == msg.EventUsageTotal {
			totals = append(totals, ev.UsageTotal)
		}
	}
	if len(totals) != 3 {
		t.Fatalf("usage_total count = %d; want 3 (one per turn)", len(totals))
	}

	// Turn 1: 100/50/150 input/output/total, context=1000.
	if totals[0].Turns != 1 || totals[0].Usage.InputTokens != 100 || totals[0].Usage.TotalTokens != 150 || totals[0].Usage.ContextTokens != 1000 {
		t.Fatalf("turn-1 usage_total = %+v; want turns=1 input=100 total=150 context=1000", totals[0])
	}
	if totals[0].Cost == nil || totals[0].Cost.TotalUSD < 0.099 || totals[0].Cost.TotalUSD > 0.101 {
		t.Fatalf("turn-1 cost = %+v; want ~0.10", totals[0].Cost)
	}

	// Turn 2 (unpriced): cumulative input 100+30=130, total 150+45=195.
	// Cost stays at the turn-1 subtotal (0.10) because turn 2 was unpriced.
	// Context_tokens stays at 1000 — turn 2's zero context shouldn't clobber.
	if totals[1].Turns != 2 || totals[1].Usage.InputTokens != 130 || totals[1].Usage.TotalTokens != 195 {
		t.Fatalf("turn-2 usage_total = %+v; want turns=2 input=130 total=195", totals[1])
	}
	if totals[1].Usage.ContextTokens != 1000 {
		t.Fatalf("turn-2 context = %d; want 1000 (sticky across unpriced turn)", totals[1].Usage.ContextTokens)
	}
	if totals[1].Cost == nil || totals[1].Cost.TotalUSD < 0.099 || totals[1].Cost.TotalUSD > 0.101 {
		t.Fatalf("turn-2 cost = %+v; want still ~0.10 (turn 2 unpriced)", totals[1].Cost)
	}

	// Turn 3: cumulative input 130+40=170, context drops to 800 (last-value-wins).
	// Cost cumulative: 0.10 + 0.05 = 0.15.
	if totals[2].Turns != 3 || totals[2].Usage.InputTokens != 170 || totals[2].Usage.TotalTokens != 255 {
		t.Fatalf("turn-3 usage_total = %+v; want turns=3 input=170 total=255", totals[2])
	}
	if totals[2].Usage.ContextTokens != 800 {
		t.Fatalf("turn-3 context = %d; want 800 (last value)", totals[2].Usage.ContextTokens)
	}
	if totals[2].Cost == nil || totals[2].Cost.TotalUSD < 0.149 || totals[2].Cost.TotalUSD > 0.151 {
		t.Fatalf("turn-3 cost.total = %+v; want ~0.15 (sum of priced turns)", totals[2].Cost)
	}

	// Conformance assertion (per spec): one turn_complete per turn,
	// each carrying that turn's per-result delta — NOT the running
	// session totals. UsageDelta is the easiest invariant to anchor:
	// it equals the originating ResultEvent.Usage exactly.
	var turnCompletes []*msg.TurnCompleteEvent
	var turnCompleteTurnIDs []string
	for _, ev := range all {
		if ev.Type == msg.EventTurnComplete {
			turnCompletes = append(turnCompletes, ev.TurnComplete)
			turnCompleteTurnIDs = append(turnCompleteTurnIDs, ev.TurnID)
		}
	}
	if len(turnCompletes) != 3 {
		t.Fatalf("turn_complete count = %d; want 3 (one per turn)", len(turnCompletes))
	}
	wantTurnIDs := []string{"turn-1", "turn-2", "turn-3"}
	for i, want := range wantTurnIDs {
		if turnCompleteTurnIDs[i] != want {
			t.Fatalf("turn_complete[%d].TurnID = %q; want %q", i, turnCompleteTurnIDs[i], want)
		}
		if turnCompletes[i].TurnID != want {
			t.Fatalf("turn_complete[%d].body.TurnID = %q; want %q", i, turnCompletes[i].TurnID, want)
		}
		if turnCompletes[i].FinalMessage != "ok" {
			t.Fatalf("turn_complete[%d].FinalMessage = %q; want %q", i, turnCompletes[i].FinalMessage, "ok")
		}
	}
	// Turn 1 delta: priced 100/50.
	if turnCompletes[0].UsageDelta.InputTokens != 100 || turnCompletes[0].UsageDelta.OutputTokens != 50 {
		t.Fatalf("turn-1 turn_complete usage_delta = %+v; want input=100 output=50", turnCompletes[0].UsageDelta)
	}
	if turnCompletes[0].Cost == nil || turnCompletes[0].Cost.TotalUSD < 0.099 || turnCompletes[0].Cost.TotalUSD > 0.101 {
		t.Fatalf("turn-1 turn_complete cost = %+v; want ~0.10 (this turn's cost, not cumulative)", turnCompletes[0].Cost)
	}
	// Turn 2 delta: 30/15, unpriced.
	if turnCompletes[1].UsageDelta.InputTokens != 30 || turnCompletes[1].UsageDelta.OutputTokens != 15 {
		t.Fatalf("turn-2 turn_complete usage_delta = %+v; want input=30 output=15", turnCompletes[1].UsageDelta)
	}
	if turnCompletes[1].Cost != nil {
		t.Fatalf("turn-2 turn_complete cost = %+v; want nil (unpriced turn)", turnCompletes[1].Cost)
	}
	// Turn 3 delta: 40/20.
	if turnCompletes[2].UsageDelta.InputTokens != 40 || turnCompletes[2].UsageDelta.OutputTokens != 20 {
		t.Fatalf("turn-3 turn_complete usage_delta = %+v; want input=40 output=20", turnCompletes[2].UsageDelta)
	}
	if turnCompletes[2].Cost == nil || turnCompletes[2].Cost.TotalUSD < 0.049 || turnCompletes[2].Cost.TotalUSD > 0.051 {
		t.Fatalf("turn-3 turn_complete cost = %+v; want ~0.05", turnCompletes[2].Cost)
	}
	for i, tc := range turnCompletes {
		if tc.IsError {
			t.Fatalf("turn_complete[%d].IsError = true; want false on success path", i)
		}
		if len(tc.ToolCalls) != 0 {
			t.Fatalf("turn_complete[%d].ToolCalls = %+v; want empty for tool-less turns", i, tc.ToolCalls)
		}
	}
}

// TestManager_DerivationStateTornDownOnProcessExit verifies that the
// derivation map entry created by deriveAndBroadcast is cleaned up
// when readEvents returns (process channel closed). Otherwise long-
// running servers would leak per-session derivation entries forever.
func TestManager_DerivationStateTornDownOnProcessExit(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-cleanup-test"

	if err := m.store.CreateSession(&store.Session{
		BridgeID: bridgeID,
		Harness:  msg.HarnessClaudeCode,
		State:    string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	proc := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 4)}
	done := make(chan struct{})
	go func() {
		m.readEvents(proc)
		close(done)
	}()

	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode}

	// Drain the subscriber-less fan-out by waiting briefly for
	// readEvents to process the event, then close.
	time.Sleep(50 * time.Millisecond)

	m.mu.RLock()
	_, hadEntry := m.derivation[bridgeID]
	m.mu.RUnlock()
	if !hadEntry {
		t.Fatalf("derivation entry not created after first event")
	}

	close(proc.ch)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("readEvents did not return within timeout")
	}

	m.mu.RLock()
	_, leaked := m.derivation[bridgeID]
	m.mu.RUnlock()
	if leaked {
		t.Fatalf("derivation entry leaked after process exit")
	}
}

// TestManager_RecoversTurnIDAfterProcessRestart simulates a process exit
// mid-turn (which deletes the in-memory msgState) and asserts that the
// next event arriving after a "restart" is stamped with the original
// TurnID/MessageID instead of being left blank until the next
// user_message. Regression for bridge-ui's TurnsView grouping breaking
// after Claude Code resumes a long-running turn.
func TestManager_RecoversTurnIDAfterProcessRestart(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-recover-test"

	if err := m.store.CreateSession(&store.Session{
		BridgeID: bridgeID,
		Harness:  msg.HarnessClaudeCode,
		State:    string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// First process: user_message + one block → channel closes
	// without a result, mirroring a harness restart mid-turn.
	proc1 := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 4)}
	done1 := make(chan struct{})
	go func() {
		m.readEvents(proc1)
		close(done1)
	}()

	proc1.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode}
	proc1.ch <- msg.Event{
		Type: msg.EventBlock, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
		Block: &msg.BlockEvent{MessageID: "h_pre"},
	}
	time.Sleep(50 * time.Millisecond)
	close(proc1.ch)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatalf("readEvents 1 did not return")
	}

	// In-memory state is gone now (manager.go:525 deletes msgState
	// on process exit). Second process feeds another block as if the
	// harness resumed the same turn.
	m.mu.RLock()
	_, stillThere := m.msgState[bridgeID]
	m.mu.RUnlock()
	if stillThere {
		t.Fatalf("msgState should be cleared after process exit")
	}

	proc2 := &fakeProcess{sid: bridgeID, ch: make(chan msg.Event, 4)}
	sub := m.Subscribe(bridgeID)
	done2 := make(chan struct{})
	go func() {
		m.readEvents(proc2)
		close(done2)
	}()

	proc2.ch <- msg.Event{
		Type: msg.EventBlock, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
		Block: &msg.BlockEvent{MessageID: "h_post"},
	}

	got := recvWithin(t, sub, 1, 2*time.Second)
	close(proc2.ch)
	<-done2

	post := got[0].Event
	if post.TurnID == "" {
		t.Fatalf("post-restart block has empty TurnID; recovery did not run")
	}

	// The recovered TurnID must match the one stamped on the
	// pre-restart block — same logical turn, same id.
	pre, err := m.store.ListEventsSinceID(bridgeID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var preTurnID string
	for _, ev := range pre {
		var parsed msg.Event
		if err := json.Unmarshal(ev.Data, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if parsed.Type == msg.EventBlock && parsed.HarnessMessageID == "h_pre" {
			preTurnID = parsed.TurnID
			break
		}
	}
	if preTurnID == "" {
		t.Fatalf("pre-restart block missing from store")
	}
	if post.TurnID != preTurnID {
		t.Errorf("post-restart TurnID = %q, want %q (same turn)", post.TurnID, preTurnID)
	}
}
