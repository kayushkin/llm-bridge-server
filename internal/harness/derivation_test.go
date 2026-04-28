package harness

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// agentStateOf is a small helper that drives ev through the state
// machine and returns the (possibly empty) sequence of agent_state
// values the derivation emitted. Non-agent_state derived events
// (usage_total, turn_complete) are filtered out so this helper stays
// focused on the state-machine projection.
func agentStateOf(t *testing.T, d *derivationState, evs []msg.Event) []msg.AgentState {
	t.Helper()
	var emitted []msg.AgentState
	for i := range evs {
		for _, derived := range d.derive(&evs[i]) {
			if derived.Type != msg.EventAgentState {
				continue
			}
			if derived.AgentState == nil {
				t.Fatalf("derive: agent_state event with nil body: %+v", derived)
			}
			emitted = append(emitted, derived.AgentState.State)
		}
	}
	return emitted
}

// firstAgentState returns the first agent_state event in a derived
// sequence, or nil if none is present. Mirrors how callers used to
// read got[0] back when derive() produced at most one event.
func firstAgentState(out []msg.Event) *msg.Event {
	for i := range out {
		if out[i].Type == msg.EventAgentState {
			return &out[i]
		}
	}
	return nil
}

func TestDerivation_InitialState(t *testing.T) {
	d := newDerivationState()
	if d.agentState != msg.AgentStateIdle {
		t.Fatalf("initial state = %q; want %q", d.agentState, msg.AgentStateIdle)
	}
}

func TestDerivation_SimpleHappyPath(t *testing.T) {
	d := newDerivationState()
	got := agentStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}},
		{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t1"}},
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
	})
	want := []msg.AgentState{
		msg.AgentStateToolRunning, // user_message starts the turn
		msg.AgentStateIdle,        // result closes the turn
	}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
}

func TestDerivation_NoTransitionEmitsNoEvent(t *testing.T) {
	d := newDerivationState()
	// Two tool_calls back to back — second one should not emit, since
	// state is already tool_running.
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	first := d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}})
	second := d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t2", Name: "Read"}})
	if len(first) != 0 {
		t.Fatalf("tool_call after user_message should not transition (already tool_running); got %v", first)
	}
	if len(second) != 0 {
		t.Fatalf("second tool_call should not transition; got %v", second)
	}
}

func TestDerivation_MultipleConcurrentTools(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}})
	d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t2", Name: "Read"}})

	// First tool returns — still one in flight, should not transition.
	if got := d.derive(&msg.Event{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t1"}}); len(got) != 0 {
		t.Fatalf("first tool_result with another in flight: got %v; want no transition", got)
	}
	if d.agentState != msg.AgentStateToolRunning {
		t.Fatalf("state mid-flight = %q; want %q", d.agentState, msg.AgentStateToolRunning)
	}

	// Second tool returns; turn isn't done until result lands.
	if got := d.derive(&msg.Event{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t2"}}); len(got) != 0 {
		t.Fatalf("last tool_result before result: got %v; want no transition (waits for result)", got)
	}

	// Result closes the turn.
	got := d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{}})
	as := firstAgentState(got)
	if as == nil || as.AgentState.State != msg.AgentStateIdle {
		t.Fatalf("result transition = %+v; want idle agent_state", got)
	}
	if len(d.activeTools) != 0 {
		t.Fatalf("activeTools after result = %v; want empty", d.activeTools)
	}
}

