package harness

import (
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// backgroundTasksChanged is the frame a harness sends with its whole list of
// running background tasks.
func backgroundTasksChanged(tasks ...msg.BackgroundTask) msg.Event {
	return msg.Event{Type: msg.EventSystem, System: &msg.SystemEvent{
		Subtype:         msg.SystemSubtypeBackgroundTasksChanged,
		BackgroundTasks: tasks,
	}}
}

var (
	searchSubagent = msg.BackgroundTask{TaskID: "af9b03cbb5668505d", TaskType: msg.TaskTypeLocalAgent, Description: "Map dash frontend and proxies"}
	testRunInShell = msg.BackgroundTask{TaskID: "bq136qbl3", TaskType: msg.TaskTypeLocalBash, Description: "Wait for the full test run to finish"}
)

// The sequence that lost a session on 2026-09-17: the turn launched subagents
// and ended while they were still searching. It was derived idle, the restart
// reconcile selects on active states, and a redeploy killed it for good.
func TestDerivation_TurnEndingOverRunningBackgroundTasksIsNotIdle(t *testing.T) {
	d := newDerivationState()
	got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "t1", Name: "Agent"}},
		backgroundTasksChanged(searchSubagent),
		{Type: msg.EventToolResult, ToolResult: &msg.ToolResultEvent{ToolID: "t1"}},
		{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "The map comes once the sweeps report."}},
	})
	want := []msg.SessionState{
		msg.SessionModelGenerating,
		msg.SessionToolRunning,
		msg.SessionModelGenerating,
		msg.SessionBackgroundTasksRunning,
	}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
	if !d.currentState().IsActive() {
		t.Errorf("state %q is not active, so a restart would not resume this session", d.currentState())
	}
}

// A backgrounded shell command holds the session active exactly as a subagent
// does: the list is of tasks, not of agents.
func TestDerivation_ABackgroundedShellCommandCountsAsABackgroundTask(t *testing.T) {
	d := newDerivationState()
	sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(testRunInShell),
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
	})
	if d.currentState() != msg.SessionBackgroundTasksRunning {
		t.Fatalf("state = %q; want %q", d.currentState(), msg.SessionBackgroundTasksRunning)
	}
}

// The list shrinking is not the list emptying: one task reporting leaves the
// others running.
func TestDerivation_StaysActiveUntilTheLastBackgroundTaskFinishes(t *testing.T) {
	d := newDerivationState()
	got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(searchSubagent, testRunInShell),
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
		backgroundTasksChanged(testRunInShell),
		backgroundTasksChanged(),
	})
	want := []msg.SessionState{
		msg.SessionModelGenerating,
		msg.SessionBackgroundTasksRunning,
		msg.SessionIdle,
	}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
}

// Tasks that finished before the turn did leave nothing running, and the turn
// settles to idle as it always has.
func TestDerivation_BackgroundTasksThatFinishedMidTurnDoNotHoldTheSession(t *testing.T) {
	d := newDerivationState()
	sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(searchSubagent),
		backgroundTasksChanged(),
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
	})
	if d.currentState() != msg.SessionIdle {
		t.Fatalf("state = %q; want idle", d.currentState())
	}
}

// A question asked over running tasks must still be waiting when they finish.
func TestDerivation_AQuestionAskedOverBackgroundTasksSurvivesThem(t *testing.T) {
	d := newDerivationState()
	got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(searchSubagent),
		{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "Which of the two do you want?"}},
		backgroundTasksChanged(),
	})
	want := []msg.SessionState{
		msg.SessionModelGenerating,
		msg.SessionBackgroundTasksRunning,
		msg.SessionAwaitingUser,
	}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
}

// The turn-end classifier answers late and writes through applyExternalState.
// While tasks hold the session active its verdict must not replace that state —
// it is still true — but must be what the session settles to afterwards.
func TestDerivation_AClassifierVerdictIsHeldUnderRunningBackgroundTasks(t *testing.T) {
	d := newDerivationState()
	sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(searchSubagent),
		{Type: msg.EventResult, Result: &msg.ResultEvent{Text: "Done for now."}},
	})
	if ev := d.applyExternalState(msg.SessionAwaitingUser, "turn_complete_signal_question",
		msg.SessionIdle, msg.SessionAwaitingUser); ev != nil {
		t.Fatalf("verdict moved the live state to %q while tasks were running", ev.State.State)
	}
	if d.currentState() != msg.SessionBackgroundTasksRunning {
		t.Fatalf("state = %q; want %q", d.currentState(), msg.SessionBackgroundTasksRunning)
	}
	got := sessionStateOf(t, d, []msg.Event{backgroundTasksChanged()})
	if !equalStates(got, []msg.SessionState{msg.SessionAwaitingUser}) {
		t.Fatalf("after the tasks finished: %v; want [awaiting_user]", got)
	}
}

// The harness opens its own turn when a task reports; that turn runs as any
// other and ends over whatever is still running.
func TestDerivation_ATurnOpenedByATaskReportReturnsToBackgroundTasksRunning(t *testing.T) {
	d := newDerivationState()
	got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		backgroundTasksChanged(searchSubagent, testRunInShell),
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
		backgroundTasksChanged(testRunInShell),
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
	})
	want := []msg.SessionState{
		msg.SessionModelGenerating,
		msg.SessionBackgroundTasksRunning,
		msg.SessionModelGenerating,
		msg.SessionBackgroundTasksRunning,
	}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
}

// A list that lands after the result which ended the turn still counts.
func TestDerivation_ABackgroundTaskListArrivingAfterTheResultStillCounts(t *testing.T) {
	d := newDerivationState()
	got := sessionStateOf(t, d, []msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventResult, Result: &msg.ResultEvent{}},
		backgroundTasksChanged(searchSubagent),
	})
	want := []msg.SessionState{msg.SessionModelGenerating, msg.SessionIdle, msg.SessionBackgroundTasksRunning}
	if !equalStates(got, want) {
		t.Fatalf("transitions = %v; want %v", got, want)
	}
}
