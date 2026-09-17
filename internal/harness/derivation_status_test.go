package harness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// statusAfter feeds events through a derivation and returns the last
// session_status it emitted, or nil when the last event emitted none.
func statusAfter(d *derivationState, events ...*msg.Event) *msg.SessionStatus {
	var last *msg.SessionStatus
	for _, ev := range events {
		last = nil
		for _, derived := range d.derive(ev) {
			if derived.Type == msg.EventSessionStatus {
				last = derived.Status
			}
		}
	}
	return last
}

func toolCall(id, name, input string) *msg.Event {
	return &msg.Event{Type: msg.EventToolCall, Timestamp: time.Unix(100, 0),
		ToolCall: &msg.ToolCallEvent{ToolID: id, Name: name, Input: json.RawMessage(input)}}
}

func toolResult(id string) *msg.Event {
	return &msg.Event{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: id}}
}

func systemEvent(sys msg.SystemEvent) *msg.Event {
	return &msg.Event{Type: msg.EventSystem, Timestamp: time.Unix(200, 0), System: &sys}
}

func userMessage() *msg.Event {
	return &msg.Event{Type: msg.EventUserMessage, Timestamp: time.Unix(50, 0), Result: &msg.ResultEvent{Text: "go"}}
}

func TestStatus_ToolInFlightCarriesNameSummaryAndStart(t *testing.T) {
	d := newDerivationState()
	got := statusAfter(d, userMessage(), toolCall("t1", "Bash", `{"command":"cat thing.txt"}`))
	if got == nil || got.State != msg.SessionToolRunning || len(got.Tools) != 1 {
		t.Fatalf("status = %+v; want tool_running with one tool", got)
	}
	tool := got.Tools[0]
	if tool.ToolID != "t1" || tool.Name != "Bash" || tool.Summary != "cat thing.txt" || !tool.StartedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("tool = %+v", tool)
	}
	if !got.TurnStartedAt.Equal(time.Unix(50, 0)) {
		t.Fatalf("turn started at %v; want the user message's time", got.TurnStartedAt)
	}
}

// The case a state transition cannot report: the state is already
// tool_running, so no session_state is emitted, and until the status existed
// nothing told a client a second tool had joined the first.
func TestStatus_SecondParallelToolIsAStatusChangeWithoutAStateChange(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage(), toolCall("t1", "Read", `{"file_path":"/a"}`))
	got := statusAfter(d, toolCall("t2", "Grep", `{"pattern":"x"}`))
	if got == nil || len(got.Tools) != 2 || got.Tools[0].ToolID != "t1" || got.Tools[1].ToolID != "t2" {
		t.Fatalf("status = %+v; want both tools, oldest first", got)
	}
	got = statusAfter(d, toolResult("t1"))
	if got == nil || got.State != msg.SessionToolRunning || len(got.Tools) != 1 || got.Tools[0].ToolID != "t2" {
		t.Fatalf("status = %+v; want tool_running with t2 alone", got)
	}
}

// A Bash call is in flight and the user presses Stop. The server decides what
// that means — nothing is in flight any more — and says so in one status, so no
// client has to weigh "a tool_call with no result" against "paused".
func TestStatus_PauseWithAToolInFlightReportsNoTools(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage(), toolCall("t1", "Bash", `{"command":"sleep 600"}`))
	if _, changed := d.forceState(msg.SessionPaused); !changed {
		t.Fatal("forceState reported no change")
	}
	got := d.currentStatus()
	if got.State != msg.SessionPaused || len(got.Tools) != 0 || !got.TurnStartedAt.IsZero() {
		t.Fatalf("status = %+v; want paused with nothing in flight", got)
	}
}

func TestStatus_GeneratingSaysThinkingOrText(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage())
	thinking := &msg.Event{Type: msg.EventStream, Stream: &msg.HarnessStream{Delta: &msg.BlockDelta{Type: msg.DeltaThinking}}}
	text := &msg.Event{Type: msg.EventStream, Stream: &msg.HarnessStream{Delta: &msg.BlockDelta{Type: msg.DeltaText}}}
	if got := statusAfter(d, thinking); got == nil || got.Generating != msg.GeneratingThinking {
		t.Fatalf("after a thinking delta: %+v", got)
	}
	if got := statusAfter(d, thinking); got != nil {
		t.Fatalf("a second thinking delta changes nothing and must emit nothing; got %+v", got)
	}
	if got := statusAfter(d, text); got == nil || got.Generating != msg.GeneratingText {
		t.Fatalf("after a text delta: %+v", got)
	}
	if got := statusAfter(d, toolCall("t1", "Read", `{"file_path":"/a"}`)); got == nil || got.Generating != "" {
		t.Fatalf("generating must be empty outside model_generating; got %+v", got)
	}
}

