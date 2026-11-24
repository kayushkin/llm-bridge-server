package store

import (
	"encoding/json"
	"path/filepath"
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
	if _, err := s.StoreEvent(sessionID, eventType, data); err != nil {
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
	id, err := s.StoreEvent("sess", "api_call", []byte(`{"type":"api_call"}`))
	if err != nil || id == 0 {
		t.Fatalf("StoreEvent = %d, %v; the event itself must still be stored", id, err)
	}
	if _, found := aggregateOf(t, s, "sess"); found {
		t.Error("an api_call event with no body produced usage")
	}
}

// databaseFromBeforeTheTokenUsageProjections writes events through the current
// code, then takes the database back to the shape it had before the token
// usage projections: no cache columns, no API call table, no backfill line.
// Reopening it runs the migration as the first start of the new binary does.
func databaseFromBeforeTheTokenUsageProjections(t *testing.T, storeEvents func(s *Store)) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "log-store.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	storeEvents(s)
	for _, statement := range []string{
		`DROP TABLE session_api_call_usage`,
		`DROP TABLE projection_backfills`,
		`ALTER TABLE sessions DROP COLUMN cache_read_tokens`,
		`ALTER TABLE sessions DROP COLUMN cache_write_tokens`,
	} {
		if _, err := s.writer.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dbPath
}

func reopen(t *testing.T, dbPath string) *Store {
	t.Helper()
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTokenUsageBackfillAddsTheEventsStoredBeforeTheProjections(t *testing.T) {
	dbPath := databaseFromBeforeTheTokenUsageProjections(t, func(s *Store) {
		mustStore(t, s, "old", "result", resultEventWithCache(1, 10, 1000, 100))
		mustStore(t, s, "old", "api_call", apiCallEvent("sdk", 1, 10, 1000, 100))
		mustStore(t, s, "old", "api_call", apiCallEvent("agent:custom", 2, 20, 2000, 200))
		mustStore(t, s, "old", "api_call", []byte(`{"type":"api_call"}`))
		mustStore(t, s, "no-cache", "result", resultEvent(5, 5, 0, 1, "m"))
	})
	s := reopen(t, dbPath)

	// Before the backfill, the old events are not in the new projections, and
	// what was already projected is untouched.
	a, found := aggregateOf(t, s, "old")
	if !found {
		t.Fatal("session missing after the migration")
	}
	if a.InputTokens != 1 || a.OutputTokens != 10 {
		t.Errorf("input/output after migration = %d/%d, want 1/10", a.InputTokens, a.OutputTokens)
	}
	if a.CacheReadTokens != 0 || len(a.APICallUsage) != 0 {
		t.Errorf("before the backfill: cache read %d, api call usage %+v; want none", a.CacheReadTokens, a.APICallUsage)
	}

	// Events stored after the migration and before the backfill finishes are
	// added by StoreEvent, and the backfill must not add them again.
	mustStore(t, s, "old", "result", resultEventWithCache(1, 10, 5000, 500))
	mustStore(t, s, "old", "api_call", apiCallEvent("sdk", 1, 10, 5000, 500))

	report, err := s.BackfillTokenUsage()
	if err != nil {
		t.Fatalf("BackfillTokenUsage: %v", err)
	}
	if report.AlreadyComplete || report.APICallTotals != 2 || report.SessionsWithCacheTokens != 1 {
		t.Errorf("report = %+v, want a first run with 2 api call totals and 1 session with cache tokens", report)
	}

	wantUsage := []APICallUsageRow{
		{QuerySource: "agent:custom", Calls: 1, InputTokens: 2, OutputTokens: 20, CacheReadTokens: 2000, CacheWriteTokens: 200},
		{QuerySource: "sdk", Calls: 2, InputTokens: 2, OutputTokens: 20, CacheReadTokens: 6000, CacheWriteTokens: 600},
	}
	check := func(when string) {
		t.Helper()
		a, _ := aggregateOf(t, s, "old")
		if a.CacheReadTokens != 6000 || a.CacheWriteTokens != 600 {
			t.Errorf("%s: cache read/write = %d/%d, want 6000/600", when, a.CacheReadTokens, a.CacheWriteTokens)
		}
		if a.InputTokens != 2 || a.OutputTokens != 20 {
			t.Errorf("%s: input/output = %d/%d, want 2/20", when, a.InputTokens, a.OutputTokens)
		}
		if !reflect.DeepEqual(a.APICallUsage, wantUsage) {
			t.Errorf("%s: api call usage =\n%+v\nwant\n%+v", when, a.APICallUsage, wantUsage)
		}
	}
	check("after the backfill")

	// A second run, and a restart, add nothing.
	report, err = s.BackfillTokenUsage()
	if err != nil {
		t.Fatalf("second BackfillTokenUsage: %v", err)
	}
	if !report.AlreadyComplete {
		t.Errorf("second run report = %+v, want AlreadyComplete", report)
	}
	check("after a second backfill run")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = reopen(t, dbPath)
	if _, err := s.BackfillTokenUsage(); err != nil {
		t.Fatalf("BackfillTokenUsage after restart: %v", err)
	}
	check("after a restart")
}

// A sessions row that is missing at start is rebuilt from the session's
// events. While the backfill is pending that rebuild and the backfill both
// read the old result events, and the cache tokens must be counted once.
func TestASessionsRowRebuiltBeforeTheBackfillCountsCacheTokensOnce(t *testing.T) {
	dbPath := databaseFromBeforeTheTokenUsageProjections(t, func(s *Store) {
		mustStore(t, s, "lost-row", "result", resultEventWithCache(1, 10, 1000, 100))
		mustStore(t, s, "lost-row", "result", resultEventWithCache(1, 10, 2000, 200))
		if _, err := s.writer.Exec(`DELETE FROM sessions WHERE session_id = 'lost-row'`); err != nil {
			t.Fatalf("delete sessions row: %v", err)
		}
	})
	s := reopen(t, dbPath)
	if _, err := s.BackfillTokenUsage(); err != nil {
		t.Fatalf("BackfillTokenUsage: %v", err)
	}
	a, found := aggregateOf(t, s, "lost-row")
	if !found {
		t.Fatal("session missing after the rebuild")
	}
	if a.CacheReadTokens != 3000 || a.CacheWriteTokens != 300 {
		t.Errorf("cache read/write = %d/%d, want 3000/300", a.CacheReadTokens, a.CacheWriteTokens)
	}

	// Once the backfill is complete, a row lost later is rebuilt with every
	// result event's cache tokens, because nothing else will add them.
	if _, err := s.writer.Exec(`DELETE FROM sessions WHERE session_id = 'lost-row'`); err != nil {
		t.Fatalf("delete sessions row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = reopen(t, dbPath)
	a, found = aggregateOf(t, s, "lost-row")
	if !found {
		t.Fatal("session missing after the second rebuild")
	}
	if a.CacheReadTokens != 3000 || a.CacheWriteTokens != 300 || a.InputTokens != 2 || a.OutputTokens != 20 {
		t.Errorf("rebuilt after the backfill = %+v, want cache 3000/300 and input/output 2/20", a)
	}
}

func TestAnEmptyDatabaseHasNothingToBackfill(t *testing.T) {
	s := newTestStore(t)
	report, err := s.BackfillTokenUsage()
	if err != nil {
		t.Fatalf("BackfillTokenUsage: %v", err)
	}
	if report.AlreadyComplete || report.ThroughEventID != 0 || report.APICallTotals != 0 {
		t.Errorf("report = %+v, want a first run through event 0 that added nothing", report)
	}
	// Events stored afterwards are counted once, by StoreEvent.
	mustStore(t, s, "sess", "api_call", apiCallEvent("sdk", 1, 2, 3, 4))
	a, _ := aggregateOf(t, s, "sess")
	want := []APICallUsageRow{{QuerySource: "sdk", Calls: 1, InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}}
	if !reflect.DeepEqual(a.APICallUsage, want) {
		t.Errorf("api call usage = %+v, want %+v", a.APICallUsage, want)
	}
}