func TestDerivation_ApprovalOverridesToolRunning(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}})

	// Approval requested — overrides tool_running.
	got := d.derive(&msg.Event{Type: msg.EventApproval, Approval: &msg.ApprovalEvent{Status: "pending"}})
	if len(got) != 1 || got[0].AgentState.State != msg.AgentStateAwaitingInput {
		t.Fatalf("pending approval: got %+v; want awaiting_input", got)
	}
	if !d.awaitingApproval {
		t.Fatalf("awaitingApproval flag not set after pending approval")
	}

	// Approved — should restore tool_running (the pre-approval state).
	got = d.derive(&msg.Event{Type: msg.EventApproval, Approval: &msg.ApprovalEvent{Status: "approved"}})
	if len(got) != 1 || got[0].AgentState.State != msg.AgentStateToolRunning {
		t.Fatalf("approved: got %+v; want tool_running", got)
	}
	if d.awaitingApproval {
		t.Fatalf("awaitingApproval flag still set after resolution")
	}

	// Tool result + result close the turn cleanly.
	d.derive(&msg.Event{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t1"}})
	got = d.derive(&msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{}})
	as := firstAgentState(got)
	if as == nil || as.AgentState.State != msg.AgentStateIdle {
		t.Fatalf("final result: got %+v; want idle agent_state", got)
	}
}

func TestDerivation_ApprovalDeniedRestoresToolRunning(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	d.derive(&msg.Event{Type: msg.EventApproval, Approval: &msg.ApprovalEvent{Status: "pending"}})

	got := d.derive(&msg.Event{Type: msg.EventApproval, Approval: &msg.ApprovalEvent{Status: "denied"}})
	if len(got) != 1 || got[0].AgentState.State != msg.AgentStateToolRunning {
		t.Fatalf("denied: got %+v; want tool_running (turn continues)", got)
	}
}

func TestDerivation_AutoApprovedNoPriorPendingIsNoop(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})

	// Pre-resolved approvals (e.g. cline auto_approved without a
	// preceding pending) shouldn't bounce the state machine.
	if got := d.derive(&msg.Event{Type: msg.EventApproval, Approval: &msg.ApprovalEvent{Status: "auto_approved"}}); len(got) != 0 {
		t.Fatalf("auto_approved with no prior pending: got %+v; want no transition", got)
	}
	if d.agentState != msg.AgentStateToolRunning {
		t.Fatalf("state after auto_approved noop = %q; want %q", d.agentState, msg.AgentStateToolRunning)
	}
}

func TestDerivation_ErrorTermination(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}})

	got := d.derive(&msg.Event{Type: msg.EventError, Error: &msg.ErrorEvent{Code: "boom"}})
	if len(got) != 1 || got[0].AgentState.State != msg.AgentStateError {
		t.Fatalf("error: got %+v; want error", got)
	}
	if len(d.activeTools) != 0 {
		t.Fatalf("activeTools after error = %v; want empty", d.activeTools)
	}
}

func TestDerivation_AbortedSessionReturnsToIdle(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	d.derive(&msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}})

	got := d.derive(&msg.Event{
		Type:  msg.EventSessionState,
		State: &msg.StateEvent{State: msg.SessionAborted},
	})
	if len(got) != 1 || got[0].AgentState.State != msg.AgentStateIdle {
		t.Fatalf("aborted: got %+v; want idle", got)
	}
}

func TestDerivation_StreamEventsAreNotTransitions(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})

	// EventStream / EventThinking / EventSystem should all be
	// no-ops — they don't appear in the transition table.
	for _, t2 := range []msg.EventType{msg.EventStream, msg.EventThinking, msg.EventSystem, msg.EventPlan, msg.EventSessionInfo, msg.EventHook} {
		if got := d.derive(&msg.Event{Type: t2}); len(got) != 0 {
			t.Errorf("%s: got transition %+v; want none", t2, got)
		}
	}
}

func TestDerivation_TransitionEventsCarryCauseCorrelation(t *testing.T) {
	d := newDerivationState()
	got := d.derive(&msg.Event{
		Type:            msg.EventUserMessage,
		Harness:          msg.HarnessClaudeCode,
		BridgeSessionID:  "br-1",
		HarnessSessionID: "sess-1",
		TurnID:           "turn-9",
		ClientRequestID:  "cr-1",
	})
	if len(got) != 1 {
		t.Fatalf("expected one derived event, got %d", len(got))
	}
	ev := got[0]
	if ev.BridgeSessionID != "br-1" || ev.HarnessSessionID != "sess-1" || ev.TurnID != "turn-9" || ev.ClientRequestID != "cr-1" || ev.Harness != msg.HarnessClaudeCode {
		t.Fatalf("derived event lost cause correlation: %+v", ev)
	}
	if ev.AgentState.Previous != msg.AgentStateIdle || ev.AgentState.State != msg.AgentStateToolRunning {
		t.Fatalf("agent_state body = %+v; want idle→tool_running", ev.AgentState)
	}
	if ev.Timestamp.IsZero() {
		t.Fatalf("derived event has zero Timestamp")
	}
}

