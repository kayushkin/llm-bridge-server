package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// storeScheduledWakeup logs a ScheduleWakeup call and its stream result in
// the shapes Claude Code 2.1.283 produced (see store's scheduled wakeup tests).
func storeScheduledWakeup(t *testing.T, st *store.Store, sessionID, toolID string, dueAt time.Time) {
	t.Helper()
	call := fmt.Sprintf(`{"type":"tool_call","tool_call":{"tool_id":%q,"name":"ScheduleWakeup","input":{"delaySeconds":1200,"prompt":"check the backfill"}}}`, toolID)
	result := fmt.Sprintf(`{"type":"tool_result","tool_result":{"tool_id":%q},"raw":{"tool_use_result":{"scheduledFor":%d}}}`, toolID, dueAt.UnixMilli())
	if err := st.StoreEvent(sessionID, "tool_call", "", "", []byte(call)); err != nil {
		t.Fatal(err)
	}
	if err := st.StoreEvent(sessionID, "tool_result", "", "", []byte(result)); err != nil {
		t.Fatal(err)
	}
}

func TestLostWakeup(t *testing.T) {
	oldestCallToRead := time.Now().Add(-3 * time.Hour)

	t.Run("an idle session's wakeup that has not fired is lost", func(t *testing.T) {
		srv, st := testServer(t)
		st.CreateSession(&store.Session{SessionID: "br_w", Harness: "claude_code", State: string(msg.SessionIdle)})
		storeScheduledWakeup(t, st, "br_w", "toolu_a", time.Now().Add(10*time.Minute))
		wakeup, err := srv.lostWakeup("br_w", oldestCallToRead)
		if err != nil || wakeup == nil || wakeup.ToolID != "toolu_a" || wakeup.Prompt != "check the backfill" {
			t.Fatalf("got %+v, %v; want toolu_a", wakeup, err)
		}
	})

	t.Run("a wakeup followed by a user message after it came due fired", func(t *testing.T) {
		srv, st := testServer(t)
		st.CreateSession(&store.Session{SessionID: "br_w", Harness: "claude_code", State: string(msg.SessionIdle)})
		storeScheduledWakeup(t, st, "br_w", "toolu_a", time.Now().Add(-time.Minute))
		if err := st.StoreEvent("br_w", "user_message", "", "", []byte(`{"type":"user_message"}`)); err != nil {
			t.Fatal(err)
		}
		wakeup, err := srv.lostWakeup("br_w", oldestCallToRead)
		if err != nil || wakeup != nil {
			t.Fatalf("got %+v, %v; want none", wakeup, err)
		}
	})

	t.Run("an aborted session is left alone", func(t *testing.T) {
		srv, st := testServer(t)
		st.CreateSession(&store.Session{SessionID: "br_w", Harness: "claude_code", State: string(msg.SessionAborted)})
		storeScheduledWakeup(t, st, "br_w", "toolu_a", time.Now().Add(10*time.Minute))
		wakeup, err := srv.lostWakeup("br_w", oldestCallToRead)
		if err != nil || wakeup != nil {
			t.Fatalf("got %+v, %v; want none", wakeup, err)
		}
	})

	t.Run("a session with no wakeup", func(t *testing.T) {
		srv, st := testServer(t)
		st.CreateSession(&store.Session{SessionID: "br_w", Harness: "claude_code", State: string(msg.SessionIdle)})
		wakeup, err := srv.lostWakeup("br_w", oldestCallToRead)
		if err != nil || wakeup != nil {
			t.Fatalf("got %+v, %v; want none", wakeup, err)
		}
	})
}
