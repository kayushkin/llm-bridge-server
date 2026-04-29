package harness

import (
	"log"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// maxInflightTurns caps how many concurrently-open turn_complete
// accumulators we keep per session. Normal sessions have ≤1 in-flight
// turn; the cap is purely a guard against pathological harnesses that
// open turns without ever terminating them. Oldest is evicted FIFO
// when the cap is hit.
const maxInflightTurns = 8

// turnAccumulator collects the per-turn data needed to construct a
// TurnCompleteEvent on the terminating EventResult / EventError.
// One per open turn_id; closed on terminator or evicted FIFO when the
// session goes over maxInflightTurns.
type turnAccumulator struct {
	turnID    string
	startedAt time.Time

	// toolCalls is the ordered record of tool_call/tool_result pairs
	// seen in this turn. ToolSummary entries are appended on
	// EventToolCall and have their Output/Error filled on the matching
	// EventToolResult.
	toolCalls []msg.ToolSummary

	// activeCalls maps tool_use_id → index into toolCalls so the
	// matching tool_result can backfill the same row.
	activeCalls map[string]int
}

// derivationState tracks the per-session state needed to derive
// convenience events from the raw event stream — the centralized
// alternative to forcing every consumer to re-implement the same
// state machine. See ~/repos/llm-bridge/msg/CONVENIENCE-EVENTS.md.
type derivationState struct {
	mu sync.Mutex

	// agentState is the last-emitted agent_state value. derive() only
	// emits an event on transition.
	agentState msg.AgentState

	// activeTools tracks tool_use_id → tool name for in-flight tool
	// calls. Multiple concurrent tools keep the session in
	// tool_running until the last result lands.
	activeTools map[string]string

	// awaitingApproval is true while a permission prompt is open.
	// Approval overrides tool_running (the spec calls this the
	// "awaiting_input overrides tool_running" rule).
	awaitingApproval bool

	// preApproval is the state we restore to once the approval
	// resolves. Captured the first time we go awaitingApproval=true.
	preApproval msg.AgentState

	// usage is the running session-cumulative TokenUsage, summed
	// field-by-field on every EventResult. ContextTokens and
	// ContextLimit are last-value-wins (current-context state, not
	// consumption); all other fields are summed. See
	// CONVENIENCE-EVENTS.md "usage_total".
	usage msg.TokenUsage

	// cost is the running session-cumulative Cost, lazily allocated
	// on the first turn that reports a non-nil ResultEvent.Cost.
	// Partial sums are intentional — a usage_total whose cost
	// reflects only the priced subset of turns is more useful than
	// no cost at all.
	cost *msg.Cost

	// turns counts how many EventResult terminators have contributed
	// to usage and cost.
	turns int

	// turnAccums holds in-flight per-turn accumulators keyed by
	// turn_id. Created on the first event seen for a turn_id, closed
	// on the terminating EventResult / EventError, or evicted FIFO
	// when the cap is exceeded.
	turnAccums map[string]*turnAccumulator

	// turnOrder preserves insertion order so we can FIFO-evict the
	// oldest open accumulator when maxInflightTurns is exceeded.
	turnOrder []string
}

func newDerivationState() *derivationState {
	return &derivationState{
		agentState:  msg.AgentStateIdle,
		activeTools: make(map[string]string),
		turnAccums:  make(map[string]*turnAccumulator),
	}
}

// derive applies the state machine to ev and returns the convenience
// events that should be broadcast immediately after ev's own fan-out.
// Returns nil when ev produces no derived events.
//
// The transition table mirrors msg/CONVENIENCE-EVENTS.md "State
// machine". Wire fields (session_id / bridge_id / harness / turn_id)
// on the returned events are copied from ev so consumers can
// correlate cause and effect on a single event-id replay.
//
// Emission order within a single derive() call:
//  1. agent_state (only when state changes)
//  2. usage_total (only on EventResult)
//  3. turn_complete (only on terminating EventResult/EventError with
//     a known turn_id; emitted after usage_total per spec's
//     "usage_total first, turn_complete second" lean).
func (d *derivationState) derive(ev *msg.Event) []msg.Event {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []msg.Event

	// Update or create the per-turn accumulator before transition
	// logic runs. Any event with a turn_id contributes; tool_call /
	// tool_result are the ones that actually mutate the summary.
	d.recordForTurn(ev)

	prev := d.agentState
	next := prev
	reason := ""

	switch ev.Type {
	case msg.EventUserMessage:
		// Turn opens — idle → tool_running. The "tool_running"
		// label covers model generation as well as actual tool
		// invocation; see the [OPEN] note in the spec.
		next = msg.AgentStateToolRunning
		reason = "turn_started"

	case msg.EventToolCall:
		if ev.ToolCall != nil && ev.ToolCall.ToolID != "" {
			d.activeTools[ev.ToolCall.ToolID] = ev.ToolCall.Name
			reason = "tool=" + ev.ToolCall.Name
		}
		if !d.awaitingApproval {
			next = msg.AgentStateToolRunning
		}

	case msg.EventToolResult:
		if ev.ToolResult != nil && ev.ToolResult.ToolID != "" {
			delete(d.activeTools, ev.ToolResult.ToolID)
		}
		// No transition purely on tool_result. The terminating
		// EventResult / EventError decides whether the turn ends
		// or another tool keeps running. While intermediate tool
		// results land we stay in whatever state we were in.

	case msg.EventApproval:
		if ev.Approval != nil {
			switch ev.Approval.Status {
			case "pending", "requested":
				if !d.awaitingApproval {
					d.preApproval = prev
					d.awaitingApproval = true
				}
				next = msg.AgentStateAwaitingInput
				reason = "approval_required"
			case "approved", "denied", "auto_approved", "resolved":
				if d.awaitingApproval {
					d.awaitingApproval = false
					next = d.preApproval
					if next == "" || next == msg.AgentStateAwaitingInput {
						next = msg.AgentStateToolRunning
					}
					reason = "approval_" + ev.Approval.Status
				}
				// auto_approved with no preceding pending — no
				// transition; the harness pre-resolved before
				// the user ever saw a prompt.
			}
		}

	case msg.EventResult:
		next = msg.AgentStateIdle
		d.activeTools = map[string]string{}
		d.awaitingApproval = false
		reason = "turn_complete"

	case msg.EventError:
		next = msg.AgentStateError
		d.activeTools = map[string]string{}
		d.awaitingApproval = false
		reason = "error"

	case msg.EventSessionState:
		if ev.State != nil && ev.State.State == msg.SessionAborted {
			next = msg.AgentStateIdle
			d.activeTools = map[string]string{}
			d.awaitingApproval = false
			reason = "aborted"
		}
	}

	if next != prev {
		d.agentState = next
		out = append(out, msg.Event{
			Type:            msg.EventAgentState,
			Harness:         ev.Harness,
			BridgeSessionID:  ev.BridgeSessionID,
			HarnessSessionID: ev.HarnessSessionID,
			ClientID:        ev.ClientID,
			ClientRequestID: ev.ClientRequestID,
			TurnID:          ev.TurnID,
			Timestamp:       time.Now(),
			AgentState: &msg.AgentStateEvent{
				State:    next,
				Previous: prev,
				Reason:   reason,
			},
		})
	}

	if ev.Type == msg.EventResult {
		d.applyResultUsage(ev.Result)
		out = append(out, msg.Event{
			Type:            msg.EventUsageTotal,
			Harness:         ev.Harness,
			BridgeSessionID:  ev.BridgeSessionID,
			HarnessSessionID: ev.HarnessSessionID,
			ClientID:        ev.ClientID,
			ClientRequestID: ev.ClientRequestID,
			TurnID:          ev.TurnID,
			Timestamp:       time.Now(),
			UsageTotal: &msg.UsageTotalEvent{
				Usage: d.usage,
				Cost:  copyCost(d.cost),
				Turns: d.turns,
			},
		})
	}

	if ev.Type == msg.EventResult || ev.Type == msg.EventError {
		if tc := d.closeTurn(ev); tc != nil {
			out = append(out, *tc)
		}
	}

	return out
}

// recordForTurn folds ev into the per-turn accumulator. Creates the
// accumulator on first sight of a turn_id for non-terminator events
// (EventResult / EventError don't open a new accumulator — they only
// close an existing one, so a late terminator for an evicted or
// never-opened turn produces no turn_complete). Tool_call /
// tool_result events mutate the toolCalls slice; other events just
// open the accumulator if missing. Caller must hold d.mu.
func (d *derivationState) recordForTurn(ev *msg.Event) {
	if ev.TurnID == "" {
		return
	}
	acc := d.turnAccums[ev.TurnID]
	if acc == nil {
		// Terminators don't open new accumulators. If the
		// originating non-terminator events never reached us (or were
		// evicted), we don't fabricate a degenerate turn_complete on
		// the way out.
		if ev.Type == msg.EventResult || ev.Type == msg.EventError {
			return
		}
		acc = &turnAccumulator{
			turnID:      ev.TurnID,
			startedAt:   eventTimestamp(ev),
			activeCalls: make(map[string]int),
		}
		d.turnAccums[ev.TurnID] = acc
		d.turnOrder = append(d.turnOrder, ev.TurnID)

		// Evict oldest if we just blew the cap. The terminator for
		// the evicted turn never landed (or hasn't yet) — the
		// summary it would have produced is dropped on the floor.
		// Pathological case only; surfaced as a logged warning.
		if len(d.turnOrder) > maxInflightTurns {
			oldest := d.turnOrder[0]
			d.turnOrder = d.turnOrder[1:]
			if _, ok := d.turnAccums[oldest]; ok {
				log.Printf("[derivation] evicting in-flight turn %q (cap=%d) — turn_complete will not be emitted", oldest, maxInflightTurns)
				delete(d.turnAccums, oldest)
			}
		}
	}

	switch ev.Type {
	case msg.EventToolCall:
		if ev.ToolCall == nil {
			return
		}
		summary := msg.ToolSummary{
			Tool:  ev.ToolCall.Name,
			Input: string(ev.ToolCall.Input),
		}
		idx := len(acc.toolCalls)
		acc.toolCalls = append(acc.toolCalls, summary)
		if ev.ToolCall.ToolID != "" {
			acc.activeCalls[ev.ToolCall.ToolID] = idx
		}

	case msg.EventToolResult:
		if ev.ToolResult == nil {
			return
		}
		idx, ok := acc.activeCalls[ev.ToolResult.ToolID]
		if !ok {
			// tool_result with no matching tool_call — append a
			// best-effort entry so the result isn't lost. Happens
			// on resume replays where the call landed in a prior
			// session window we never saw.
			summary := msg.ToolSummary{Tool: ev.ToolResult.Name}
			if ev.ToolResult.IsError {
				summary.Error = ev.ToolResult.Output
			} else {
				summary.Output = ev.ToolResult.Output
			}
			acc.toolCalls = append(acc.toolCalls, summary)
			return
		}
		entry := &acc.toolCalls[idx]
		if ev.ToolResult.IsError {
			entry.Error = ev.ToolResult.Output
		} else {
			entry.Output = ev.ToolResult.Output
		}
		delete(acc.activeCalls, ev.ToolResult.ToolID)
	}
}

// closeTurn finalizes the accumulator for ev.TurnID and returns the
// constructed turn_complete event, or nil if no accumulator was
// open for this turn (e.g. a result/error replayed mid-restart, or a
// turn that was already evicted). Caller must hold d.mu.
func (d *derivationState) closeTurn(ev *msg.Event) *msg.Event {
	if ev.TurnID == "" {
		return nil
	}
	acc, ok := d.turnAccums[ev.TurnID]
	if !ok {
		return nil
	}
	delete(d.turnAccums, ev.TurnID)
	for i, tid := range d.turnOrder {
		if tid == ev.TurnID {
			d.turnOrder = append(d.turnOrder[:i], d.turnOrder[i+1:]...)
			break
		}
	}

	tc := &msg.TurnCompleteEvent{
		TurnID:    acc.turnID,
		ToolCalls: acc.toolCalls,
	}
	endedAt := eventTimestamp(ev)
	if !acc.startedAt.IsZero() && !endedAt.IsZero() {
		tc.DurationMS = endedAt.Sub(acc.startedAt).Milliseconds()
		if tc.DurationMS < 0 {
			tc.DurationMS = 0
		}
	}

	switch ev.Type {
	case msg.EventResult:
		if ev.Result != nil {
			tc.FinalMessage = ev.Result.Text
			tc.UsageDelta = ev.Result.Usage
			tc.Cost = copyCost(ev.Result.Cost)
			if ev.Result.IsError {
				tc.IsError = true
				tc.ErrorMessage = ev.Result.Text
			}
		}
	case msg.EventError:
		tc.IsError = true
		if ev.Error != nil {
			tc.ErrorMessage = ev.Error.Message
		}
	}

	return &msg.Event{
		Type:             msg.EventTurnComplete,
		Harness:          ev.Harness,
		BridgeSessionID:  ev.BridgeSessionID,
		HarnessSessionID: ev.HarnessSessionID,
		ClientID:         ev.ClientID,
		ClientRequestID:  ev.ClientRequestID,
		TurnID:           ev.TurnID,
		Timestamp:        time.Now(),
		TurnComplete:     tc,
	}
}

// eventTimestamp returns ev.Timestamp if non-zero, else time.Now().
// Lets tests drive deterministic durations by stamping events; falls
// back to wall-clock for production paths that rely on the bridge
// clock rather than the harness's.
func eventTimestamp(ev *msg.Event) time.Time {
	if ev != nil && !ev.Timestamp.IsZero() {
		return ev.Timestamp
	}
	return time.Now()
}

// applyResultUsage folds a ResultEvent's Usage and Cost into the
// running session totals. Caller must hold d.mu.
//
// TokenUsage fields are summed except ContextTokens/ContextLimit,
// which are last-value-wins (they describe current context state,
// not cumulative consumption). Cost is summed where reported; turns
// without a cost still count toward d.turns and Usage.
func (d *derivationState) applyResultUsage(r *msg.ResultEvent) {
	d.turns++
	if r == nil {
		return
	}
	u := r.Usage
	d.usage.InputTokens += u.InputTokens
	d.usage.OutputTokens += u.OutputTokens
	d.usage.TotalTokens += u.TotalTokens
	d.usage.CacheReadTokens += u.CacheReadTokens
	d.usage.CacheWriteTokens += u.CacheWriteTokens
	d.usage.ReasoningTokens += u.ReasoningTokens
	// Context fields: current-state, not consumption. Last value wins,
	// but only when the result actually carried one — a zero context
	// reading from a turn that didn't report context shouldn't clobber
	// the prior reading.
	if u.ContextTokens != 0 {
		d.usage.ContextTokens = u.ContextTokens
	}
	if u.ContextLimit != 0 {
		d.usage.ContextLimit = u.ContextLimit
	}

	if r.Cost != nil {
		if d.cost == nil {
			d.cost = &msg.Cost{}
		}
		d.cost.TotalUSD += r.Cost.TotalUSD
		d.cost.InputUSD += r.Cost.InputUSD
		d.cost.OutputUSD += r.Cost.OutputUSD
		d.cost.UpstreamCost += r.Cost.UpstreamCost
		// IsByok is sticky: once any contributing turn was BYOK we
		// keep the flag set so consumers can show the right pricing
		// caveat. (Mirrors how a single non-priced turn shouldn't
		// hide the fact that the session is partly BYOK.)
		if r.Cost.IsByok {
			d.cost.IsByok = true
		}
	}
}

// copyCost returns a defensive copy of c so each emitted UsageTotal
// carries its own Cost snapshot rather than sharing a pointer with
// future mutations of d.cost. Returns nil for nil input.
func copyCost(c *msg.Cost) *msg.Cost {
	if c == nil {
		return nil
	}
	v := *c
	return &v
}