func equalStates(a, b []msg.AgentState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// derivedOf collects the full derived event sequence (any types) for a
// feed of inbound events.
func derivedOf(d *derivationState, evs []msg.Event) []msg.Event {
	var out []msg.Event
	for i := range evs {
		out = append(out, d.derive(&evs[i])...)
	}
	return out
}

// sameUsage compares two TokenUsage values on the fields the
// derivation layer manages. The Overflow map makes TokenUsage
// non-comparable with `==`, so we compare numeric fields directly.
func sameUsage(a, b msg.TokenUsage) bool {
	return a.InputTokens == b.InputTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.TotalTokens == b.TotalTokens &&
		a.CacheReadTokens == b.CacheReadTokens &&
		a.CacheWriteTokens == b.CacheWriteTokens &&
		a.ReasoningTokens == b.ReasoningTokens &&
		a.ContextTokens == b.ContextTokens &&
		a.ContextLimit == b.ContextLimit
}

// usageTotalEvents filters a derived stream down to just the
// usage_total events, in order.
func usageTotalEvents(out []msg.Event) []*msg.UsageTotalEvent {
	var got []*msg.UsageTotalEvent
	for i := range out {
		if out[i].Type == msg.EventUsageTotal {
			got = append(got, out[i].UsageTotal)
		}
	}
	return got
}

func TestDerivation_UsageTotal_NoEmissionWithoutResult(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}},
		{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t1"}},
	})
	if len(usageTotalEvents(got)) != 0 {
		t.Fatalf("usage_total emitted before any result; got %+v", usageTotalEvents(got))
	}
	if d.turns != 0 {
		t.Fatalf("turns counter advanced without a result: %d", d.turns)
	}
}

func TestDerivation_UsageTotal_SingleTurn(t *testing.T) {
	d := newDerivationState()
	usage := msg.TokenUsage{
		InputTokens:      100,
		OutputTokens:     50,
		TotalTokens:      150,
		CacheReadTokens:  20,
		CacheWriteTokens: 10,
		ReasoningTokens:  5,
		ContextTokens:    1234,
		ContextLimit:     200000,
	}
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{Usage: usage, Cost: &msg.Cost{TotalUSD: 0.123, InputUSD: 0.05, OutputUSD: 0.073}}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 1 {
		t.Fatalf("usage_total count = %d; want 1 (got events: %+v)", len(totals), got)
	}
	ut := totals[0]
	if !sameUsage(ut.Usage, usage) {
		t.Fatalf("usage = %+v; want %+v", ut.Usage, usage)
	}
	if ut.Turns != 1 {
		t.Fatalf("turns = %d; want 1", ut.Turns)
	}
	if ut.Cost == nil || ut.Cost.TotalUSD != 0.123 {
		t.Fatalf("cost = %+v; want TotalUSD=0.123", ut.Cost)
	}
}

func TestDerivation_UsageTotal_OrderingAfterAgentState(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
	})
	// Expect: agent_state(idle→tool_running) on user_message,
	// then agent_state(tool_running→idle) AND usage_total on result.
	// Per spec lean: usage_total ordering relative to agent_state
	// is unconstrained, but turn_complete (child 4) must follow
	// usage_total. We anchor agent_state→usage_total here so child
	// 4 can slot turn_complete in after usage_total.
	if len(got) != 3 {
		t.Fatalf("derived count = %d; want 3 (got %+v)", len(got), got)
	}
	if got[0].Type != msg.EventAgentState {
		t.Fatalf("got[0].Type = %q; want agent_state", got[0].Type)
	}
	if got[1].Type != msg.EventAgentState {
		t.Fatalf("got[1].Type = %q; want agent_state (idle transition)", got[1].Type)
	}
	if got[2].Type != msg.EventUsageTotal {
		t.Fatalf("got[2].Type = %q; want usage_total", got[2].Type)
	}
}

