package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/store/storetest"
	"github.com/kayushkin/llm-bridge/msg"
)

// testBridgeDatabases remembers the database each test's bridge store was
// opened on, so enableTestSessionEvents can open the session log beside it:
// a status event and its session row commit together only when the two
// modules share one database.
var testBridgeDatabases sync.Map // *store.Store → *storetest.Database

// testSessionEventStores is the session event store enableTestSessionEvents
// gave each test's server, by its bridge store.
var testSessionEventStores sync.Map // *store.Store → *sessionevents.SingleCopy

// newTestBridgeStore is the bridge's own records in a Postgres database of
// the test's own, holding the session log module too.
func newTestBridgeStore(t testing.TB) *store.Store {
	t.Helper()
	database := storetest.NewDatabase(t)
	st := database.OpenStore()
	testBridgeDatabases.Store(st, database)
	t.Cleanup(func() { testBridgeDatabases.Delete(st) })
	return st
}

// enableTestSessionEvents makes the session log in st's database srv's
// session event log and the only copy of its events, as main does
// (enableSessionLog): its routes, the harness manager's reads and every event
// store. It answers the session log, for a test that reads it directly.
func enableTestSessionEvents(t testing.TB, srv *Server, st *store.Store) *logstoreserver.Server {
	t.Helper()
	held, ok := testBridgeDatabases.Load(st)
	if !ok {
		t.Fatal("enableTestSessionEvents: the store was not opened by newTestBridgeStore")
	}
	database := held.(*storetest.Database)
	sessionLog := database.OpenSessionLog(false)
	srv.EnableSessionLog(sessionLog, sessionLog)
	singleCopy, err := sessionevents.OpenSingleCopy(context.Background(), st, sessionLog)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseSessionEventStore(singleCopy)
	testSessionEventStores.Store(st, singleCopy)
	t.Cleanup(func() { testSessionEventStores.Delete(st) })
	return sessionLog
}

// testSessionEventsOf is where the server built over st stores its events.
func testSessionEventsOf(t testing.TB, st *store.Store) sessionevents.Store {
	t.Helper()
	held, ok := testSessionEventStores.Load(st)
	if !ok {
		t.Fatal("testSessionEventsOf: no server over this store has its session events enabled")
	}
	return held.(*sessionevents.SingleCopy)
}

// storeTestEvent stores one event of sessionID, its body data as written, in
// the session log of the server built over st, with the columns the harness
// manager files an event under, and answers its id.
func storeTestEvent(t testing.TB, st *store.Store, sessionID string, data []byte) int64 {
	t.Helper()
	var event msg.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("storeTestEvent: %v", err)
	}
	record := store.SessionEventRecord{SessionID: sessionID, Type: string(event.Type), MessageID: event.MessageID,
		HarnessMessageID: event.HarnessMessageID, Data: data, Event: &event}
	switch {
	case event.ToolCall != nil:
		record.ToolUseID = event.ToolCall.ToolID
		record.ToolName = event.ToolCall.Name
	case event.ToolResult != nil:
		record.ToolUseID = event.ToolResult.ToolID
	}
	id, err := testSessionEventsOf(t, st).StoreSessionEvent(record)
	if err != nil {
		t.Fatalf("storeTestEvent: %v", err)
	}
	return id
}
