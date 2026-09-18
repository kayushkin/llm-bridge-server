package harness

import (
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The status half of the derivation: everything msg.SessionStatus carries
// beyond the one-word state. It is folded from the same events, under the same
// lock, as the state machine in derive(), so the two can never describe
// different moments.
//
// This is the only place a session's status is decided. Clients used to
// rebuild the tool list, the subagent list and the thinking/text word from
// their own copy of the transcript; they now render what is published here.

// statusTracking is the status bookkeeping embedded in derivationState.
type statusTracking struct {
	// toolCalls are the calls in flight, oldest first. activeTools (in
	// derivationState) stays the state machine's own record of the same set;
	// this slice adds what a status line needs on top — order, a summary of
	// the input, and when the call started.
	toolCalls []msg.StatusTool

	// subagents are the harness tasks started and not yet terminal, oldest
	// first. They outlive a turn: a backgrounded shell keeps running after
	// the result that spawned it, and it is still running.
	subagents []msg.StatusSubagent

	// claimedToolIDs are the tool_use ids of calls that spawned a task. Such a
	// call is reported as a subagent, never also as a tool in flight. A
	// settled task keeps its claim: its call's result may land an event
	// later, and in that gap the call is not "a tool running" either.
	claimedToolIDs map[string]struct{}

	generating    string
	rateLimit     *msg.StatusRateLimit
	turnStartedAt time.Time
	changedAt     time.Time

	// harnessCompacting is true while the harness reports, in its own words,
	// that it is compacting. Kept apart from the compact_ack path because a
	// `status` frame with an empty status is also what a permission-mode
	// change sends, and must close only a compaction this flag opened.
	harnessCompacting bool

	// lastStatus is the last status emitted; statusEmitted says whether there
	// has been one. A status is emitted only when it differs from lastStatus.
	lastStatus    msg.SessionStatus
	statusEmitted bool
}

// foldStatus records what ev says about tools, tasks, generation and rate
// limits. Caller holds d.mu. It never touches d.sessionState — derive() owns
// the state machine, and calls clearTurnInFlight when a turn ends.
func (d *derivationState) foldStatus(ev *msg.Event) {
	switch ev.Type {
	case msg.EventUserMessage:
		d.turnStartedAt = eventTime(ev)
		d.generating = ""

	case msg.EventToolCall:
		if ev.ToolCall == nil || ev.ToolCall.ToolID == "" {
			return
		}
		for _, call := range d.toolCalls {
			if call.ToolID == ev.ToolCall.ToolID {
				return
			}
		}
		d.toolCalls = append(d.toolCalls, msg.StatusTool{
			ToolID:    ev.ToolCall.ToolID,
			Name:      ev.ToolCall.Name,
			Summary:   msg.ToolCallSummary(ev.ToolCall.Name, ev.ToolCall.Input),
			StartedAt: eventTime(ev),
		})
		d.generating = ""

	case msg.EventToolResult:
		if ev.ToolResult == nil || ev.ToolResult.ToolID == "" {
			return
		}
		for i, call := range d.toolCalls {
			if call.ToolID == ev.ToolResult.ToolID {
				d.toolCalls = append(d.toolCalls[:i:i], d.toolCalls[i+1:]...)
				break
			}
		}

	case msg.EventThinking:
		d.generating = msg.GeneratingThinking

	case msg.EventStream:
		if ev.Stream == nil || ev.Stream.Delta == nil {
			return
		}
		if ev.Stream.Delta.Type == msg.DeltaThinking {
			d.generating = msg.GeneratingThinking
		} else if ev.Stream.Delta.Type == msg.DeltaText {
			d.generating = msg.GeneratingText
		}

	case msg.EventBlock:
		if ev.Block == nil || ev.Block.Block == nil {
			return
		}
		switch ev.Block.Block.Type {
		case msg.BlockThinking, msg.BlockRedactedThinking:
			d.generating = msg.GeneratingThinking
		case msg.BlockText:
			d.generating = msg.GeneratingText
		}

	case msg.EventSystem:
		if ev.System != nil {
			d.foldSystemStatus(ev)
		}
	}
}

func (d *derivationState) foldSystemStatus(ev *msg.Event) {
	sys := ev.System
	switch sys.Subtype {
	case "task_started":
		if sys.TaskID == "" {
			return
		}
		if sys.ToolUseID != "" {
			if d.claimedToolIDs == nil {
				d.claimedToolIDs = make(map[string]struct{})
			}
			d.claimedToolIDs[sys.ToolUseID] = struct{}{}
		}
		for i := range d.subagents {
			if d.subagents[i].TaskID == sys.TaskID {
				// Claude Code repeats task_started for a task it has already
				// announced. The first one is when it started; a repeat may be
				// the first to carry the promoted session id.
				if sys.SubagentSessionID != "" {
					d.subagents = replaceSubagent(d.subagents, i, func(s *msg.StatusSubagent) {
						s.SessionID = sys.SubagentSessionID
					})
				}
				return
			}
		}
		d.subagents = append(d.subagents, msg.StatusSubagent{
			TaskID:       sys.TaskID,
			TaskType:     sys.TaskType,
			SubagentType: sys.SubagentType,
			Description:  sys.Description,
			StartedAt:    eventTime(ev),
			SessionID:    sys.SubagentSessionID,
		})

	case "task_progress":
		if sys.TaskID == "" || sys.LastToolName == "" {
			return
		}
		for i := range d.subagents {
			if d.subagents[i].TaskID == sys.TaskID && d.subagents[i].LastToolName != sys.LastToolName {
				d.subagents = replaceSubagent(d.subagents, i, func(s *msg.StatusSubagent) {
					s.LastToolName = sys.LastToolName
				})
				return
			}
		}

	case msg.SystemSubtypeBackgroundTasksChanged:
		// The harness's own list of everything running, whole, on every change.
		// It decides MEMBERSHIP: a task it no longer lists is gone even if its
		// terminal frame never arrived, and a task it lists that task_started
		// never announced is running even so. What task_started / task_progress
		// add — the agent role, the last tool, the promoted session — is kept
		// for the tasks that stay.
		listed := make(map[string]msg.BackgroundTask, len(sys.BackgroundTasks))
		for _, task := range sys.BackgroundTasks {
			listed[task.TaskID] = task
		}
		var next []msg.StatusSubagent
		for _, task := range d.subagents {
			if _, still := listed[task.TaskID]; still {
				next = append(next, task)
				delete(listed, task.TaskID)
			}
		}
		for _, task := range sys.BackgroundTasks {
			if unseen, ok := listed[task.TaskID]; ok {
				next = append(next, msg.StatusSubagent{
					TaskID:      unseen.TaskID,
					TaskType:    unseen.TaskType,
					Description: unseen.Description,
					StartedAt:   eventTime(ev),
				})
			}
		}
		d.subagents = next

	case "rate_limit":
		switch sys.RateLimitStatus {
		case "":
			// A frame from an adapter that does not report the verdict.
		case msg.RateLimitAllowed:
			d.rateLimit = nil
		default:
			d.rateLimit = &msg.StatusRateLimit{
				Status:    sys.RateLimitStatus,
				LimitType: sys.RateLimitType,
				ResetsAt:  sys.RateLimitResetsAt,
			}
		}
	}

	// Any frame may carry a terminal task status (task_updated,
	// task_notification, and the cancelled frames the router writes when the
	// process dies), so this is not a subtype case.
	if sys.TaskID != "" && msg.TaskStatusIsTerminal(sys.TaskStatus) {
		for i := range d.subagents {
			if d.subagents[i].TaskID == sys.TaskID {
				d.subagents = append(d.subagents[:i:i], d.subagents[i+1:]...)
				break
			}
		}
	}
}

// replaceSubagent returns a copy of tasks with element i edited. A copy,
// because the previous slice is still referenced by lastStatus and editing it
// in place would change the status it is compared against.
func replaceSubagent(tasks []msg.StatusSubagent, i int, edit func(*msg.StatusSubagent)) []msg.StatusSubagent {
	out := make([]msg.StatusSubagent, len(tasks))
	copy(out, tasks)
	edit(&out[i])
	return out
}

// clearTurnInFlight forgets what only a turn in flight can have: its tool
// calls, its open prompts, what the model was emitting, and when it started.
// Running tasks are kept — see statusTracking.subagents. Caller holds d.mu.
func (d *derivationState) clearTurnInFlight() {
	d.activeTools = map[string]string{}
	d.awaitingApproval = false
	d.pendingApprovals = make(map[string]struct{})
	d.toolCalls = nil
	d.generating = ""
	d.turnStartedAt = time.Time{}
	d.harnessCompacting = false
}

// statusLocked builds the current status. AsOf is left zero: it is the row id
// of the event that carries this status, known only once that event is stored.
// Caller holds d.mu.
func (d *derivationState) statusLocked() msg.SessionStatus {
	status := msg.SessionStatus{
		State:         d.sessionState,
		RateLimit:     d.rateLimit,
		TurnStartedAt: d.turnStartedAt,
		ChangedAt:     d.changedAt,
		Subagents:     d.subagents,
	}
	if d.sessionState == msg.SessionModelGenerating {
		status.Generating = d.generating
	}
	for _, call := range d.toolCalls {
		if _, claimed := d.claimedToolIDs[call.ToolID]; claimed {
			continue
		}
		status.Tools = append(status.Tools, call)
	}
	return status
}

// changedStatusLocked returns the status to emit, or nil when it says nothing
// the last emitted one did not. Caller holds d.mu.
func (d *derivationState) changedStatusLocked() *msg.SessionStatus {
	status := d.statusLocked()
	if d.statusEmitted && d.lastStatus.Equal(status) {
		return nil
	}
	d.lastStatus = status
	d.statusEmitted = true
	return &status
}

// currentStatus is statusLocked for a caller outside the derivation.
func (d *derivationState) currentStatus() msg.SessionStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusLocked()
}

func eventTime(ev *msg.Event) time.Time {
	if ev.Timestamp.IsZero() {
		return time.Now()
	}
	return ev.Timestamp
}