func TestDerivation_UsageTotal_MultiTurnAccumulation(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CacheReadTokens: 1},
			Cost:  &msg.Cost{TotalUSD: 0.10, InputUSD: 0.04, OutputUSD: 0.06},
		}},
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 5, OutputTokens: 15, TotalTokens: 20, CacheReadTokens: 2, ReasoningTokens: 3},
			Cost:  &msg.Cost{TotalUSD: 0.20, InputUSD: 0.08, OutputUSD: 0.12},
		}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 2 {
		t.Fatalf("usage_total count = %d; want 2", len(totals))
	}

	first := totals[0]
	if first.Turns != 1 || first.Usage.InputTokens != 10 || first.Usage.OutputTokens != 20 {
		t.Fatalf("first usage_total = %+v; want turns=1 input=10 output=20", first)
	}

	second := totals[1]
	if second.Turns != 2 {
		t.Fatalf("second turns = %d; want 2", second.Turns)
	}
	wantUsage := msg.TokenUsage{
		InputTokens:     15,
		OutputTokens:    35,
		TotalTokens:     50,
		CacheReadTokens: 3,
		ReasoningTokens: 3,
	}
	if !sameUsage(second.Usage, wantUsage) {
		t.Fatalf("second usage = %+v; want %+v", second.Usage, wantUsage)
	}
	if second.Cost == nil || second.Cost.TotalUSD < 0.299 || second.Cost.TotalUSD > 0.301 {
		t.Fatalf("second cost.total = %+v; want ~0.30", second.Cost)
	}
}

func TestDerivation_UsageTotal_PartialCostSummation(t *testing.T) {
	// Mixed priced / unpriced turns: cost reflects only the priced
	// turns, not nil — partial cost is more useful than no cost
	// (spec [OPEN] resolved to lean (a)).
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 10, OutputTokens: 5},
			// No cost — unpriced turn (e.g. BYOK without priced model).
		}},
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 20, OutputTokens: 10},
			Cost:  &msg.Cost{TotalUSD: 0.50},
		}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 2 {
		t.Fatalf("usage_total count = %d; want 2", len(totals))
	}
	if totals[0].Cost != nil {
		t.Fatalf("cost on unpriced turn = %+v; want nil until any priced turn lands", totals[0].Cost)
	}
	if totals[1].Cost == nil || totals[1].Cost.TotalUSD != 0.50 {
		t.Fatalf("cost after priced turn = %+v; want TotalUSD=0.50", totals[1].Cost)
	}
	// Both turns counted toward usage_total.Turns regardless of cost.
	if totals[1].Turns != 2 {
		t.Fatalf("turns = %d; want 2 (unpriced turns still count)", totals[1].Turns)
	}
}

func TestDerivation_UsageTotal_ContextFieldsLastValueWins(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 10, ContextTokens: 1000, ContextLimit: 200000},
		}},
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{InputTokens: 20, ContextTokens: 1500, ContextLimit: 200000},
		}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 2 {
		t.Fatalf("usage_total count = %d; want 2", len(totals))
	}
	// Cumulative input tokens: 10 + 20 = 30.
	if totals[1].Usage.InputTokens != 30 {
		t.Fatalf("cumulative input tokens = %d; want 30", totals[1].Usage.InputTokens)
	}
	// Context tokens: last value wins (1500), NOT summed (which would be 2500).
	if totals[1].Usage.ContextTokens != 1500 {
		t.Fatalf("context tokens after second turn = %d; want 1500 (last-value-wins)", totals[1].Usage.ContextTokens)
	}
	if totals[1].Usage.ContextLimit != 200000 {
		t.Fatalf("context limit after second turn = %d; want 200000", totals[1].Usage.ContextLimit)
	}
}

