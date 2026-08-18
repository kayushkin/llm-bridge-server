package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func (f *fakeProcess) PID() int                                                { return 0 }
func (f *fakeProcess) SessionID() string                                       { return f.sid }
func (f *fakeProcess) Events() <-chan msg.Event                                { return f.ch }
func (f *fakeProcess) Send(message string, blocks []msg.ContentBlock) error    { return nil }
func (f *fakeProcess) SendCommand(cmd string) error                            { return nil }
func (f *fakeProcess) SendJSONRPC(method string, params json.RawMessage) error { return nil }
func (f *fakeProcess) Interrupt() error                                        { return nil }
func (f *fakeProcess) Kill() error                                             { return nil }

// newTestManager returns a Manager backed by a temp SQLite store and an
// in-process log-store stub served via httptest. RecoverInFlightTurn /
// InterruptedTurn / RecentTurnTexts / ListToolCallInputs all read from
// log-store post-II.A-cutover, so unit tests need a live log-store
// backend. The stub keeps events in memory keyed by session_id and
// implements just the three endpoints the cutover read paths use:
// POST /api/v1/events, GET /api/v1/sessions/{id}/turn-state, and
// GET /api/v1/sessions/{id}/events.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ls := httptest.NewServer(newLogStoreStub())
	t.Cleanup(ls.Close)

	return NewManager(st, ls.URL, "http://127.0.0.1:0", "", 0, nil)
}

// logStoreStub is a minimal in-memory implementation of the log-store HTTP
// surface bridge-server's read paths depend on. Not safe for production —
// strictly a unit-test fixture.
type logStoreStub struct {
	mu     sync.Mutex
	events map[string][]storedStubEvent // session_id → events in insertion order
	nextID int64
}

type storedStubEvent struct {
	id   int64
	typ  string
	data []byte
}

func newLogStoreStub() *logStoreStub {
	return &logStoreStub{events: map[string][]storedStubEvent{}}
}

