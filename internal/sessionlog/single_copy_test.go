package sessionlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/store/storetest"
	"github.com/kayushkin/llm-bridge/msg"
)

// newStores is a bridge core store and the session log as the only copy,
// in one test database, as a server opens them.
func newStores(t *testing.T) (*store.Store, *SingleCopy, *storetest.Database) {
	t.Helper()
	database := storetest.NewDatabase(t)
	bridge := database.OpenStore()
	singleCopy, err := OpenSingleCopy(context.Background(), bridge, database.OpenSessionLog(false))
	if err != nil {
		t.Fatal(err)
	}
	return bridge, singleCopy, database
}

func createSession(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	if err := st.CreateSession(&store.Session{SessionID: sessionID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		t.Fatal(err)
	}
}

func recordOf(sessionID string, ev *msg.Event) store.SessionEventRecord {
	ev.BridgeSessionID = sessionID
	data, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	record := store.SessionEventRecord{SessionID: sessionID, Type: string(ev.Type), MessageID: ev.MessageID, HarnessMessageID: ev.HarnessMessageID, Data: data, Event: ev}
	switch {
	case ev.ToolCall != nil:
		record.ToolUseID = ev.ToolCall.ToolID
		record.ToolName = ev.ToolCall.Name
	case ev.ToolResult != nil:
		record.ToolUseID = ev.ToolResult.ToolID
	}
	return record
}

// parityStep is one write both stores are given.
type parityStep struct {
	event  *msg.Event
	status bool
}

// parityConversation is a made-up session that touches every reader: two
// turns, a user message stored twice under one turn id, message ids, tools,
// a ScheduleWakeup call and its result, background tasks, statuses, and a
// turn left open.
func parityConversation() []parityStep {
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	status := func(state msg.SessionState) parityStep {
		return parityStep{status: true, event: &msg.Event{Type: msg.EventSessionStatus, Harness: msg.HarnessClaudeCode, Timestamp: at,
			Status: &msg.SessionStatus{State: state, Reason: msg.ReasonStartupReconcile, ChangedAt: at}}}
	}
	event := func(ev msg.Event) parityStep {
		ev.Harness = msg.HarnessClaudeCode
		ev.Timestamp = at
		return parityStep{event: &ev}
	}
	wakeupResult, _ := json.Marshal(map[string]any{"tool_use_result": map[string]any{"scheduledFor": at.Add(time.Hour).UnixMilli()}})
	return []parityStep{
		event(msg.Event{Type: msg.EventUserMessage, TurnID: "turn-1", Result: &msg.ResultEvent{Text: "first request"}}),
		status(msg.SessionModelGenerating),
		event(msg.Event{Type: msg.EventBlock, TurnID: "turn-1", MessageID: "message-1", HarnessMessageID: "harness-1",
			Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "first answer"}}}}),
		event(msg.Event{Type: msg.EventToolCall, TurnID: "turn-1", MessageID: "message-1", HarnessMessageID: "harness-1",
			ToolCall: &msg.ToolCallEvent{ToolID: "tool-1", Name: store.ClaudeCodeScheduleWakeupToolName, Input: json.RawMessage(`{"prompt":"wake up"}`)}}),
		event(msg.Event{Type: msg.EventToolResult, TurnID: "turn-1", MessageID: "message-1", HarnessMessageID: "harness-1",
			ToolResult: &msg.ToolResultEvent{ToolID: "tool-1", Name: store.ClaudeCodeScheduleWakeupToolName, Output: "scheduled"}, Raw: wakeupResult}),
		event(msg.Event{Type: msg.EventSystem, TurnID: "turn-1",
			System: &msg.SystemEvent{Subtype: msg.SystemSubtypeBackgroundTasksChanged, BackgroundTasks: []msg.BackgroundTask{{TaskID: "task-1", Description: "made up"}}}}),
		event(msg.Event{Type: msg.EventResult, TurnID: "turn-1", MessageID: "message-1", Result: &msg.ResultEvent{Text: "done"}}),
		status(msg.SessionIdle),
		event(msg.Event{Type: msg.EventUserMessage, TurnID: "turn-2", Result: &msg.ResultEvent{Text: "second request"}}),
		event(msg.Event{Type: msg.EventUserMessage, TurnID: "turn-2", Result: &msg.ResultEvent{Text: "second request"}}),
		event(msg.Event{Type: msg.EventBlock, TurnID: "turn-2", MessageID: "message-2", HarnessMessageID: "harness-2",
			Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "   "}}}}),
		event(msg.Event{Type: msg.EventToolCall, TurnID: "turn-2", MessageID: "message-2", HarnessMessageID: "harness-2",
			ToolCall: &msg.ToolCallEvent{ToolID: "tool-2", Name: "Bash", Input: json.RawMessage(`{"command":"true"}`)}}),
		event(msg.Event{Type: msg.EventBlock, TurnID: "turn-2", MessageID: "message-2", HarnessMessageID: "harness-2",
			Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "second answer so far"}}}}),
		status(msg.SessionToolRunning),
	}
}

