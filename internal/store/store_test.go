package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// testStore creates a temporary store for testing.
func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ──────────────────────────────────────────────────────────────────────────────
// Session CRUD
// ──────────────────────────────────────────────────────────────────────────────

func TestCreateAndGetSession(t *testing.T) {
	s := testStore(t)

	sess := &Session{
		BridgeID:    "br_100",
		ClientID:    "fe_1",
		DisplayName: "Test Session",
		Harness:     "claude_code",
		State:       "idle",
		AgentID:     "agent_1",
		SpawnerID:   "spawner_1",
	}

	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetSession("br_100")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.BridgeID != "br_100" {
		t.Errorf("bridge_id = %q, want br_100", got.BridgeID)
	}
	if got.ClientID != "fe_1" {
		t.Errorf("client_id = %q, want fe_1", got.ClientID)
	}
	if got.DisplayName != "Test Session" {
		t.Errorf("display_name = %q, want Test Session", got.DisplayName)
	}
	if got.Harness != "claude_code" {
		t.Errorf("harness = %q, want claude_code", got.Harness)
	}
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.GetSession("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestListSessions(t *testing.T) {
	s := testStore(t)

	for i, name := range []string{"first", "second", "third"} {
		sess := &Session{
			BridgeID:    "br_" + name,
			ClientID:    "fe_" + name,
			DisplayName: name,
			Harness:     "claude_code",
			State:       "idle",
		}
		// Stagger creation times so ordering is deterministic
		time.Sleep(time.Duration(i) * time.Millisecond)
		if err := s.CreateSession(sess); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	sessions, err := s.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}
	// Should be ordered by created_at DESC
	if sessions[0].DisplayName != "third" {
		t.Errorf("first result = %q, want third (DESC order)", sessions[0].DisplayName)
	}
}

func TestListSessionsByState(t *testing.T) {
	s := testStore(t)

	for _, tc := range []struct {
		id    string
		state string
	}{
		{"br_1", "idle"},
		{"br_2", "running"},
		{"br_3", "idle"},
		{"br_4", "completed"},
	} {
		sess := &Session{
			BridgeID: tc.id,
			ClientID: "fe_x",
			Harness:  "mock",
			State:    tc.state,
		}
		if err := s.CreateSession(sess); err != nil {
			t.Fatalf("create %s: %v", tc.id, err)
		}
	}

	idle, err := s.ListSessionsByState("idle")
	if err != nil {
		t.Fatalf("list idle: %v", err)
	}
	if len(idle) != 2 {
		t.Errorf("idle count = %d, want 2", len(idle))
	}

	running, err := s.ListSessionsByState("running")
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 1 {
		t.Errorf("running count = %d, want 1", len(running))
	}
}

func TestDeleteSession(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_del", ClientID: "fe_x", Harness: "mock", State: "idle"}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.DeleteSession("br_del"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := s.GetSession("br_del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	s := testStore(t)
	err := s.DeleteSession("nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent session")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Session state transitions
// ──────────────────────────────────────────────────────────────────────────────

func TestUpdateSessionState(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_state", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	if err := s.UpdateSessionState("br_state", "running"); err != nil {
		t.Fatalf("update state: %v", err)
	}

	got, _ := s.GetSession("br_state")
	if got.State != "running" {
		t.Errorf("state = %q, want running", got.State)
	}
}

func TestUpdateSessionState_NotFound(t *testing.T) {
	s := testStore(t)
	err := s.UpdateSessionState("nonexistent", "running")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestUpdateSessionPID(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_pid", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	if err := s.UpdateSessionPID("br_pid", 12345); err != nil {
		t.Fatalf("update pid: %v", err)
	}

	got, _ := s.GetSession("br_pid")
	if got.PID != 12345 {
		t.Errorf("pid = %d, want 12345", got.PID)
	}
}

func TestSetHarnessSessionID(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_hid", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	if err := s.SetHarnessSessionID("br_hid", "cc-uuid-abc123"); err != nil {
		t.Fatalf("set harness session id: %v", err)
	}

	got, _ := s.GetSession("br_hid")
	if got.HarnessSessionID != "cc-uuid-abc123" {
		t.Errorf("harness_session_id = %q, want cc-uuid-abc123", got.HarnessSessionID)
	}

	// Also verify GetSessionByHarnessSessionID
	got2, err := s.GetSessionByHarnessSessionID("cc-uuid-abc123")
	if err != nil {
		t.Fatalf("get by harness session id: %v", err)
	}
	if got2.BridgeID != "br_hid" {
		t.Errorf("bridge_id from harness_session_id lookup = %q, want br_hid", got2.BridgeID)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Harness config persistence
// ──────────────────────────────────────────────────────────────────────────────

func TestHarnessConfig(t *testing.T) {
	s := testStore(t)

	cfg := json.RawMessage(`{"system_prompt":"you are a test","model":"opus"}`)
	sess := &Session{
		BridgeID:      "br_cfg",
		ClientID:      "fe_x",
		Harness:       "mock",
		State:         "idle",
		HarnessConfig: cfg,
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, _ := s.GetSession("br_cfg")
	if string(got.HarnessConfig) != string(cfg) {
		t.Errorf("harness_config = %s, want %s", got.HarnessConfig, cfg)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Events
// ──────────────────────────────────────────────────────────────────────────────

func TestStoreAndListEvents(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_ev", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	event1 := msg.Event{Type: msg.EventStream, BridgeSessionID: "br_ev", Timestamp: time.Now()}
	event2 := msg.Event{Type: msg.EventResult, BridgeSessionID: "br_ev", Timestamp: time.Now()}

	data1, _ := json.Marshal(event1)
	data2, _ := json.Marshal(event2)

	if err := s.StoreEvent("br_ev", string(event1.Type), "", "", data1); err != nil {
		t.Fatalf("store event 1: %v", err)
	}
	if err := s.StoreEvent("br_ev", string(event2.Type), "", "", data2); err != nil {
		t.Fatalf("store event 2: %v", err)
	}

	// ListEventsSinceID should return events after the given ID
	events, err := s.ListEventsSinceID("br_ev", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("event count = %d, want 2", len(events))
	}

	// List since first event should return only the second
	events, err = s.ListEventsSinceID("br_ev", events[0].RowID)
	if err != nil {
		t.Fatalf("list events since: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("event count after first = %d, want 1", len(events))
	}
}

func TestStoreEventReturningID(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_eid", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	data, _ := json.Marshal(msg.Event{Type: msg.EventStream})
	id, err := s.StoreEventReturningID("br_eid", "stream", "", "", data)
	if err != nil {
		t.Fatalf("store event returning id: %v", err)
	}
	if id <= 0 {
		t.Errorf("returned id = %d, want > 0", id)
	}
}

func TestListCurrentTurnEvents(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_turn", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	// Store events: stream, result, user_message, stream, result
	types := []string{"stream", "result", "user_message", "stream", "result"}
	for _, typ := range types {
		data, _ := json.Marshal(msg.Event{Type: msg.EventType(typ)})
		s.StoreEvent("br_turn", typ, "", "", data)
	}

	// Current turn should only return events after the last user_message
	events, err := s.ListCurrentTurnEventsWithIDs("br_turn")
	if err != nil {
		t.Fatalf("list current turn: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("current turn event count = %d, want 2", len(events))
	}
}

func TestRecoverInFlightTurn(t *testing.T) {
	s := testStore(t)

	sess := &Session{BridgeID: "br_recov", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	// No user_message yet — nothing to recover.
	got, err := s.RecoverInFlightTurn("br_recov")
	if err != nil {
		t.Fatalf("recover empty: %v", err)
	}
	if got != nil {
		t.Errorf("recover empty = %+v, want nil", got)
	}

	storeEv := func(typ msg.EventType, ev msg.Event) {
		t.Helper()
		ev.Type = typ
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := s.StoreEventReturningID("br_recov", string(typ), ev.MessageID, ev.HarnessMessageID, data); err != nil {
			t.Fatalf("store: %v", err)
		}
	}

	// Closed turn: user_message → block → result. Nothing in flight.
	storeEv(msg.EventUserMessage, msg.Event{TurnID: "turn_a", ClientRequestID: "req_a", MessageID: "msg_user_a"})
	storeEv(msg.EventBlock, msg.Event{TurnID: "turn_a", MessageID: "msg_assist_a", HarnessMessageID: "h_a"})
	storeEv(msg.EventResult, msg.Event{TurnID: "turn_a", MessageID: "msg_assist_a", HarnessMessageID: "h_a"})

	got, err = s.RecoverInFlightTurn("br_recov")
	if err != nil {
		t.Fatalf("recover after closed: %v", err)
	}
	if got != nil {
		t.Errorf("recover after closed = %+v, want nil", got)
	}

	// Open a new turn that the harness restart interrupts before any
	// result/error lands. Recovery must surface the in-flight state.
	storeEv(msg.EventUserMessage, msg.Event{TurnID: "turn_b", ClientRequestID: "req_b", MessageID: "msg_user_b"})
	storeEv(msg.EventBlock, msg.Event{TurnID: "turn_b", MessageID: "msg_assist_b1", HarnessMessageID: "h_b1"})
	storeEv(msg.EventToolCall, msg.Event{TurnID: "turn_b", MessageID: "msg_assist_b2", HarnessMessageID: "h_b2"})

	got, err = s.RecoverInFlightTurn("br_recov")
	if err != nil {
		t.Fatalf("recover in-flight: %v", err)
	}
	if got == nil {
		t.Fatalf("recover in-flight = nil, want state")
	}
	if got.TurnID != "turn_b" {
		t.Errorf("TurnID = %q, want turn_b", got.TurnID)
	}
	if got.ClientRequestID != "req_b" {
		t.Errorf("ClientRequestID = %q, want req_b", got.ClientRequestID)
	}
	if got.BridgeMessageID != "msg_assist_b2" {
		t.Errorf("BridgeMessageID = %q, want msg_assist_b2 (most recent bubble)", got.BridgeMessageID)
	}
	if got.HarnessMessageID != "h_b2" {
		t.Errorf("HarnessMessageID = %q, want h_b2", got.HarnessMessageID)
	}

	// Closing the turn with a result clears the recovery state.
	storeEv(msg.EventResult, msg.Event{TurnID: "turn_b", MessageID: "msg_assist_b2", HarnessMessageID: "h_b2"})

	got, err = s.RecoverInFlightTurn("br_recov")
	if err != nil {
		t.Fatalf("recover after terminator: %v", err)
	}
	if got != nil {
		t.Errorf("recover after terminator = %+v, want nil", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovered sessions
// ──────────────────────────────────────────────────────────────────────────────

func TestUpsertDiscoveredSession(t *testing.T) {
	s := testStore(t)

	now := time.Now()

	// First insert
	bridgeID, inserted, err := s.UpsertDiscoveredSession("cc-uuid-1", "Test Task", "claude_code", "inst_1", "", "", now, now)
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if !inserted {
		t.Error("expected inserted=true for new session")
	}
	if bridgeID == "" {
		t.Error("expected bridge_id to be returned for new session")
	}

	// Second upsert (same harness_session_id) should not insert and should return the existing bridge_id
	bridgeID2, inserted, err := s.UpsertDiscoveredSession("cc-uuid-1", "Updated Name", "claude_code", "", "", "", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if inserted {
		t.Error("expected inserted=false for existing session")
	}
	if bridgeID2 != bridgeID {
		t.Errorf("existing bridge_id = %q, want %q (should be stable)", bridgeID2, bridgeID)
	}

	// Verify the session exists and has instance_id preserved
	sess, err := s.GetSessionByHarnessSessionID("cc-uuid-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.InstanceID != "inst_1" {
		t.Errorf("instance_id = %q, want inst_1 (should be preserved)", sess.InstanceID)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Concurrent access
// ──────────────────────────────────────────────────────────────────────────────

func TestConcurrentWrites(t *testing.T) {
	s := testStore(t)

	// Create a session for event writes
	sess := &Session{BridgeID: "br_conc", ClientID: "fe_x", Harness: "mock", State: "idle"}
	s.CreateSession(sess)

	// Sequential writes (the production code serializes via Manager.mu)
	for i := range 20 {
		data, _ := json.Marshal(msg.Event{Type: msg.EventStream, BridgeSessionID: "br_conc"})
		if err := s.StoreEvent("br_conc", "stream", "", "", data); err != nil {
			t.Errorf("write %d: %v", i, err)
		}
	}

	// Verify all 20 events stored via StoreEventReturningID
	lastID, err := s.StoreEventReturningID("br_conc", "stream", "", "", []byte(`{}`))
	if err != nil {
		t.Fatalf("store returning id: %v", err)
	}
	if lastID < 21 {
		t.Errorf("last insert id = %d, want >= 21", lastID)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DB creation in nonexistent directory
// ──────────────────────────────────────────────────────────────────────────────

func TestNewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "path")
	dbPath := filepath.Join(dir, "test.db")

	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("expected directory to be created")
	}
}
