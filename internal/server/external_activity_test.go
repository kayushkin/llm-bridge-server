package server

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// The starting mark decides which transcript entries the watcher treats as a
// terminal run. Too early and the dash shows the bridge's own turns twice; too
// late and the terminal turn never appears.

func TestExternalActivityStartingMark(t *testing.T) {
	srv, st := testServer(t)
	watcherStarted := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)

	if err := st.CreateSession(&store.Session{SessionID: "br_none", Harness: "claude_code", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.externalActivityStartingMark("br_none", watcherStarted); err != nil || !got.Equal(watcherStarted) {
		t.Fatalf("no mark and no events: got %s (%v), want when the watcher started", got, err)
	}

	if err := st.CreateSession(&store.Session{SessionID: "br_events", Harness: "claude_code", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StoreEventReturningID("br_events", "result", "", "", []byte(`{"type":"result"}`)); err != nil {
		t.Fatal(err)
	}
	last, err := st.LastActivityAt("br_events")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := srv.externalActivityStartingMark("br_events", watcherStarted); err != nil || !got.Equal(last.Add(time.Second)) {
		t.Fatalf("events but no mark: got %s (%v), want the last event's second rounded up, %s", got, err, last.Add(time.Second))
	}

	mark := time.Date(2026, 10, 20, 15, 30, 0, 123, time.UTC)
	if err := st.AdvanceExternalActivityMark("br_events", mark); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.externalActivityStartingMark("br_events", watcherStarted); err != nil || !got.Equal(mark) {
		t.Fatalf("with a mark: got %s (%v), want the mark %s", got, err, mark)
	}
}