func TestDerivation_UsageTotal_ContextFieldZeroDoesNotClobber(t *testing.T) {
	// A turn that doesn't report context shouldn't reset the prior
	// reading to zero. Otherwise UI gauges would flicker every other
	// result event.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Usage: msg.TokenUsage{ContextTokens: 5000, ContextLimit: 200000},
		}},
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			// No context fields — zero values.
			Usage: msg.TokenUsage{InputTokens: 10},
		}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 2 {
		t.Fatalf("usage_total count = %d; want 2", len(totals))
	}
	if totals[1].Usage.ContextTokens != 5000 {
		t.Fatalf("context tokens after no-context turn = %d; want 5000 (sticky)", totals[1].Usage.ContextTokens)
	}
}

func TestDerivation_UsageTotal_NilResultStillEmits(t *testing.T) {
	// Defensive: an EventResult with nil Result body shouldn't crash;
	// we still count the turn and emit usage_total at the current
	// totals so the consumer sees the terminator reflected.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: nil},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 1 {
		t.Fatalf("usage_total count = %d; want 1 (nil result still terminates the turn)", len(totals))
	}
	if totals[0].Turns != 1 {
		t.Fatalf("turns = %d; want 1", totals[0].Turns)
	}
}

func TestDerivation_UsageTotal_IsByokSticky(t *testing.T) {
	// Once any priced turn reported BYOK, the running cost retains
	// the flag — even if subsequent turns don't set it.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Cost: &msg.Cost{TotalUSD: 0.10, IsByok: true},
		}},
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{
			Cost: &msg.Cost{TotalUSD: 0.20, IsByok: false},
		}},
	})
	totals := usageTotalEvents(got)
	if len(totals) != 2 {
		t.Fatalf("usage_total count = %d; want 2", len(totals))
	}
	if !totals[1].Cost.IsByok {
		t.Fatalf("IsByok = false after a BYOK turn; want true (sticky)")
	}
}

func TestDerivation_UsageTotal_CarriesCauseCorrelation(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage})
	got := d.derive(&msg.Event{
		Type:            msg.EventResult,
		Harness:         msg.HarnessClaudeCode,
		SessionID:       "sess-2",
		BridgeID:        "br-2",
		TurnID:          "turn-1",
		ClientRequestID: "cr-2",
		Result:          &msg.ResultEvent{Usage: msg.TokenUsage{InputTokens: 5}},
	})
	// Should be agent_state + usage_total. (No turn_complete: this
	// Result's turn_id never appeared on a prior event, so no
	// accumulator was open to close. Terminators don't open new
	// accumulators — see recordForTurn.)
	if len(got) != 2 {
		t.Fatalf("derived = %d events; want 2", len(got))
	}
	ut := got[1]
	if ut.Type != msg.EventUsageTotal {
		t.Fatalf("second derived = %q; want usage_total", ut.Type)
	}
	if ut.SessionID != "sess-2" || ut.BridgeID != "br-2" || ut.TurnID != "turn-1" || ut.ClientRequestID != "cr-2" || ut.Harness != msg.HarnessClaudeCode {
		t.Fatalf("usage_total lost cause correlation: %+v", ut)
	}
	if ut.Timestamp.IsZero() {
		t.Fatalf("usage_total has zero Timestamp")
	}
}

// turnCompleteEvents filters a derived stream down to just the
// turn_complete events, in order.
func turnCompleteEvents(out []msg.Event) []*msg.TurnCompleteEvent {
	var got []*msg.TurnCompleteEvent
	for i := range out {
		if out[i].Type == msg.EventTurnComplete {
			got = append(got, out[i].TurnComplete)
		}
	}
	return got
}

func TestDerivation_TurnComplete_NoTurnIDNoEmission(t *testing.T) {
	// Without a turn_id, no accumulator is opened. A Result without
	// turn_id (replays, harness without turn tracking) emits
	// usage_total but no turn_complete.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "ok"}},
	})
	if tc := turnCompleteEvents(got); len(tc) != 0 {
		t.Fatalf("turn_complete emitted without turn_id: %+v", tc)
	}
}

