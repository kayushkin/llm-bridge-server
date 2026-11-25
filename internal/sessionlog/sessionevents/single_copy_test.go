package sessionevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

func newBridgeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newSessionLog(t *testing.T) *server.Server {
	t.Helper()
	module, err := logstorestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	writer := database.Open(module)
	reader, err := logstorestore.OpenReader(context.Background(), database.URL(), module)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return server.New(logstorestore.New(writer, reader), nil)
}

func newSingleCopy(t *testing.T, bridge *store.Store) *SingleCopy {
	t.Helper()
	singleCopy, err := OpenSingleCopy(context.Background(), bridge, newSessionLog(t))
	if err != nil {
		t.Fatal(err)
	}
	return singleCopy
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

// TestSingleCopyReadersAnswerWhatTheBridgeDatabaseAnswered writes one
// conversation through both stores and holds every reader of the session
// log to what the bridge database's events table answers.
func TestSingleCopyReadersAnswerWhatTheBridgeDatabaseAnswered(t *testing.T) {
	const sessionID = "br_parity"
	oldBridge := newBridgeStore(t)
	createSession(t, oldBridge, sessionID)
	old := BridgeDatabase{Store: oldBridge}
	newBridge := newBridgeStore(t)
	createSession(t, newBridge, sessionID)
	single := newSingleCopy(t, newBridge)

	steps := parityConversation()
	oldIDs := writeConversation(t, old, sessionID, steps)
	newIDs := writeConversation(t, single, sessionID, steps)

	for name, events := range map[string]Reads{"bridge database": old, "single copy": single} {
		heads, err := events.MaxEventRowIDs([]string{sessionID, "br_no_events"})
		head, _ := events.MaxEventRowID(sessionID)
		if err != nil || len(heads) != 2 || heads[sessionID] != head || heads["br_no_events"] != 0 {
			t.Errorf("%s: stream heads %v, %v; want %s at %d and br_no_events at 0", name, heads, err, sessionID, head)
		}
	}
	for name, store := range map[string]struct {
		events Reads
		ids    []int64
	}{"bridge database": {old, oldIDs}, "single copy": {single, newIDs}} {
		head, err := store.events.MaxEventRowID(sessionID)
		if err != nil || head != store.ids[len(store.ids)-1] {
			t.Errorf("%s: stream head %d, %v; want the last id written, %d", name, head, err, store.ids[len(store.ids)-1])
		}
	}

	compare := func(what string, oldAnswer, newAnswer any) {
		t.Helper()
		if !reflect.DeepEqual(oldAnswer, newAnswer) {
			t.Errorf("%s:\nbridge database %#v\nsingle copy     %#v", what, oldAnswer, newAnswer)
		}
	}
	for _, after := range []int{0, 3, 8, len(steps) - 1} {
		oldEvents, err := old.ListEventsSinceID(sessionID, int(oldIDs[after]))
		if err != nil {
			t.Fatal(err)
		}
		newEvents, err := single.ListEventsSinceID(sessionID, int(newIDs[after]))
		if err != nil {
			t.Fatal(err)
		}
		compare("events after step", positionsOf(t, oldIDs, oldEvents), positionsOf(t, newIDs, newEvents))
	}
	oldTurn, err := old.ListCurrentTurnEventsWithIDs(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	newTurn, err := single.ListCurrentTurnEventsWithIDs(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldTurn) == 0 {
		t.Fatal("the bridge database replayed no current turn")
	}
	compare("current turn", positionsOf(t, oldIDs, oldTurn), positionsOf(t, newIDs, newTurn))

	answer := func(read func(Reads) (any, error)) (any, any) {
		t.Helper()
		oldAnswer, err := read(old)
		if err != nil {
			t.Fatal(err)
		}
		newAnswer, err := read(single)
		if err != nil {
			t.Fatal(err)
		}
		return oldAnswer, newAnswer
	}
	same := func(what string, read func(Reads) (any, error)) {
		t.Helper()
		oldAnswer, newAnswer := answer(read)
		compare(what, oldAnswer, newAnswer)
	}
	same("harness to bridge message ids", func(r Reads) (any, error) { return r.HarnessToBridgeMap(sessionID) })
	same("tool use message ids", func(r Reads) (any, error) { return r.ToolUseToMessageMap(sessionID) })
	same("user turns", func(r Reads) (any, error) { return r.CountUserTurns(sessionID) })
	same("first user message", func(r Reads) (any, error) { return r.FirstUserMessage(sessionID) })
	same("background tasks", func(r Reads) (any, error) { return r.LastReportedBackgroundTasks(sessionID) })
	same("current turn text", func(r Reads) (any, error) {
		user, texts, err := r.CurrentTurnText(sessionID)
		return []any{user, texts}, err
	})
	same("wakeup", func(r Reads) (any, error) {
		return r.LatestScheduledWakeup(sessionID, time.Now().Add(-time.Hour))
	})
	same("wakeup before the window", func(r Reads) (any, error) {
		return r.LatestScheduledWakeup(sessionID, time.Now().Add(time.Hour))
	})
	same("user message since an hour ago", func(r Reads) (any, error) {
		return r.UserMessageLoggedSince(sessionID, time.Now().Add(-time.Hour))
	})
	same("user message since an hour on", func(r Reads) (any, error) {
		return r.UserMessageLoggedSince(sessionID, time.Now().Add(time.Hour))
	})
	same("a session with no events", func(r Reads) (any, error) { return r.LastActivityAt("br_no_events") })
	// The conversation is built so every reader has something to answer;
	// two empty answers would agree and prove nothing.
	harnessIDs, _ := single.HarnessToBridgeMap(sessionID)
	toolUses, _ := single.ToolUseToMessageMap(sessionID)
	turns, _ := single.CountUserTurns(sessionID)
	tasks, _ := single.LastReportedBackgroundTasks(sessionID)
	wakeup, _ := single.LatestScheduledWakeup(sessionID, time.Now().Add(-time.Hour))
	userMessage, texts, _ := single.CurrentTurnText(sessionID)
	if len(harnessIDs) != 2 || len(toolUses) != 2 || turns != 2 || len(tasks) != 1 || wakeup == nil || wakeup.Prompt != "wake up" ||
		userMessage != "second request" || len(texts) != 1 {
		t.Errorf("readers answered too little: %v %v %d %v %+v %q %q", harnessIDs, toolUses, turns, tasks, wakeup, userMessage, texts)
	}
	oldActivity, newActivity := answer(func(r Reads) (any, error) { return r.LastActivityAt(sessionID) })
	if difference := oldActivity.(time.Time).Sub(newActivity.(time.Time)); difference > time.Second || difference < -time.Second {
		t.Errorf("last activity: bridge database %v, single copy %v", oldActivity, newActivity)
	}

	oldRow, err := oldBridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	newRow, err := newBridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRow.State != newRow.State || newRow.State != string(msg.SessionToolRunning) {
		t.Errorf("session row state: bridge database %q, single copy %q", oldRow.State, newRow.State)
	}
	if oldRow.Status.AsOf != oldIDs[len(oldIDs)-1] || newRow.Status.AsOf != newIDs[len(newIDs)-1] {
		t.Errorf("session row as_of: bridge database %d (want %d), single copy %d (want %d)",
			oldRow.Status.AsOf, oldIDs[len(oldIDs)-1], newRow.Status.AsOf, newIDs[len(newIDs)-1])
	}

	oldReconciled, err := old.ReconcileSessions(msg.ActiveSessionStates()...)
	if err != nil {
		t.Fatal(err)
	}
	newReconciled, err := single.ReconcileSessions(msg.ActiveSessionStates()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldReconciled) != 1 || len(newReconciled) != 1 || oldReconciled[0].Status.Status.State != newReconciled[0].Status.Status.State {
		t.Fatalf("reconcile: bridge database %+v, single copy %+v", oldReconciled, newReconciled)
	}
	newRow, err = newBridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	newHead, _ := single.MaxEventRowID(sessionID)
	if newRow.State != string(msg.SessionIdle) || newRow.Status.AsOf != newHead || newReconciled[0].Status.Status.AsOf != newHead {
		t.Errorf("after reconcile the row is %q as of %d, the status as of %d; want idle as of the session's newest event %d",
			newRow.State, newRow.Status.AsOf, newReconciled[0].Status.Status.AsOf, newHead)
	}
}

func TestSingleCopyNumbersAboveTheBridgeDatabase(t *testing.T) {
	bridge := newBridgeStore(t)
	if err := bridge.RaiseEventNumberingAbove(5_000_000); err != nil {
		t.Fatal(err)
	}
	createSession(t, bridge, "br_numbering")
	bridgeID, err := bridge.StoreEventReturningID("br_numbering", "system", "", "", []byte(`{"type":"system"}`))
	if err != nil || bridgeID != 5_000_001 {
		t.Fatalf("bridge database id %d, %v; want 5000001", bridgeID, err)
	}
	sessionLog := newSessionLog(t)
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
		t.Fatalf("first single-copy id %d, %v; want 5000002", id, err)
	}
	if !single.StreamCursorFromEarlierNumbering(bridgeID) || single.StreamCursorFromEarlierNumbering(id) || single.StreamCursorFromEarlierNumbering(0) {
		t.Error("a cursor below the first single-copy id is from the bridge database's numbering, and only such a cursor")
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

	// Back on the old path, the bridge database numbers above the session log.
	highest, err := sessionLog.Store().HighestEventID()
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.RaiseEventNumberingAbove(highest); err != nil {
		t.Fatal(err)
	}
	oldPathID, err := bridge.StoreEventReturningID("br_numbering", "system", "", "", []byte(`{"type":"system"}`))
	if err != nil || oldPathID <= highest {
		t.Errorf("old path id %d, %v; want above the session log's %d", oldPathID, err, highest)
	}
}

func TestARunnerEntrySentAgainIsStoredOnce(t *testing.T) {
	bridge := newBridgeStore(t)
	createSession(t, bridge, "br_runner")
	if err := bridge.RecordRunnerSpawn("br_runner", "spawn_made_up", "machine_made_up"); err != nil {
		t.Fatal(err)
	}
	single := newSingleCopy(t, bridge)
	ev := msg.Event{Type: msg.EventToolCall, ToolCall: &msg.ToolCallEvent{ToolID: "tool-1", Name: "Bash"}}
	record := recordOf("br_runner", &ev)

	// The server stopped after storing entry 1's event and before recording
	// the spawn as stored through it.
	event := eventToAppend(record)
	event.IdempotencyKey = RunnerEntryIdempotencyKey("spawn_made_up", 1)
	stored, err := single.sessionLog.AppendEvent(&ev, event)
	if err != nil {
		t.Fatal(err)
	}
	spawn, _ := bridge.RunnerSpawnOfSession("br_runner")
	if spawn.LastSequence != 0 {
		t.Fatalf("spawn stored through %d before the replay", spawn.LastSequence)
	}

	// The runner sends entry 1 again.
	id, err := single.StoreSessionEventAdvancingRunnerSpawn(record, "br_runner", "spawn_made_up", 1)
	if err != nil {
		t.Fatal(err)
	}
	if id != stored.ID {
		t.Errorf("replayed entry stored as %d; want the first write's %d", id, stored.ID)
	}
	events, err := single.ListEventsSinceID("br_runner", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("%d events, %v; want the entry stored once", len(events), err)
	}
	spawn, _ = bridge.RunnerSpawnOfSession("br_runner")
	if spawn.LastSequence != 1 {
		t.Errorf("spawn stored through %d; want 1", spawn.LastSequence)
	}

	// The next entry is stored as a new event.
	next, err := single.StoreSessionEventAdvancingRunnerSpawn(record, "br_runner", "spawn_made_up", 2)
	if err != nil || next <= id {
		t.Errorf("entry 2 stored as %d, %v", next, err)
	}
}

func TestAStatusWhoseRowWriteWasCutOffIsRepairedAtStart(t *testing.T) {
	bridge := newBridgeStore(t)
	createSession(t, bridge, "br_repair")
	single := newSingleCopy(t, bridge)
	status := func(state msg.SessionState) *msg.Event {
		return &msg.Event{Type: msg.EventSessionStatus, BridgeSessionID: "br_repair", Status: &msg.SessionStatus{State: state, Reason: msg.ReasonStartupReconcile}}
	}
	if _, err := single.WriteSessionStatus("br_repair", status(msg.SessionIdle)); err != nil {
		t.Fatal(err)
	}

	// A status event stored with no row write after it, as a stop between
	// the two leaves it.
	cutOff := status(msg.SessionModelGenerating)
	appended, err := single.sessionLog.AppendEvent(cutOff, logstorestore.EventToAppend{
		SessionID: "br_repair", Type: string(msg.EventSessionStatus), IdempotencyKey: "session_status:br_repair:made-up",
		DataForID: func(id int64) ([]byte, error) { cutOff.Status.AsOf = id; return json.Marshal(cutOff) },
	})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := bridge.GetSession("br_repair")
	if row.State != string(msg.SessionIdle) {
		t.Fatalf("row %q before the repair", row.State)
	}
	repaired, err := single.RepairSessionStatusRows()
	if err != nil || repaired != 1 {
		t.Fatalf("repaired %d, %v; want 1", repaired, err)
	}
	row, _ = bridge.GetSession("br_repair")
	if row.State != string(msg.SessionModelGenerating) || row.Status.AsOf != appended.ID {
		t.Errorf("row %q as of %d; want model_generating as of %d", row.State, row.Status.AsOf, appended.ID)
	}
	if repaired, err := single.RepairSessionStatusRows(); err != nil || repaired != 0 {
		t.Errorf("a second repair changed %d rows, %v; want none", repaired, err)
	}

	// An older status never replaces a newer one on the row.
	older := &msg.SessionStatus{State: msg.SessionError, AsOf: appended.ID - 1}
	if changed, err := bridge.WriteSessionStatusRow("br_repair", older, false); err != nil || changed {
		t.Errorf("an older status changed the row: %v, %v", changed, err)
	}
}

func TestAStatusForASessionWithNoRowStoresNothing(t *testing.T) {
	bridge := newBridgeStore(t)
	single := newSingleCopy(t, bridge)
	_, err := single.WriteSessionStatus("br_hook_only", &msg.Event{Type: msg.EventSessionStatus, Status: &msg.SessionStatus{State: msg.SessionIdle}})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err %v; want sql.ErrNoRows", err)
	}
	events, err := single.ListEventsSinceID("br_hook_only", 0)
	if err != nil || len(events) != 0 {
		t.Errorf("%d events stored, %v; want none", len(events), err)
	}
}
