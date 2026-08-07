package harness

import (
	"log"
	"strings"
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
// state machine. SessionState (13 values) is the authoritative
// projection emitted via EventSessionState; the legacy 4-value
// AgentState surface is now deprecated and no longer emitted.
type derivationState struct {
	mu sync.Mutex

	// sessionState is the last-emitted SessionState value. derive()
	// only emits an event on transition.
	sessionState msg.SessionState

	// activeTools tracks tool_use_id → tool name for in-flight tool
	// calls. Multiple concurrent tools keep the session in
	// tool_running until the last result lands.
	activeTools map[string]string

	// pendingApprovals holds RequestIDs of HookEvents currently in
	// awaiting_resolution. While non-empty the session is in
	// SessionAwaitingPermission; the original pre-approval state is
	// captured in preApprovalState and restored on drain.
	pendingApprovals map[string]struct{}

	// awaitingApproval is true while pendingApprovals is non-empty OR
	// while a deprecated EventApproval pending is unresolved. Permission
	// override beats tool_running per the override rule below.
	awaitingApproval bool

	// preApprovalState is the SessionState we restore to once approvals
	// drain. Captured the first time we go awaitingApproval=true.
	preApprovalState msg.SessionState

	// preCompactState is the SessionState we restore to when a
	// compaction finishes. Captured on the `compact_ack` that opens
	// SessionCompacting. A compaction interrupts a turn rather than
	// ending it, so restoring beats assuming idle.
	preCompactState msg.SessionState

	// usage is the running session-cumulative TokenUsage, summed
	// field-by-field on every EventResult. ContextTokens and
	// ContextLimit are last-value-wins (current-context state, not
	// consumption); all other fields are summed.
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

	// apiSpendUSD is the running session-cumulative spend across every
	// EventAPICall observed — across every harness process the session has
	// had, not just the current one. A derivation is discarded when its
	// harness process exits, so on a resume these accumulators are seeded
	// back from the persisted row (seedAPISpend); without that seeding the
	// session would start a second, independent spend history and the
	// ceiling that halted it would re-arm from zero. Distinct from cost
	// (above) which sums
	// EventResult.Usage and excludes auxiliary API calls
	// (session-title generation, prompt-suggestion). When the UI's
	// top-line cost reads APISpendTotal (when Calls > 0), this is the
	// number it displays; UsageTotal becomes the fallback for legacy
	// or non-OTel-instrumented sessions.
	apiSpendUSD      float64
	apiSpendUsage    msg.TokenUsage
	apiSpendCalls    int
	apiSpendByModel  map[string]float64 // USD per model
	apiSpendBySource map[string]float64 // USD per query_source

	// turnAccums holds in-flight per-turn accumulators keyed by
	// turn_id. Created on the first event seen for a turn_id, closed
	// on the terminating EventResult / EventError, or evicted FIFO
	// when the cap is exceeded.
	turnAccums map[string]*turnAccumulator

	// turnOrder preserves insertion order so we can FIFO-evict the
	// oldest open accumulator when maxInflightTurns is exceeded.
	turnOrder []string
}

// newDerivationState builds an empty per-session derivation whose
// initial (prev) SessionState is idle. Use this only where no prior
// persisted state exists — brand-new sessions and pure state-machine
// unit tests. Recreating a derivation for a session that already has a
// persisted sessions.state row must go through newDerivationStateSeeded
// so the first terminal event isn't suppressed by a stale idle prev
// (the F1 server-side settle bug).
func newDerivationState() *derivationState {
	return newDerivationStateSeeded(msg.SessionIdle)
}

// newDerivationStateSeeded builds a derivation whose initial (prev)
// SessionState is seeded from the caller-supplied value — the canonical
// persisted sessions.state row read at derivation-creation time. After
// a bridge-server restart the in-memory prev would otherwise reset to
// idle, so a later EventResult deriving next==idle==prev is suppressed
// (derive only emits on next!=prev) and a row still reading
// tool_running/running is never corrected. Seeding prev from the
// persisted row keeps the closing transition observable. An empty
// initial falls back to idle (no persisted row yet).
func newDerivationStateSeeded(initial msg.SessionState) *derivationState {
	if initial == "" {
		initial = msg.SessionIdle
	}
	return &derivationState{
		sessionState:     initial,
		activeTools:      make(map[string]string),
		pendingApprovals: make(map[string]struct{}),
		turnAccums:       make(map[string]*turnAccumulator),
		apiSpendByModel:  make(map[string]float64),
		apiSpendBySource: make(map[string]float64),
	}
}

// seedAPISpend starts the derivation's spend accumulators from what the
// session has already been recorded as spending, so the api_spend_total it
// emits continues that history rather than opening a second one.
//
// This is the spend counterpart of the prev-SessionState seeding above, and
// it exists for the same reason: a derivation is per-process, a session is
// not. It must be called before the derivation sees any event — it assigns
// rather than adds, so calling it on a derivation that has already counted
// an api_call would discard that call.
//
// The maps are copied. The caller's copy comes from a store read it may reuse,
// and a derivation that shared it would mutate the caller's data on every
// priced API call.
func (d *derivationState) seedAPISpend(totalUSD float64, calls int, usage msg.TokenUsage, byModel, byQuerySource map[string]float64) {
	d.apiSpendUSD = totalUSD
	d.apiSpendCalls = calls
	d.apiSpendUsage = usage
	d.apiSpendByModel = make(map[string]float64, len(byModel))
	for model, usd := range byModel {
		d.apiSpendByModel[model] = usd
	}
	d.apiSpendBySource = make(map[string]float64, len(byQuerySource))
	for source, usd := range byQuerySource {
		d.apiSpendBySource[source] = usd
	}
}

// currentState returns the last-derived SessionState under lock. Used
// by the manager's terminal-settle reconcile to learn the settled
// target after derive() has run — d.sessionState holds the settled
// value even when the transition was suppressed (next==prev), because
// prev already equalled next.
func (d *derivationState) currentState() msg.SessionState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionState
}