func TestDerivation_TurnComplete_SimpleTurn(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1"},
		{Type: msg.EventResult, TurnID: "turn-1", Result: &msg.ResultEvent{
			Text:  "all done",
			Usage: msg.TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
			Cost:  &msg.Cost{TotalUSD: 0.05},
		}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1", len(tcs))
	}
	tc := tcs[0]
	if tc.TurnID != "turn-1" {
		t.Fatalf("turn_id = %q; want turn-1", tc.TurnID)
	}
	if tc.FinalMessage != "all done" {
		t.Fatalf("final_message = %q; want %q", tc.FinalMessage, "all done")
	}
	if tc.UsageDelta.InputTokens != 10 || tc.UsageDelta.TotalTokens != 30 {
		t.Fatalf("usage_delta = %+v; want this turn's per-result usage", tc.UsageDelta)
	}
	if tc.Cost == nil || tc.Cost.TotalUSD != 0.05 {
		t.Fatalf("cost = %+v; want TotalUSD=0.05", tc.Cost)
	}
	if tc.IsError {
		t.Fatalf("is_error = true; want false on success")
	}
	if len(tc.ToolCalls) != 0 {
		t.Fatalf("tool_calls = %+v; want empty for tool-less turn", tc.ToolCalls)
	}
}

func TestDerivation_TurnComplete_OrderingAfterUsageTotal(t *testing.T) {
	// Spec lean: usage_total first, turn_complete second on the same
	// terminating EventResult.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1"},
		{Type: msg.EventResult, TurnID: "turn-1", Result: &msg.ResultEvent{}},
	})
	// Expect: agent_state(idle→tool_running),
	//         agent_state(tool_running→idle),
	//         usage_total,
	//         turn_complete.
	if len(got) != 4 {
		t.Fatalf("derived count = %d; want 4 (got %+v)", len(got), got)
	}
	if got[2].Type != msg.EventUsageTotal {
		t.Fatalf("got[2].Type = %q; want usage_total", got[2].Type)
	}
	if got[3].Type != msg.EventTurnComplete {
		t.Fatalf("got[3].Type = %q; want turn_complete", got[3].Type)
	}
}

func TestDerivation_TurnComplete_MultiToolTurn(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-7"},
		{Type: msg.EventToolCall, TurnID: "turn-7", ToolCall: &msg.ToolCallEvent{
			ToolID: "t1", Name: "Bash", Input: []byte(`{"cmd":"ls"}`),
		}},
		{Type: msg.EventToolResult, TurnID: "turn-7", ToolResult: &msg.ToolResultEvent{
			ToolID: "t1", Output: "file1\nfile2",
		}},
		{Type: msg.EventToolCall, TurnID: "turn-7", ToolCall: &msg.ToolCallEvent{
			ToolID: "t2", Name: "Read", Input: []byte(`{"path":"file1"}`),
		}},
		{Type: msg.EventToolResult, TurnID: "turn-7", ToolResult: &msg.ToolResultEvent{
			ToolID: "t2", Output: "contents", IsError: true,
		}},
		{Type: msg.EventResult, TurnID: "turn-7", Result: &msg.ResultEvent{Text: "summary"}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1", len(tcs))
	}
	tc := tcs[0]
	if len(tc.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %+v; want 2 entries", tc.ToolCalls)
	}
	if tc.ToolCalls[0].Tool != "Bash" || tc.ToolCalls[0].Output != "file1\nfile2" || tc.ToolCalls[0].Error != "" {
		t.Fatalf("tool_calls[0] = %+v; want Bash with success output", tc.ToolCalls[0])
	}
	if tc.ToolCalls[0].Input != `{"cmd":"ls"}` {
		t.Fatalf("tool_calls[0].Input = %q; want raw JSON", tc.ToolCalls[0].Input)
	}
	if tc.ToolCalls[1].Tool != "Read" || tc.ToolCalls[1].Error != "contents" || tc.ToolCalls[1].Output != "" {
		t.Fatalf("tool_calls[1] = %+v; want Read with error output", tc.ToolCalls[1])
	}
}