func (s *logStoreStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/events":
		s.handleIngest(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/turn-state"):
		s.handleTurnState(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
		s.handleEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *logStoreStub) handleIngest(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Type            string `json:"type"`
		BridgeSessionID string `json:"bridge_session_id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.BridgeSessionID == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.events[probe.BridgeSessionID] = append(s.events[probe.BridgeSessionID], storedStubEvent{
		id: s.nextID, typ: probe.Type, data: body,
	})
	json.NewEncoder(w).Encode(map[string]int64{"id": s.nextID})
}

func (s *logStoreStub) handleTurnState(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	id = strings.TrimSuffix(id, "/turn-state")
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastUser, lastTerm int64
	for _, e := range s.events[id] {
		switch e.typ {
		case "user_message":
			if e.id > lastUser {
				lastUser = e.id
			}
		case "result", "error":
			if e.id > lastTerm {
				lastTerm = e.id
			}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"last_user_message_event_id": lastUser,
		"last_terminator_event_id":   lastTerm,
		"in_flight":                  lastUser > lastTerm && lastUser > 0,
	})
}

func (s *logStoreStub) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	id = strings.TrimSuffix(id, "/events")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	typeFilter := map[string]bool{}
	if raw := r.URL.Query().Get("types"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			typeFilter[strings.TrimSpace(t)] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []json.RawMessage{}
	for _, e := range s.events[id] {
		if e.id <= after {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[e.typ] {
			continue
		}
		// Splice event_id in like the real log-store does.
		spliced := []byte(fmt.Sprintf(`{"event_id":%d,%s`, e.id, e.data[1:]))
		out = append(out, spliced)
	}
	json.NewEncoder(w).Encode(out)
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

// TestManager_DerivesSessionStateAfterRawEvent walks one full turn
// (user_message → tool_call → tool_result → result) through the
// real readEvents path and asserts that:
//  1. raw events arrive at subscribers in order
//  2. each raw event that triggers a transition is followed by a
//     session_state derived event whose Previous matches the prior
//     state and whose State matches the new one
//  3. the derived event is persisted (has a non-zero RowID)
func TestManager_DerivesSessionStateAfterRawEvent(t *testing.T) {
	m := newTestManager(t)

	const bridgeID = "br-derivation-test"

	// Seed a session row so UpdateSessionPID/UpdateSessionState
	// inside readEvents have something to update. The store's
	// methods are tolerant of unknown ids (they no-op), but
	// seeding keeps the test honest.
	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
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

	got := recvWithin(t, sub, 10, 2*time.Second)

	// Every raw event that moves the state machine is now followed by its
	// own session_state. The turn opens in model_generating, the tool_call
	// takes it to tool_running, the tool_result hands it back to
	// model_generating, and the result settles it to idle — four
	// transitions where the single-state machine emitted two.
	wantOrder := []msg.EventType{
		msg.EventUserMessage,
		msg.EventSessionState,
		msg.EventToolCall,
		msg.EventSessionState,
		msg.EventToolResult,
		msg.EventSessionState,
		msg.EventResult,
		msg.EventSessionState,
		msg.EventUsageTotal,
		msg.EventTurnComplete,
	}
	for i, ev := range got {
		if ev.Type != wantOrder[i] {
			t.Fatalf("event %d: got type %q; want %q", i, ev.Type, wantOrder[i])
		}
	}

	// Previous on the first transition is "running" (not idle) because the
	// derivation seeds its prev from the persisted sessions.state row,
	// which this test created as SessionRunning (F1 settle fix, Part 1).
	first := got[1]
	if first.State == nil || first.State.Previous != msg.SessionRunning || first.State.State != msg.SessionModelGenerating {
		t.Fatalf("first session_state body = %+v; want running→model_generating", first.State)
	}
	if first.RowID == 0 {
		t.Fatalf("first derived event has zero RowID — not persisted")
	}
	if first.BridgeSessionID != bridgeID {
		t.Fatalf("derived event missing bridge_session_id stamp: %+v", first.Event)
	}

	toolTransition := got[3]
	if toolTransition.State == nil || toolTransition.State.Previous != msg.SessionModelGenerating || toolTransition.State.State != msg.SessionToolRunning {
		t.Fatalf("tool session_state body = %+v; want model_generating→tool_running", toolTransition.State)
	}

	drainTransition := got[5]
	if drainTransition.State == nil || drainTransition.State.Previous != msg.SessionToolRunning || drainTransition.State.State != msg.SessionModelGenerating {
		t.Fatalf("drain session_state body = %+v; want tool_running→model_generating", drainTransition.State)
	}

	idleTransition := got[7]
	if idleTransition.State == nil || idleTransition.State.Previous != msg.SessionModelGenerating || idleTransition.State.State != msg.SessionIdle {
		t.Fatalf("idle session_state body = %+v; want model_generating→idle", idleTransition.State)
	}
	if idleTransition.RowID == 0 {
		t.Fatalf("idle session_state derived event has zero RowID — not persisted")
	}

	usageTotal := got[8]
	if usageTotal.UsageTotal == nil || usageTotal.UsageTotal.Turns != 1 {
		t.Fatalf("usage_total body = %+v; want turns=1", usageTotal.UsageTotal)
	}
	if usageTotal.RowID == 0 {
		t.Fatalf("usage_total derived event has zero RowID — not persisted")
	}
	if usageTotal.BridgeSessionID != bridgeID {
		t.Fatalf("usage_total missing bridge_session_id stamp: %+v", usageTotal.Event)
	}

	turnComplete := got[9]
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
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
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
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
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
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
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

	// In-memory state is gone now (readEvents deletes msgState in its
	// cleanup tail on process exit). Second process feeds another block as if the
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

// firstSessionState returns the first EventSessionState in got, or nil.
func firstStoredSessionState(got []StoredEvent) *StoredEvent {
	for i := range got {
		if got[i].Type == msg.EventSessionState {
			return &got[i]
		}
	}
	return nil
}

// TestManager_SeedsDerivationPrevFromStateAfterRestart is the F1 settle
// bug, Part 1. A fresh derivation (as after a bridge-server restart)
// must seed its prev from the persisted sessions.state row, so the next
// EventResult produces a real tool_running→idle transition that corrects
// the persisted row — instead of resetting prev to idle, deriving
// idle==idle, suppressing the transition, and leaving the row stuck at
// tool_running forever.
func TestManager_SeedsDerivationPrevFromStateAfterRestart(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-f1-seed"

	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionRunning),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// Persisted row is holding at tool_running (as a live turn would be)
	// with NO in-memory derivation entry — the post-restart condition.
	if err := m.store.UpdateSessionState(bridgeID, string(msg.SessionToolRunning)); err != nil {
		t.Fatalf("set tool_running: %v", err)
	}
	m.mu.RLock()
	_, exists := m.derivation[bridgeID]
	m.mu.RUnlock()
	if exists {
		t.Fatalf("precondition: no derivation entry should exist yet")
	}

	sub := m.Subscribe(bridgeID)
	m.deriveAndBroadcast(bridgeID, &msg.Event{
		Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
		TurnID: "turn-x", Result: &msg.ResultEvent{Text: "done"},
	})

	// Expect a session_state(tool_running→idle) transition — proving the
	// derivation seeded prev from the persisted tool_running row.
	got := recvWithin(t, sub, 2, 2*time.Second)
	ss := firstStoredSessionState(got)
	if ss == nil {
		t.Fatalf("no session_state event emitted; got %+v", eventTypes(got))
	}
	if ss.State.Previous != msg.SessionToolRunning || ss.State.State != msg.SessionIdle {
		t.Fatalf("session_state = %+v; want tool_running→idle", ss.State)
	}

	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.State != string(msg.SessionIdle) {
		t.Fatalf("persisted state = %q; want idle (row must be corrected)", sess.State)
	}
}

// TestManager_ReconcilesStaleSettledRowWhenTransitionSuppressed is the F1
// settle bug, Part 2. Even when the in-memory prev already matches the
// settled state (so derive() suppresses the transition), a persisted row
// that was written directly to a holding value (a send/resume path, or a
// pre-seed restart) must be reconciled to the settled truth on the
// terminal event — with a broadcast so live consumers converge.
func TestManager_ReconcilesStaleSettledRowWhenTransitionSuppressed(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-f1-reconcile"

	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionIdle),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// In-memory derivation already settled at idle...
	m.mu.Lock()
	m.derivation[bridgeID] = newDerivationStateSeeded(msg.SessionIdle)
	m.mu.Unlock()
	// ...but the persisted row was written directly to a holding value,
	// bypassing derivation (as manager.Start/Resume do).
	if err := m.store.UpdateSessionState(bridgeID, string(msg.SessionToolRunning)); err != nil {
		t.Fatalf("set tool_running: %v", err)
	}

	sub := m.Subscribe(bridgeID)
	m.deriveAndBroadcast(bridgeID, &msg.Event{
		Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
		TurnID: "turn-y", Result: &msg.ResultEvent{Text: "done"},
	})

	// usage_total (from the loop) + the reconcile session_state (after).
	got := recvWithin(t, sub, 2, 2*time.Second)
	ss := firstStoredSessionState(got)
	if ss == nil {
		t.Fatalf("reconcile did not broadcast a session_state; got %+v", eventTypes(got))
	}
	if ss.State.Previous != msg.SessionToolRunning || ss.State.State != msg.SessionIdle {
		t.Fatalf("reconcile session_state = %+v; want tool_running→idle", ss.State)
	}
	if ss.State.Reason != "turn_settled_reconcile" {
		t.Fatalf("reconcile reason = %q; want turn_settled_reconcile", ss.State.Reason)
	}

	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.State != string(msg.SessionIdle) {
		t.Fatalf("persisted state = %q; want idle (stale holding row must be corrected)", sess.State)
	}
}

// TestManager_NoReconcileWhenSettledRowAlreadyMatches is the F1 no-op
// guard. When the persisted row already agrees with the settled state,
// a terminal event whose transition is suppressed must NOT trigger a
// redundant UpdateSessionState or a spurious session_state broadcast.
func TestManager_NoReconcileWhenSettledRowAlreadyMatches(t *testing.T) {
	m := newTestManager(t)
	const bridgeID = "br-f1-noop"

	if err := m.store.CreateSession(&store.Session{
		SessionID: bridgeID,
		Harness:   msg.HarnessClaudeCode,
		State:     string(msg.SessionIdle),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	m.mu.Lock()
	m.derivation[bridgeID] = newDerivationStateSeeded(msg.SessionIdle)
	m.mu.Unlock()

	sub := m.Subscribe(bridgeID)
	m.deriveAndBroadcast(bridgeID, &msg.Event{
		Type: msg.EventResult, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode,
		TurnID: "turn-z", Result: &msg.ResultEvent{Text: "done"},
	})

	// Only usage_total should arrive; assert no session_state follows.
	first := recvWithin(t, sub, 1, 2*time.Second)
	if first[0].Type != msg.EventUsageTotal {
		t.Fatalf("first event = %q; want usage_total", first[0].Type)
	}
	select {
	case ev := <-sub:
		if ev.Type == msg.EventSessionState {
			t.Fatalf("unexpected session_state broadcast on no-op settle: %+v", ev.State)
		}
	case <-time.After(200 * time.Millisecond):
		// No further event — correct.
	}
}

// eventTypes is a small helper for failure messages.
func eventTypes(got []StoredEvent) []msg.EventType {
	out := make([]msg.EventType, len(got))
	for i := range got {
		out[i] = got[i].Type
	}
	return out
}

// ForceSessionState is the path an interrupt takes. The point of routing it
// through the manager rather than writing the session row directly is that
// SSE subscribers hear it: only derive() broadcasts, and a user pressing Stop
// produces no harness event to derive from. Before this, every client learned
// about an interrupt by refetching — which is why bridge-ui kept its own
// localStorage set of interrupted session ids.
func TestManager_ForceSessionStateBroadcastsAndPersists(t *testing.T) {
	m := newTestManager(t)

	const bridgeID = "br_force_state"
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

	// Open a turn so there is a live derivation holding a real state.
	proc.ch <- msg.Event{Type: msg.EventUserMessage, BridgeSessionID: bridgeID, Harness: msg.HarnessClaudeCode, TurnID: "turn-1"}
	got := recvWithin(t, sub, 2, 2*time.Second)
	if got[1].State == nil || got[1].State.State != msg.SessionModelGenerating {
		t.Fatalf("turn open = %+v; want model_generating", got[1].State)
	}

	if !m.ForceSessionState(bridgeID, msg.SessionPaused, "user_interrupt") {
		t.Fatal("ForceSessionState reported no change; want changed")
	}

	forced := recvWithin(t, sub, 1, 2*time.Second)
	if forced[0].Type != msg.EventSessionState || forced[0].State == nil {
		t.Fatalf("broadcast event = %+v; want session_state", forced[0].Event)
	}
	if forced[0].State.State != msg.SessionPaused || forced[0].State.Previous != msg.SessionModelGenerating {
		t.Fatalf("broadcast body = %+v; want model_generating→paused", forced[0].State)
	}
	if forced[0].State.Reason != "user_interrupt" {
		t.Fatalf("broadcast reason = %q; want user_interrupt", forced[0].State.Reason)
	}
	if forced[0].RowID == 0 {
		t.Fatal("forced session_state has zero RowID — not persisted")
	}

	sess, err := m.store.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.State != string(msg.SessionPaused) {
		t.Fatalf("persisted state = %q; want paused", sess.State)
	}

	// Forcing the state it already holds must not emit a second event.
	if m.ForceSessionState(bridgeID, msg.SessionPaused, "user_interrupt") {
		t.Fatal("re-forcing the held state reported a change; want none")
	}

	close(proc.ch)
}
