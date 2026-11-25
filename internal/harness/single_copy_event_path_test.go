package harness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

// With the session log as the only copy, every event the stream carries is
// numbered by the session log, the bridge database's events table is never
// written, and the session row's status is as of a session log event.
func TestSingleCopyStreamsTheSessionLogsIDs(t *testing.T) {
	bridge, err := store.New(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bridge.Close() })
	module, err := logstorestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	reader, err := logstorestore.OpenReader(context.Background(), database.URL(), module)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	sessionLog := logstoreserver.New(logstorestore.New(database.Open(module), reader), nil)
	singleCopy, err := sessionevents.OpenSingleCopy(context.Background(), bridge, sessionLog)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(bridge, "http://127.0.0.1:0", "", 0, nil)
	m.SetSessionEventLog(sessionLog)
	m.SetSessionEventStore(singleCopy, true)

	const sessionID = "br_single_copy"
	if err := bridge.CreateSession(&store.Session{SessionID: sessionID, Harness: msg.HarnessClaudeCode, State: string(msg.SessionIdle)}); err != nil {
		t.Fatal(err)
	}
	subscriber := m.Subscribe(sessionID)
	process := &fakeProcess{sid: sessionID, ch: make(chan msg.Event, 8)}
	go m.readEvents(process)

	user := msg.Event{Type: msg.EventUserMessage, BridgeSessionID: sessionID, TurnID: "turn-1", Result: &msg.ResultEvent{Text: "made-up request"}}
	userID, err := m.BroadcastEvent(&user)
	if err != nil {
		t.Fatal(err)
	}
	process.ch <- msg.Event{Type: msg.EventBlock, Harness: msg.HarnessClaudeCode, TurnID: "turn-1", HarnessMessageID: "harness-1",
		Block: &msg.BlockEvent{Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: "made-up answer"}}}}
	process.ch <- msg.Event{Type: msg.EventResult, Harness: msg.HarnessClaudeCode, TurnID: "turn-1", Result: &msg.ResultEvent{Text: "made-up answer"}}

	received := map[int64]msg.EventType{}
	deadline := time.After(30 * time.Second)
	for sawResult := false; !sawResult; {
		select {
		case stored := <-subscriber:
			received[stored.RowID] = stored.Type
			sawResult = stored.Type == msg.EventResult
		case <-deadline:
			t.Fatalf("no result on the stream; had %v", received)
		}
	}
	close(process.ch)

	stored, err := singleCopy.ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	storedIDs := map[int64]bool{}
	for _, event := range stored {
		storedIDs[int64(event.RowID)] = true
	}
	if !storedIDs[userID] {
		t.Errorf("BroadcastEvent answered %d, which the session log does not hold", userID)
	}
	for id, eventType := range received {
		if id == 0 || !storedIDs[id] {
			t.Errorf("the stream carried %s as id %d, which the session log does not hold (it holds %v)", eventType, id, storedIDs)
		}
		if id < singleCopy.Numbering().FirstEventID {
			t.Errorf("id %d is below the single copy's first id %d", id, singleCopy.Numbering().FirstEventID)
		}
	}
	if head, err := bridge.MaxEventRowID(sessionID); err != nil || head != 0 {
		t.Errorf("the bridge database's events table holds events up to %d (%v); want none", head, err)
	}
	row, err := bridge.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status == nil || !storedIDs[row.Status.AsOf] {
		t.Errorf("the session row's status %+v is not as of a session log event", row.Status)
	}
}