// forceState moves the derivation to next because the SERVER decided so,
// not because an event implied it, and reports the state it replaced.
// The second return is false when the derivation already held next, so
// the caller can skip a no-op write and broadcast.
//
// This exists for decisions only the server can make. An interrupt is the
// case it was written for: the user pressed Stop, and no event the harness
// emits says that happened. Writing the session row directly instead would
// leave the derivation still holding the pre-interrupt state, so the very
// next event would compute its transition from a state the session is no
// longer in — and the SSE subscribers would never hear about the interrupt
// at all, since only derive() broadcasts.
//
// Clears the in-flight bookkeeping for the same reason EventResult does:
// whatever tools or prompts were outstanding are not coming back.
func (d *derivationState) forceState(next msg.SessionState) (msg.SessionState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.sessionState
	if prev == next {
		return prev, false
	}
	d.sessionState = next
	d.activeTools = map[string]string{}
	d.awaitingApproval = false
	d.pendingApprovals = make(map[string]struct{})
	return prev, true
}

// isHoldingSessionState reports whether s is a "turn in flight" state —
// one that a settled terminal event (EventResult/EventError) should
// overwrite when it lingers in the persisted row. Per the live-DB
// audit (docs/findings/2026-07-27-interrupt-dual-emit-turn-hijack.md
// §5) the granular model_generating/compacting/starting states are
// never actually emitted by Claude Code, but they remain valid holding
// values in the enum, so they're included for completeness. Critically
// this treats BOTH running AND tool_running AND awaiting_permission as
// holding — the classic trap is comparing only against SessionRunning
// and missing tool_running. Settled/terminal states (idle, awaiting_user,
// completed, error, aborted, paused, disconnected) are NOT holding and
// are left untouched so a late stray result can't clobber them.
func isHoldingSessionState(s msg.SessionState) bool {
	switch s {
	case msg.SessionStarting,
		msg.SessionRunning,
		msg.SessionModelGenerating,
		msg.SessionToolRunning,
		msg.SessionCompacting,
		msg.SessionAwaitingPermission,
		msg.SessionWaitingApproval,
		msg.SessionRateLimited:
		return true
	default:
		return false
	}
}