func TestDerivation_TurnComplete_ErrorTermination(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-9"},
		{Type: msg.EventToolCall, TurnID: "turn-9", ToolCall: &msg.ToolCallEvent{
			ToolID: "t1", Name: "Bash",
		}},
		{Type: msg.EventError, TurnID: "turn-9", Error: &msg.ErrorEvent{
			Code: "boom", Message: "model unavailable",
		}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1", len(tcs))
	}
	tc := tcs[0]
	if !tc.IsError {
		t.Fatalf("is_error = false; want true on EventError termination")
	}
	if tc.ErrorMessage != "model unavailable" {
		t.Fatalf("error_message = %q; want %q", tc.ErrorMessage, "model unavailable")
	}
	if len(tc.ToolCalls) != 1 || tc.ToolCalls[0].Tool != "Bash" {
		t.Fatalf("tool_calls = %+v; want one Bash entry (in-flight call captured)", tc.ToolCalls)
	}
}

func TestDerivation_TurnComplete_ResultIsErrorFlag(t *testing.T) {
	// EventResult.IsError (success-path with an error message body)
	// should also flip turn_complete.IsError.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1"},
		{Type: msg.EventResult, TurnID: "turn-1", Result: &msg.ResultEvent{
			Text:    "model returned an error",
			IsError: true,
		}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 || !tcs[0].IsError {
		t.Fatalf("expected one is_error turn_complete; got %+v", tcs)
	}
	if tcs[0].ErrorMessage != "model returned an error" {
		t.Fatalf("error_message = %q; want propagated from Result.Text", tcs[0].ErrorMessage)
	}
}

func TestDerivation_TurnComplete_DurationFromTimestamps(t *testing.T) {
	d := newDerivationState()
	t0 := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(750 * time.Millisecond)
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1", Timestamp: t0},
		{Type: msg.EventResult, TurnID: "turn-1", Timestamp: t1, Result: &msg.ResultEvent{}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1", len(tcs))
	}
	if tcs[0].DurationMS != 750 {
		t.Fatalf("duration_ms = %d; want 750 (wall-clock delta)", tcs[0].DurationMS)
	}
}

func TestDerivation_TurnComplete_AbortedTurnNoEmission(t *testing.T) {
	// SessionAborted is not a terminator for turn_complete; the
	// accumulator stays open until either a Result/Error lands or
	// FIFO eviction kicks in. Asserts: no turn_complete on abort.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-aborted"},
		{Type: msg.EventToolCall, TurnID: "turn-aborted", ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Bash"}},
		{Type: msg.EventSessionState, State: &msg.StateEvent{State: msg.SessionAborted}},
	})
	if tc := turnCompleteEvents(got); len(tc) != 0 {
		t.Fatalf("aborted turn emitted turn_complete = %+v; want none", tc)
	}
	// And the accumulator is still open, in case a delayed
	// terminator arrives.
	if _, ok := d.turnAccums["turn-aborted"]; !ok {
		t.Fatalf("aborted turn accumulator was dropped — should remain open until terminator or eviction")
	}
}

func TestDerivation_TurnComplete_InflightEvictionLogsWarning(t *testing.T) {
	// Open more turns than maxInflightTurns without terminating any
	// of them; oldest must be evicted (and won't emit turn_complete
	// when its terminator finally arrives).
	d := newDerivationState()
	for i := 0; i < maxInflightTurns+2; i++ {
		turnID := "turn-" + string(rune('a'+i))
		d.derive(&msg.Event{Type: msg.EventUserMessage, TurnID: turnID})
	}
	if len(d.turnAccums) != maxInflightTurns {
		t.Fatalf("turnAccums size = %d; want cap=%d", len(d.turnAccums), maxInflightTurns)
	}
	// First two turns ("turn-a", "turn-b") were evicted. A late
	// terminator for an evicted turn should NOT emit turn_complete.
	got := d.derive(&msg.Event{Type: msg.EventResult, TurnID: "turn-a", Result: &msg.ResultEvent{}})
	if tc := turnCompleteEvents(got); len(tc) != 0 {
		t.Fatalf("evicted turn emitted turn_complete on late terminator: %+v", tc)
	}
}

