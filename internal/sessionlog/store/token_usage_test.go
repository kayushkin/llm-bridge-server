package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// resultEventWithCache builds a result event whose usage carries cache tokens.
func resultEventWithCache(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) []byte {
	body, _ := json.Marshal(map[string]any{
		"type": "result",
		"result": map[string]any{
			"usage": map[string]any{
				"input_tokens":       inputTokens,
				"output_tokens":      outputTokens,
				"cache_read_tokens":  cacheReadTokens,
				"cache_write_tokens": cacheWriteTokens,
			},
			"model": "test-model",
		},
	})
	return body
}

// apiCallEvent builds an api_call event. An empty querySource leaves the field
// out, as a harness that names none does.
func apiCallEvent(querySource string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) []byte {
	apiCall := map[string]any{
		"model":                 "test-model",
		"input_tokens":          inputTokens,
		"output_tokens":         outputTokens,
		"cache_read_tokens":     cacheReadTokens,
		"cache_creation_tokens": cacheCreationTokens,
	}
	if querySource != "" {
		apiCall["query_source"] = querySource
	}
	body, _ := json.Marshal(map[string]any{"type": "api_call", "api_call": apiCall})
	return body
}

func mustStore(t *testing.T, s *Store, sessionID, eventType string, data []byte) {
	t.Helper()
	if _, err := s.StoreEvent(context.Background(), sessionID, eventType, data); err != nil {
		t.Fatalf("StoreEvent %s/%s: %v", sessionID, eventType, err)
	}
}

func aggregateOf(t *testing.T, s *Store, sessionID string) (SessionAggregateRow, bool) {
	t.Helper()
	aggregates, err := s.ListSessionAggregates()
	if err != nil {
		t.Fatalf("ListSessionAggregates: %v", err)
	}
	for _, a := range aggregates {
		if a.SessionID == sessionID {
			return a, true
		}
	}
	return SessionAggregateRow{}, false
}

func TestResultCacheTokensAreSummedIntoTheAggregate(t *testing.T) {
	s := newTestStore(t)
	mustStore(t, s, "sess", "result", resultEventWithCache(4, 100, 700000, 500))
	mustStore(t, s, "sess", "result", resultEventWithCache(2, 30, 800000, 0))

	a, found := aggregateOf(t, s, "sess")
	if !found {
		t.Fatal("session missing from the aggregates")
	}
	if a.InputTokens != 6 || a.OutputTokens != 130 {
		t.Errorf("input/output = %d/%d, want 6/130", a.InputTokens, a.OutputTokens)
	}
	if a.CacheReadTokens != 1500000 || a.CacheWriteTokens != 500 {
		t.Errorf("cache read/write = %d/%d, want 1500000/500", a.CacheReadTokens, a.CacheWriteTokens)
	}
}

func TestAPICallsAreSummedPerQuerySource(t *testing.T) {
	s := newTestStore(t)
	mustStore(t, s, "sess", "api_call", apiCallEvent("sdk", 2, 10, 1000, 50))
	mustStore(t, s, "sess", "api_call", apiCallEvent("sdk", 3, 20, 2000, 0))
	mustStore(t, s, "sess", "api_call", apiCallEvent("agent:builtin:general-purpose", 4, 300, 500000, 7000))
	mustStore(t, s, "sess", "api_call", apiCallEvent("", 1, 1, 1, 1))
	mustStore(t, s, "other", "api_call", apiCallEvent("sdk", 9, 9, 9, 9))

	// A session with api_call events and no result event has usage to report.
	a, found := aggregateOf(t, s, "sess")
	if !found {
		t.Fatal("a session with only api_call events is missing from the aggregates")
	}
	want := []APICallUsageRow{
		{QuerySource: "", Calls: 1, InputTokens: 1, OutputTokens: 1, CacheReadTokens: 1, CacheWriteTokens: 1},
		{QuerySource: "agent:builtin:general-purpose", Calls: 1, InputTokens: 4, OutputTokens: 300, CacheReadTokens: 500000, CacheWriteTokens: 7000},
		{QuerySource: "sdk", Calls: 2, InputTokens: 5, OutputTokens: 30, CacheReadTokens: 3000, CacheWriteTokens: 50},
	}
	if !reflect.DeepEqual(a.APICallUsage, want) {
		t.Errorf("api call usage =\n%+v\nwant\n%+v", a.APICallUsage, want)
	}
	// The result-event columns stay the result events' own: api_call events
	// are not folded into them.
	if a.InputTokens != 0 || a.OutputTokens != 0 || a.CacheReadTokens != 0 {
		t.Errorf("result-event totals = %d/%d/%d, want all 0 with no result event", a.InputTokens, a.OutputTokens, a.CacheReadTokens)
	}
}

func TestAnAPICallEventWithNoBodyIsStoredAndNotCounted(t *testing.T) {
	s := newTestStore(t)
	id, err := s.StoreEvent(context.Background(), "sess", "api_call", []byte(`{"type":"api_call"}`))
	if err != nil || id == 0 {
		t.Fatalf("StoreEvent = %d, %v; the event itself must still be stored", id, err)
	}
	if _, found := aggregateOf(t, s, "sess"); found {
		t.Error("an api_call event with no body produced usage")
	}
}
