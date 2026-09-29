package store

import (
	"fmt"
	"testing"
	"time"
)

// The event shapes below are the ones Claude Code 2.1.283 produced in
// br_1790697291165681122: the call carries tool_call.tool_id and the input,
// and the stream's tool_result carries raw.tool_use_result.scheduledFor in
// epoch milliseconds (0 after a stop). The OTel copy of the result has a
// name but no tool_id and no raw, and must not be mistaken for it.
func storeWakeupCall(t *testing.T, s *Store, sessionID, toolID, input string) {
	t.Helper()
	data := fmt.Sprintf(`{"type":"tool_call","tool_call":{"tool_id":%q,"name":"ScheduleWakeup","input":%s}}`, toolID, input)
	if err := s.StoreEvent(sessionID, "tool_call", "", "", []byte(data)); err != nil {
		t.Fatalf("store tool_call: %v", err)
	}
}

func storeWakeupResult(t *testing.T, s *Store, sessionID, toolID, toolUseResult string) {
	t.Helper()
	data := fmt.Sprintf(`{"type":"tool_result","tool_result":{"tool_id":%q,"name":"","output":""},"raw":{"type":"user","tool_use_result":%s}}`, toolID, toolUseResult)
	if err := s.StoreEvent(sessionID, "tool_result", "", "", []byte(data)); err != nil {
		t.Fatalf("store tool_result: %v", err)
	}
	otel := `{"type":"tool_result","tool_result":{"tool_id":"","name":"ScheduleWakeup","output":""},"extensions":{"source":"otel"}}`
	if err := s.StoreEvent(sessionID, "tool_result", "", "", []byte(otel)); err != nil {
		t.Fatalf("store otel tool_result: %v", err)
	}
}

func TestPendingWakeupDueAt(t *testing.T) {
	processStartedAt := time.Now().Add(-time.Minute)
	const dueMilliseconds = 1790697420000

	t.Run("no wakeup call", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil || !due.IsZero() {
			t.Fatalf("got %v, %v; want zero time", due, err)
		}
	})

	t.Run("scheduled wakeup", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_w", "toolu_a", `{"delaySeconds":60}`)
		storeWakeupResult(t, s, "br_w", "toolu_a", fmt.Sprintf(`{"scheduledFor":%d,"clampedDelaySeconds":60}`, dueMilliseconds))
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil {
			t.Fatal(err)
		}
		if want := time.UnixMilli(dueMilliseconds).UTC(); !due.Equal(want) {
			t.Fatalf("due = %v, want %v", due, want)
		}
	})

	t.Run("a later call replaces the earlier one", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_w", "toolu_a", `{"delaySeconds":900}`)
		storeWakeupResult(t, s, "br_w", "toolu_a", `{"scheduledFor":1790698440000}`)
		storeWakeupCall(t, s, "br_w", "toolu_b", `{"delaySeconds":300}`)
		storeWakeupResult(t, s, "br_w", "toolu_b", fmt.Sprintf(`{"scheduledFor":%d}`, dueMilliseconds))
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil {
			t.Fatal(err)
		}
		if want := time.UnixMilli(dueMilliseconds).UTC(); !due.Equal(want) {
			t.Fatalf("due = %v, want %v", due, want)
		}
	})

	t.Run("a stop cancels", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_w", "toolu_a", `{"delaySeconds":600}`)
		storeWakeupResult(t, s, "br_w", "toolu_a", `{"scheduledFor":1790698140000}`)
		storeWakeupCall(t, s, "br_w", "toolu_b", `{"stop":true}`)
		storeWakeupResult(t, s, "br_w", "toolu_b", `{"scheduledFor":0,"stopped":true,"cancelledWakeups":1}`)
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil || !due.IsZero() {
			t.Fatalf("got %v, %v; want zero time", due, err)
		}
	})

	t.Run("a call with no result yet", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_w", "toolu_a", `{"delaySeconds":600}`)
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil || !due.IsZero() {
			t.Fatalf("got %v, %v; want zero time", due, err)
		}
	})

	t.Run("a wakeup set by an earlier process", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_w", "toolu_a", `{"delaySeconds":600}`)
		storeWakeupResult(t, s, "br_w", "toolu_a", fmt.Sprintf(`{"scheduledFor":%d}`, dueMilliseconds))
		due, err := s.PendingWakeupDueAt("br_w", time.Now().Add(time.Minute))
		if err != nil || !due.IsZero() {
			t.Fatalf("got %v, %v; want zero time", due, err)
		}
	})

	t.Run("another session's wakeup", func(t *testing.T) {
		s := testStore(t)
		s.CreateSession(&Session{SessionID: "br_w", Harness: "claude_code", State: "idle"})
		s.CreateSession(&Session{SessionID: "br_other", Harness: "claude_code", State: "idle"})
		storeWakeupCall(t, s, "br_other", "toolu_a", `{"delaySeconds":600}`)
		storeWakeupResult(t, s, "br_other", "toolu_a", fmt.Sprintf(`{"scheduledFor":%d}`, dueMilliseconds))
		due, err := s.PendingWakeupDueAt("br_w", processStartedAt)
		if err != nil || !due.IsZero() {
			t.Fatalf("got %v, %v; want zero time", due, err)
		}
	})
}