func TestDerivation_TurnComplete_CarriesCauseCorrelation(t *testing.T) {
	d := newDerivationState()
	d.derive(&msg.Event{Type: msg.EventUserMessage, TurnID: "turn-1"})
	got := d.derive(&msg.Event{
		Type:            msg.EventResult,
		Harness:         msg.HarnessClaudeCode,
		SessionID:       "sess-1",
		BridgeID:        "br-1",
		TurnID:          "turn-1",
		ClientRequestID: "cr-1",
		Result:          &msg.ResultEvent{},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1 (got %+v)", len(tcs), got)
	}
	// Find the carrier event for cause-correlation assertions.
	var tcEvent msg.Event
	for _, ev := range got {
		if ev.Type == msg.EventTurnComplete {
			tcEvent = ev
			break
		}
	}
	if tcEvent.SessionID != "sess-1" || tcEvent.BridgeID != "br-1" || tcEvent.TurnID != "turn-1" || tcEvent.ClientRequestID != "cr-1" || tcEvent.Harness != msg.HarnessClaudeCode {
		t.Fatalf("turn_complete lost cause correlation: %+v", tcEvent)
	}
	if tcEvent.Timestamp.IsZero() {
		t.Fatalf("turn_complete has zero Timestamp")
	}
}

func TestDerivation_TurnComplete_MultipleTurns(t *testing.T) {
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1"},
		{Type: msg.EventResult, TurnID: "turn-1", Result: &msg.ResultEvent{
			Text:  "first",
			Usage: msg.TokenUsage{InputTokens: 10, OutputTokens: 20},
		}},
		{Type: msg.EventUserMessage, TurnID: "turn-2"},
		{Type: msg.EventResult, TurnID: "turn-2", Result: &msg.ResultEvent{
			Text:  "second",
			Usage: msg.TokenUsage{InputTokens: 30, OutputTokens: 40},
		}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 2 {
		t.Fatalf("turn_complete count = %d; want 2", len(tcs))
	}
	// usage_delta is per-turn, NOT cumulative. (Cumulative lives on
	// usage_total.)
	if tcs[0].UsageDelta.InputTokens != 10 || tcs[0].FinalMessage != "first" {
		t.Fatalf("turn 1 = %+v; want input=10 final=first", tcs[0])
	}
	if tcs[1].UsageDelta.InputTokens != 30 || tcs[1].FinalMessage != "second" {
		t.Fatalf("turn 2 = %+v; want input=30 final=second (delta, not cumulative)", tcs[1])
	}
	// Both accumulators cleared.
	if len(d.turnAccums) != 0 {
		t.Fatalf("turnAccums after both terminators = %v; want empty", d.turnAccums)
	}
}

func TestDerivation_TurnComplete_ToolResultWithoutCallStillRecorded(t *testing.T) {
	// Resume case: tool_result with no preceding tool_call (e.g. the
	// call landed in a session window we never saw). Should still
	// produce a best-effort entry in tool_calls so the result isn't
	// silently dropped.
	d := newDerivationState()
	got := derivedOf(d, []msg.Event{
		{Type: msg.EventUserMessage, TurnID: "turn-1"},
		{Type: msg.EventToolResult, TurnID: "turn-1", ToolResult: &msg.ToolResultEvent{
			ToolID: "orphan", Name: "Bash", Output: "stranded",
		}},
		{Type: msg.EventResult, TurnID: "turn-1", Result: &msg.ResultEvent{}},
	})
	tcs := turnCompleteEvents(got)
	if len(tcs) != 1 {
		t.Fatalf("turn_complete count = %d; want 1", len(tcs))
	}
	if len(tcs[0].ToolCalls) != 1 || tcs[0].ToolCalls[0].Output != "stranded" {
		t.Fatalf("orphan tool_result = %+v; want one entry with output=stranded", tcs[0].ToolCalls)
	}
}