// writeConversation writes the steps through one store and answers the id
// of each.
func writeConversation(t *testing.T, events Store, sessionID string, steps []parityStep) []int64 {
	t.Helper()
	ids := make([]int64, len(steps))
	for i, step := range steps {
		ev := *step.event
		if step.status {
			status := *ev.Status
			ev.Status = &status
			ev.BridgeSessionID = sessionID
			id, err := events.WriteSessionStatus(sessionID, &ev)
			if err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			ids[i] = id
			continue
		}
		id, err := events.StoreSessionEvent(recordOf(sessionID, &ev))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

// positionsOf turns event ids into step positions, so two stores' answers
// compare whatever ids each handed out. A status's as_of is an id too.
func positionsOf(t *testing.T, ids []int64, events []store.EventWithID) []string {
	t.Helper()
	position := map[int64]int{}
	for i, id := range ids {
		position[id] = i
	}
	out := make([]string, len(events))
	for i, event := range events {
		var ev msg.Event
		if err := json.Unmarshal(event.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Status != nil {
			ev.Status.AsOf = int64(position[ev.Status.AsOf])
		}
		normalized, _ := json.Marshal(ev)
		out[i] = string(rune('0'+position[int64(event.RowID)])) + " " + string(normalized)
	}
	return out
}

// positionsOf turns event ids into step positions, so an answer compares
// whatever ids the store handed out. A status's as_of is an id too.
func positionsOfEvents(t *testing.T, ids []int64, events []store.EventWithID) []int {
	t.Helper()
	position := map[int64]int{}
	for i, id := range ids {
		position[id] = i
	}
	out := make([]int, len(events))
	for i, event := range events {
		found, ok := position[int64(event.RowID)]
		if !ok {
			t.Fatalf("event %d is none of the conversation's", event.RowID)
		}
		out[i] = found
	}
	return out
}

// TestSingleCopyReadersAnswerWhatTheConversationHolds writes one
// conversation and holds every reader to what it holds.
func TestSingleCopyReadersAnswerWhatTheConversationHolds(t *testing.T) {
	const sessionID = "br_parity"
	bridge, single, _ := newStores(t)
	createSession(t, bridge, sessionID)
	steps := parityConversation()
	ids := writeConversation(t, single, sessionID, steps)
	last := len(steps) - 1

	heads, err := single.MaxEventRowIDs([]string{sessionID, "br_no_events"})
	if err != nil || len(heads) != 2 || heads[sessionID] != ids[last] || heads["br_no_events"] != 0 {
		t.Errorf("stream heads %v, %v; want %s at %d and br_no_events at 0", heads, err, sessionID, ids[last])
	}
	if head, err := single.MaxEventRowID(sessionID); err != nil || head != ids[last] {
		t.Errorf("stream head %d, %v; want the last id written, %d", head, err, ids[last])
	}

	same := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\ngot  %#v\nwant %#v", what, got, want)
		}
	}
	for _, after := range []int{0, 3, 8, last} {
		events, err := single.ListEventsSinceID(sessionID, int(ids[after]))
		if err != nil {
			t.Fatal(err)
		}
		want := []int{}
		for position := after + 1; position <= last; position++ {
			want = append(want, position)
		}
		same("events after step", positionsOfEvents(t, ids, events), want)
	}
	// After the last user message (step 9): everything but user messages.
	turn, err := single.ListCurrentTurnEventsWithIDs(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	same("current turn", positionsOfEvents(t, ids, turn), []int{10, 11, 12, 13})

	harnessIDs, err := single.HarnessToBridgeMap(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	same("harness to bridge message ids", harnessIDs, map[string]string{"harness-1": "message-1", "harness-2": "message-2"})
	toolUses, err := single.ToolUseToMessageMap(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	same("tool use message ids", toolUses, map[string]store.ToolUseBinding{
		"tool-1": {BridgeMessageID: "message-1", HarnessMessageID: "harness-1"},
		"tool-2": {BridgeMessageID: "message-2", HarnessMessageID: "harness-2"},
	})
	turns, err := single.CountUserTurns(sessionID)
	same("user turns", []any{turns, err}, []any{2, error(nil)})
	first, err := single.FirstUserMessage(sessionID)
	same("first user message", []any{first, err}, []any{"first request", error(nil)})
	tasks, err := single.LastReportedBackgroundTasks(sessionID)
	same("background tasks", []any{tasks, err}, []any{[]msg.BackgroundTask{{TaskID: "task-1", Description: "made up"}}, error(nil)})
	userMessage, texts, err := single.CurrentTurnText(sessionID)
	same("current turn text", []any{userMessage, texts, err}, []any{"second request", []string{"second answer so far"}, error(nil)})
	wakeup, err := single.LatestScheduledWakeup(sessionID, time.Now().Add(-time.Hour))
	if err != nil || wakeup == nil || wakeup.Prompt != "wake up" || wakeup.ToolID != "tool-1" ||
		!wakeup.DueAt.Equal(time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("wakeup %+v, %v; want tool-1's, due at 13:00", wakeup, err)
	}
	if wakeup, err := single.LatestScheduledWakeup(sessionID, time.Now().Add(time.Hour)); err != nil || wakeup != nil {
		t.Errorf("a wakeup called before the window: %+v, %v; want none", wakeup, err)
	}
	if since, err := single.UserMessageLoggedSince(sessionID, time.Now().Add(-time.Hour)); err != nil || !since {
		t.Errorf("a user message in the last hour: %v, %v; want one", since, err)
	}
	if since, err := single.UserMessageLoggedSince(sessionID, time.Now().Add(time.Hour)); err != nil || since {
		t.Errorf("a user message from an hour on: %v, %v; want none", since, err)
	}
	if activity, err := single.LastActivityAt("br_no_events"); err != nil || !activity.IsZero() {
		t.Errorf("a session with no events was last active at %v, %v; want the zero time", activity, err)
	}
	if activity, err := single.LastActivityAt(sessionID); err != nil || time.Since(activity) > time.Minute || time.Since(activity) < -time.Minute {
		t.Errorf("last activity %v, %v; want about now", activity, err)
	}

	row, err := bridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != string(msg.SessionToolRunning) || row.Status.AsOf != ids[last] {
		t.Errorf("session row %q as of %d; want tool_running as of %d", row.State, row.Status.AsOf, ids[last])
	}

	reconciled, err := single.ReconcileSessions(msg.ActiveSessionStates()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].Status.Status.State != msg.SessionIdle {
		t.Fatalf("reconcile: %+v; want the one session made idle", reconciled)
	}
	row, err = bridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := single.MaxEventRowID(sessionID)
	if row.State != string(msg.SessionIdle) || row.Status.AsOf != head || reconciled[0].Status.Status.AsOf != head {
		t.Errorf("after reconcile the row is %q as of %d, the status as of %d; want idle as of the session's newest event %d",
			row.State, row.Status.AsOf, reconciled[0].Status.Status.AsOf, head)
	}
	if !row.UpdatedAt.Equal(reconciled[0].Session.UpdatedAt) {
		t.Errorf("reconcile moved updated_at from %v to %v; it is left as the row's last real change", reconciled[0].Session.UpdatedAt, row.UpdatedAt)
	}
}

func TestSingleCopyNumbersAboveTheArchivedEvents(t *testing.T) {
	database := storetest.NewDatabase(t)
	database.RecordArchivedEventNumbering(5_000_001)
	bridge := database.OpenStore()
	createSession(t, bridge, "br_numbering")
	sessionLog := database.OpenSessionLog(false)
	single, err := OpenSingleCopy(context.Background(), bridge, sessionLog)
	if err != nil {
		t.Fatal(err)
	}
	if single.Numbering().FirstEventID != 5_000_002 || single.Numbering().BridgeDatabaseHighestEventID != 5_000_001 {
		t.Fatalf("numbering %+v", single.Numbering())
	}
	ev := msg.Event{Type: msg.EventSystem}
	id, err := single.StoreSessionEvent(recordOf("br_numbering", &ev))
	if err != nil || id != 5_000_002 {
		t.Fatalf("first id %d, %v; want 5000002", id, err)
	}
	if !single.StreamCursorFromEarlierNumbering(5_000_001) || single.StreamCursorFromEarlierNumbering(id) || single.StreamCursorFromEarlierNumbering(0) {
		t.Error("a cursor below the first id is from the archived numbering, and only such a cursor")
	}

	// Opening again keeps the record and numbers on.
	again, err := OpenSingleCopy(context.Background(), bridge, sessionLog)
	if err != nil {
		t.Fatal(err)
	}
	if again.Numbering().FirstEventID != 5_000_002 {
		t.Errorf("reopened numbering %+v; want the first record kept", again.Numbering())
	}
	next, err := again.StoreSessionEvent(recordOf("br_numbering", &ev))
	if err != nil || next <= id {
		t.Errorf("next id %d, %v; want above %d", next, err, id)
	}
}

// A runner entry's event and its spawn's number commit together, and an
// entry written again under its key (a retry after a commit that was cut
// off) is found stored, not stored twice.
func TestARunnerEntryAndItsSpawnsNumberAreStoredTogether(t *testing.T) {
	bridge, single, _ := newStores(t)
	createSession(t, bridge, "br_runner")
	if err := bridge.RecordRunnerSpawn("br_runner", "spawn_made_up", "machine_made_up"); err != nil {
		t.Fatal(err)
	}
	ev := msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "tool-1", Name: "Bash"}}
	record := recordOf("br_runner", &ev)

	id, err := single.StoreSessionEventAdvancingRunnerSpawn(record, "br_runner", "spawn_made_up", 1)
	if err != nil {
		t.Fatal(err)
	}
	spawn, _ := bridge.RunnerSpawnOfSession("br_runner")
	if spawn.LastSequence != 1 {
		t.Fatalf("spawn stored through %d; want 1", spawn.LastSequence)
	}

	again, err := single.StoreSessionEventAdvancingRunnerSpawn(record, "br_runner", "spawn_made_up", 1)
	if err != nil || again != id {
		t.Errorf("entry 1 written again: %d, %v; want the first write's %d", again, err, id)
	}
	events, err := single.ListEventsSinceID("br_runner", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("%d events, %v; want the entry stored once", len(events), err)
	}

	next, err := single.StoreSessionEventAdvancingRunnerSpawn(record, "br_runner", "spawn_made_up", 2)
	if err != nil || next <= id {
		t.Errorf("entry 2 stored as %d, %v", next, err)
	}
	spawn, _ = bridge.RunnerSpawnOfSession("br_runner")
	if spawn.LastSequence != 2 {
		t.Errorf("spawn stored through %d; want 2", spawn.LastSequence)
	}
}

func TestAStatusForASessionWithNoRowStoresNothing(t *testing.T) {
	_, single, _ := newStores(t)
	_, err := single.WriteSessionStatus("br_hook_only", &msg.Event{Type: msg.EventSessionStatus, Status: &msg.SessionStatus{State: msg.SessionIdle}})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err %v; want sql.ErrNoRows", err)
	}
	events, err := single.ListEventsSinceID("br_hook_only", 0)
	if err != nil || len(events) != 0 {
		t.Errorf("%d events stored, %v; want none", len(events), err)
	}
	// The writer's connection is usable after the rolled-back transaction.
	ev := msg.Event{Type: msg.EventSystem}
	if _, err := single.StoreSessionEvent(recordOf("br_hook_only", &ev)); err != nil {
		t.Errorf("the next write failed: %v", err)
	}
}

// A status whose row cannot be written stores no event: the two are one
// transaction.
func TestAStatusWhoseRowWriteFailsStoresNoEvent(t *testing.T) {
	bridge, single, database := newStores(t)
	createSession(t, bridge, "br_refused")
	// Take away the session log's right to write the row.
	core := database.Database.Open(database.CoreModule())
	if _, err := core.Exec(`REVOKE UPDATE (state, status, updated_at) ON sessions FROM session_log_module`); err != nil {
		t.Fatal(err)
	}
	_, err := single.WriteSessionStatus("br_refused", &msg.Event{Type: msg.EventSessionStatus, Status: &msg.SessionStatus{State: msg.SessionModelGenerating}})
	if err == nil {
		t.Fatal("the status was written with the row write refused")
	}
	events, err := single.ListEventsSinceID("br_refused", 0)
	if err != nil || len(events) != 0 {
		t.Errorf("%d events stored, %v; want none", len(events), err)
	}
	row, _ := bridge.GetSession("br_refused")
	if row.State != string(msg.SessionIdle) {
		t.Errorf("row %q; want it left idle", row.State)
	}
}