// derive applies the state machine to ev and returns the convenience
// events that should be broadcast immediately after ev's own fan-out.
// Returns nil when ev produces no derived events.
//
// Wire fields (session_id / bridge_id / harness / turn_id) on the
// returned events are copied from ev so consumers can correlate cause
// and effect on a single event-id replay.
//
// Emission order within a single derive() call:
//  1. session_state (only when state changes)
//  2. usage_total (only on EventResult)
//  3. turn_complete (only on terminating EventResult/EventError with
//     a known turn_id; emitted after usage_total).
func (d *derivationState) derive(ev *msg.Event) []msg.Event {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []msg.Event

	// Update or create the per-turn accumulator before transition
	// logic runs. Any event with a turn_id contributes; tool_call /
	// tool_result are the ones that actually mutate the summary.
	d.recordForTurn(ev)

	prev := d.sessionState
	next := prev
	reason := ""

	switch ev.Type {
	case msg.EventUserMessage:
		// A turn opens with the model generating, not with a tool
		// running: no tool has been called yet and none may ever be.
		// This used to answer tool_running for the whole turn, which
		// made the two states indistinguishable in practice and left
		// model_generating emitted zero times across the live table.
		next = msg.SessionModelGenerating
		reason = "turn_started"

	case msg.EventToolCall:
		if ev.ToolCall != nil && ev.ToolCall.ToolID != "" {
			d.activeTools[ev.ToolCall.ToolID] = ev.ToolCall.Name
			reason = "tool=" + ev.ToolCall.Name
		}
		if !d.awaitingApproval {
			next = msg.SessionToolRunning
		}

	case msg.EventToolResult:
		if ev.ToolResult != nil && ev.ToolResult.ToolID != "" {
			delete(d.activeTools, ev.ToolResult.ToolID)
		}
		// The turn does not end here — the model reads the result and
		// keeps generating — so drop back to model_generating once the
		// LAST in-flight tool reports. While other tools are still out
		// the session stays tool_running, which is why this is gated on
		// activeTools draining rather than firing on every result.
		//
		// An open permission prompt outranks both (see the override rule
		// on awaitingApproval): resolving it is what releases the tool,
		// so answering model_generating here would erase a prompt the
		// user still has on screen.
		if len(d.activeTools) == 0 && !d.awaitingApproval && prev == msg.SessionToolRunning {
			next = msg.SessionModelGenerating
			reason = "tools_drained"
		}

	case msg.EventSystem:
		// Compaction. Claude Code emits `compact_ack` when it accepts a
		// manual /compact and `compact_boundary` when any compaction —
		// manual OR automatic — finishes. Only the ack opens the state;
		// an automatic compaction is invisible until its boundary lands,
		// so the boundary must tolerate never having seen an ack and
		// simply not transition in that case.
		//
		// preCompactState restores whatever the turn was doing rather
		// than assuming idle: a compaction runs mid-turn and the model
		// carries on generating afterwards.
		if ev.System != nil {
			switch ev.System.Subtype {
			case "compact_ack":
				if prev != msg.SessionCompacting {
					d.preCompactState = prev
				}
				next = msg.SessionCompacting
				reason = "compact_requested"
			case "compact_boundary":
				if prev == msg.SessionCompacting {
					next = d.preCompactState
					if next == "" || next == msg.SessionCompacting {
						next = msg.SessionModelGenerating
					}
					reason = "compact_complete"
				}
			}
		}

	case msg.EventHook:
		// Authoritative path for permission prompts. permission-store
		// drives prehook → bridge-server emits HookEvent with
		// awaiting_resolution while the prompt is open and completed
		// once the user (or a rule) resolves it. Multiple hooks can
		// be pending simultaneously; pendingApprovals tracks the set.
		if ev.Hook != nil && ev.Hook.RequestID != "" {
			switch ev.Hook.Phase {
			case "awaiting_resolution":
				if !d.awaitingApproval {
					d.preApprovalState = prev
					d.awaitingApproval = true
				}
				d.pendingApprovals[ev.Hook.RequestID] = struct{}{}
				next = msg.SessionAwaitingPermission
				reason = "hook=" + ev.Hook.Source
			case "completed":
				if _, was := d.pendingApprovals[ev.Hook.RequestID]; was {
					delete(d.pendingApprovals, ev.Hook.RequestID)
					if len(d.pendingApprovals) == 0 && d.awaitingApproval {
						d.awaitingApproval = false
						next = d.preApprovalState
						if next == "" || next == msg.SessionAwaitingPermission {
							// The model is what resumes when a prompt resolves — an
							// allowed tool announces itself with its own tool_call,
							// and a denied one hands the model a refusal to read.
							// Falling back to tool_running claimed a tool was in
							// flight on the very path where none is.
							next = msg.SessionModelGenerating
						}
						reason = "hook_resolved"
					}
				}
			}
		}

	case msg.EventApproval:
		// Deprecated path. Kept for harnesses still on the legacy
		// approval flow; same semantics as EventHook above. Will be
		// removed once all harnesses route permission through the
		// hook system.
		if ev.Approval != nil {
			switch ev.Approval.Status {
			case "pending", "requested":
				if !d.awaitingApproval {
					d.preApprovalState = prev
					d.awaitingApproval = true
				}
				next = msg.SessionAwaitingPermission
				reason = "approval_required"
			case "approved", "denied", "auto_approved", "resolved":
				if d.awaitingApproval && len(d.pendingApprovals) == 0 {
					d.awaitingApproval = false
					next = d.preApprovalState
					if next == "" || next == msg.SessionAwaitingPermission {
						// The model is what resumes when a prompt resolves — an
						// allowed tool announces itself with its own tool_call,
						// and a denied one hands the model a refusal to read.
						// Falling back to tool_running claimed a tool was in
						// flight on the very path where none is.
						next = msg.SessionModelGenerating
					}
					reason = "approval_" + ev.Approval.Status
				}
				// auto_approved with no preceding pending — no
				// transition; the harness pre-resolved before the
				// user ever saw a prompt.
			}
		}

	case msg.EventResult:
		// Heuristic awaiting_user: if final message looks like an
		// open question to the user, emit awaiting_user instead of
		// idle. Best-effort string match for now; replace with a
		// cheap-model classifier later (same pattern as the session
		// auto-rename feature) once that infrastructure is reusable.
		if ev.Result != nil && looksLikeQuestion(ev.Result.Text) {
			next = msg.SessionAwaitingUser
			reason = "turn_complete_awaiting_user"
		} else {
			next = msg.SessionIdle
			reason = "turn_complete"
		}
		d.activeTools = map[string]string{}
		d.awaitingApproval = false
		d.pendingApprovals = make(map[string]struct{})

	case msg.EventError:
		next = msg.SessionError
		d.activeTools = map[string]string{}
		d.awaitingApproval = false
		d.pendingApprovals = make(map[string]struct{})
		reason = "error"

	case msg.EventSessionState:
		// Manager-emitted lifecycle states (subprocess starting,
		// completed, disconnected, paused, etc.) pass through.
		// Harness-emitted EventSessionState is dropped at intake by
		// the manager (see manager.readEvents); only manager-injected
		// lifecycle signals reach here.
		if ev.State != nil {
			switch ev.State.State {
			case msg.SessionAborted:
				next = msg.SessionAborted
				d.activeTools = map[string]string{}
				d.awaitingApproval = false
				d.pendingApprovals = make(map[string]struct{})
				reason = "aborted"
			case msg.SessionStarting,
				msg.SessionCompacting, msg.SessionPaused, msg.SessionRateLimited,
				msg.SessionCompleted, msg.SessionDisconnected,
				// SessionIdle is the reverse of SessionCompleted, and it
				// arrives from the same caller: handleMarkSessionDone
				// sends completed for done=true and idle for done=false.
				// Leaving it out of this list made the mark one-way — the
				// folder moved back out of Archive and the state stayed
				// completed, because next==prev suppressed the transition
				// that writes the session row.
				msg.SessionIdle:
				next = ev.State.State
				reason = "lifecycle"
			}
		}
	}

	if next != prev {
		d.sessionState = next
		out = append(out, msg.Event{
			Type:             msg.EventSessionState,
			Harness:          ev.Harness,
			BridgeSessionID:  ev.BridgeSessionID,
			HarnessSessionID: ev.HarnessSessionID,
			ClientRequestID:  ev.ClientRequestID,
			TurnID:           ev.TurnID,
			Timestamp:        time.Now(),
			DerivedFrom:      derivedFromIDs(ev),
			State: &msg.StateEvent{
				State:    next,
				Previous: prev,
				Reason:   reason,
			},
		})
	}

	if ev.Type == msg.EventResult {
		d.applyResultUsage(ev.Result)
		out = append(out, msg.Event{
			Type:             msg.EventUsageTotal,
			Harness:          ev.Harness,
			BridgeSessionID:  ev.BridgeSessionID,
			HarnessSessionID: ev.HarnessSessionID,
			ClientRequestID:  ev.ClientRequestID,
			TurnID:           ev.TurnID,
			Timestamp:        time.Now(),
			UsageTotal: &msg.UsageTotalEvent{
				Usage: d.usage,
				Cost:  copyCost(d.cost),
				Turns: d.turns,
			},
		})
	}

	if ev.Type == msg.EventAPICall {
		d.applyAPICall(ev.APICall)
		out = append(out, msg.Event{
			Type:             msg.EventAPISpendTotal,
			Harness:          ev.Harness,
			BridgeSessionID:  ev.BridgeSessionID,
			HarnessSessionID: ev.HarnessSessionID,
			ClientRequestID:  ev.ClientRequestID,
			TurnID:           ev.TurnID,
			Timestamp:        time.Now(),
			DerivedFrom:      derivedFromIDs(ev),
			APISpendTotal: &msg.APISpendTotalEvent{
				TotalUSD:      d.apiSpendUSD,
				Usage:         d.apiSpendUsage,
				Calls:         d.apiSpendCalls,
				ByModel:       copyFloatMap(d.apiSpendByModel),
				ByQuerySource: copyFloatMap(d.apiSpendBySource),
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

// looksLikeQuestion is the awaiting_user heuristic. Best-effort: a
// final message ending with a question mark or with common handoff
// phrasing is treated as expecting a user reply. False positives are
// fine — the worst case is a session that says "awaiting_user" when
// it really meant "idle", and the user moves on. See the
// CONVENIENCE-EVENTS.md migration note for the planned upgrade to a
// classifier model.
func looksLikeQuestion(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	// Take the last non-empty line. Trailing markdown like "---" or a
	// single "." should not break the heuristic.
	lines := strings.Split(trimmed, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || l == "---" {
			continue
		}
		last = l
		break
	}
	if last == "" {
		return false
	}
	if strings.HasSuffix(last, "?") {
		return true
	}
	// Conservative phrase list — only the unambiguous "I am asking you"
	// signals. Avoid catching "let me know if you have questions"
	// (which is a sign-off, not a question).
	lower := strings.ToLower(last)
	for _, phrase := range []string{
		"which would you prefer",
		"what would you like",
		"should i proceed",
		"should i go ahead",
		"shall i proceed",
		"want me to",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// derivedFromIDs returns the upstream event IDs (if any) carried on ev
// so the derived event can list its causes via DerivedFrom. Currently
// returns the source's MessageID when present; falls back to nil. The
// field is informational — consumers that want to dedupe derivations
// against their causes can use it.
func derivedFromIDs(ev *msg.Event) []string {
	if ev == nil || ev.MessageID == "" {
		return nil
	}
	return []string{ev.MessageID}
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

// applyAPICall folds one EventAPICall observation into the running
// per-call spend totals. Caller must hold d.mu.
//
// Unlike applyResultUsage (which sums per-turn EventResult.Usage), this
// sums per-call telemetry from the OTel pipeline — including auxiliary
// calls (session-title, prompt-suggestion) that don't show up in turn
// results. Total tokens are summed; context tokens / limit are not
// reported per-call so we leave the existing ContextTokens/ContextLimit
// on d.usage alone — they belong to the per-turn view, not this one.
//
// ByModel / ByQuerySource maps accumulate USD per dimension. Calls
// with empty Model or QuerySource still count toward TotalUSD; only
// their dimensional attribution is skipped (fail-fast: don't fabricate
// a placeholder key like "unknown" that masks a producer bug).
func (d *derivationState) applyAPICall(a *msg.APICallEvent) {
	if a == nil {
		return
	}
	d.apiSpendCalls++
	d.apiSpendUSD += a.CostUSD
	d.apiSpendUsage.InputTokens += a.InputTokens
	d.apiSpendUsage.OutputTokens += a.OutputTokens
	d.apiSpendUsage.CacheReadTokens += a.CacheReadTokens
	d.apiSpendUsage.CacheWriteTokens += a.CacheCreationTokens
	d.apiSpendUsage.TotalTokens += a.InputTokens + a.OutputTokens
	if a.Model != "" {
		d.apiSpendByModel[a.Model] += a.CostUSD
	}
	if a.QuerySource != "" {
		d.apiSpendBySource[a.QuerySource] += a.CostUSD
	}
}

// copyFloatMap returns a shallow copy so each emitted APISpendTotal
// carries its own snapshot rather than sharing a map pointer with
// future mutations of the derivation state.
func copyFloatMap(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