func TestStatus_SubagentIsListedOnceAndNotAlsoAsATool(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage(), toolCall("toolu_agent", "Agent", `{"description":"find the parser"}`))
	got := statusAfter(d, systemEvent(msg.SystemEvent{
		Subtype: "task_started", TaskID: "task1", ToolUseID: "toolu_agent", TaskType: msg.TaskTypeLocalAgent,
		SubagentType: "Explore", Description: "find the parser", SubagentSessionID: "br_sub",
	}))
	if got == nil || len(got.Tools) != 0 || len(got.Subagents) != 1 {
		t.Fatalf("status = %+v; want the Agent call claimed by its subagent", got)
	}
	sub := got.Subagents[0]
	if sub.TaskID != "task1" || sub.SubagentType != "Explore" || sub.Description != "find the parser" ||
		sub.SessionID != "br_sub" || !sub.StartedAt.Equal(time.Unix(200, 0)) {
		t.Fatalf("subagent = %+v", sub)
	}

	got = statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "task_progress", TaskID: "task1", LastToolName: "Grep"}))
	if got == nil || got.Subagents[0].LastToolName != "Grep" {
		t.Fatalf("after task_progress: %+v", got)
	}

	got = statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "task_notification", TaskID: "task1", TaskStatus: msg.TaskStatusCompleted}))
	if got == nil || len(got.Subagents) != 0 {
		t.Fatalf("after a terminal task status: %+v", got)
	}
}

// A backgrounded shell outlives the turn that started it, and is still running.
func TestStatus_RunningTaskSurvivesTheEndOfItsTurn(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage(), systemEvent(msg.SystemEvent{
		Subtype: "task_started", TaskID: "bg1", ToolUseID: "toolu_bash", TaskType: msg.TaskTypeLocalBash, Description: "npm run dev",
	}))
	got := statusAfter(d, &msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "started it."}})
	if got == nil || got.State != msg.SessionIdle || len(got.Subagents) != 1 {
		t.Fatalf("status = %+v; want idle with the shell still listed", got)
	}
}

// The sequence stored for br_1789576721656282047 on 2026-09-16: three
// `compacting` frames 30s apart, then a closing frame, then compact_boundary.
// No compact_ack precedes an automatic compaction.
func TestStatus_AutomaticCompactionIsReportedWhileItRuns(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage(), toolCall("t1", "Bash", `{"command":"x"}`))
	compacting := systemEvent(msg.SystemEvent{Subtype: "status", Status: msg.SystemStatusCompacting})
	if got := statusAfter(d, compacting); got == nil || got.State != msg.SessionCompacting {
		t.Fatalf("first compacting frame: %+v", got)
	}
	if got := statusAfter(d, compacting); got != nil {
		t.Fatalf("a repeated compacting frame must emit nothing; got %+v", got)
	}
	got := statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "status", CompactResult: "success"}))
	if got == nil || got.State != msg.SessionToolRunning {
		t.Fatalf("closing frame: %+v; want the state the compaction interrupted", got)
	}
	if got := statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "compact_boundary"})); got != nil {
		t.Fatalf("compact_boundary after the close changes nothing; got %+v", got)
	}
}

// Claude Code sends the same empty `status` frame when the permission mode
// changes. It must not end a compaction it did not open, nor move a session
// that was never compacting.
func TestStatus_EmptyStatusFrameOutsideACompactionChangesNothing(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage())
	if got := statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "status"})); got != nil {
		t.Fatalf("got %+v; want no status", got)
	}
}

func TestStatus_RateLimitVerdictIsCarriedUntilAllowed(t *testing.T) {
	d := newDerivationState()
	statusAfter(d, userMessage())
	got := statusAfter(d, systemEvent(msg.SystemEvent{
		Subtype: "rate_limit", RateLimitStatus: msg.RateLimitRejected, RateLimitType: "seven_day", RateLimitResetsAt: 1789686000,
	}))
	if got == nil || got.RateLimit == nil || *got.RateLimit != (msg.StatusRateLimit{Status: msg.RateLimitRejected, LimitType: "seven_day", ResetsAt: 1789686000}) {
		t.Fatalf("status = %+v", got)
	}
	// A rejected limit ends the turn in an error; the reason outlives it.
	got = statusAfter(d, &msg.Event{Type: msg.EventError, Error: &msg.ErrorEvent{Code: "EXECUTION_ERROR"}})
	if got == nil || got.State != msg.SessionError || got.RateLimit == nil {
		t.Fatalf("after the error: %+v; want error with the rate limit still reported", got)
	}
	got = statusAfter(d, systemEvent(msg.SystemEvent{Subtype: "rate_limit", RateLimitStatus: msg.RateLimitAllowed}))
	if got == nil || got.RateLimit != nil {
		t.Fatalf("after allowed: %+v; want the report cleared", got)
	}
}
