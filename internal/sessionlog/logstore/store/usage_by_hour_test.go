package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func sessionCostEvent(totalUSD float64) []byte {
	body, _ := json.Marshal(map[string]any{
		"type":         "session_cost",
		"session_cost": map[string]any{"total_usd": totalUSD},
	})
	return body
}

func aggregateSince(t *testing.T, s *Store, since time.Time, sessionID string) (SessionAggregateRow, bool) {
	t.Helper()
	aggregates, err := s.ListSessionAggregatesSince(since)
	if err != nil {
		t.Fatalf("ListSessionAggregatesSince: %v", err)
	}
	for _, a := range aggregates {
		if a.SessionID == sessionID {
			return a, true
		}
	}
	return SessionAggregateRow{}, false
}

func TestANewEventIsCountedInTheHourItWasStored(t *testing.T) {
	s := newTestStore(t)
	mustStore(t, s, "sess", "user_message", []byte(`{}`))
	mustStore(t, s, "sess", "api_call", apiCallEvent("sdk", 2, 10, 1000, 50))
	mustStore(t, s, "sess", "api_call", apiCallEvent("agent:custom", 3, 20, 2000, 60))
	mustStore(t, s, "sess", "session_cost", sessionCostEvent(0.25))
	mustStore(t, s, "sess", "result", resultEventWithCache(2, 10, 1000, 50))
	mustStore(t, s, "sess", "session_cost", sessionCostEvent(0.75))
	// Events of other types carry nothing the hourly projections count.
	mustStore(t, s, "sess", "tool_call", []byte(`{}`))

	// A window that began inside this hour includes the whole hour.
	a, found := aggregateSince(t, s, time.Now(), "sess")
	if !found {
		t.Fatal("session missing from a window that holds this hour")
	}
	if a.Turns != 1 || a.InputTokens != 2 || a.OutputTokens != 10 || a.CacheReadTokens != 1000 || a.CacheWriteTokens != 50 {
		t.Errorf("turn-level counts = %+v, want 1 turn and the result event's tokens", a)
	}
	wantUsage := []APICallUsageRow{
		{QuerySource: "agent:custom", Calls: 1, InputTokens: 3, OutputTokens: 20, CacheReadTokens: 2000, CacheWriteTokens: 60},
		{QuerySource: "sdk", Calls: 1, InputTokens: 2, OutputTokens: 10, CacheReadTokens: 1000, CacheWriteTokens: 50},
	}
	if !reflect.DeepEqual(a.APICallUsage, wantUsage) {
		t.Errorf("api call usage =\n%+v\nwant\n%+v", a.APICallUsage, wantUsage)
	}
	if a.SessionCostUSD != 0.75 {
		t.Errorf("cost in the window = %v, want 0.75: the highest total, with none recorded before", a.SessionCostUSD)
	}
	if a.Model != "test-model" {
		t.Errorf("model = %q, want the session's latest", a.Model)
	}

	// A window that begins after this hour holds nothing.
	if _, found := aggregateSince(t, s, time.Now().Add(2*time.Hour), "sess"); found {
		t.Error("session listed in a window that begins after its last event")
	}
}

func TestAWindowLeavesOutSessionsThatUsedNothingInIt(t *testing.T) {
	s := newTestStore(t)
	past := time.Now().UTC().Add(-48 * time.Hour)
	mustStore(t, s, "quiet-since", "api_call", apiCallEvent("sdk", 1, 10, 100, 1))
	mustStore(t, s, "quiet-since", "session_cost", sessionCostEvent(2.00))
	// Move the session's events, and the hours they were counted in, two days
	// back, as if it had last worked then.
	if _, err := s.writer.Exec(`UPDATE events SET created_at = $2 WHERE session_id = $1`, "quiet-since", past); err != nil {
		t.Fatalf("move the events: %v", err)
	}
	for _, table := range []string{"session_usage_by_hour", "session_api_call_usage_by_hour"} {
		if _, err := s.writer.Exec(`UPDATE `+table+` SET hour_start_unix = $2 WHERE session_id = $1`, "quiet-since", hourStartUnixOf(past)); err != nil {
			t.Fatalf("move the hours of %s: %v", table, err)
		}
	}
	// A session that has only taken a message in the window has used nothing.
	mustStore(t, s, "only-a-message", "user_message", []byte(`{}`))

	lastHour := time.Now().Add(-time.Hour)
	aggregates, err := s.ListSessionAggregatesSince(lastHour)
	if err != nil {
		t.Fatalf("ListSessionAggregatesSince: %v", err)
	}
	if len(aggregates) != 0 {
		t.Errorf("window of the last hour lists %s, want no session", fmt.Sprint(aggregates))
	}
	if a, found := aggregateSince(t, s, past, "quiet-since"); !found || a.SessionCostUSD != 2.00 {
		t.Errorf("window holding its events: %+v found=%v, want the session with cost 2.00", a, found)
	}
}
