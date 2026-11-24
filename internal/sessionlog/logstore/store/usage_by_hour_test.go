package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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

// storeMovedTo stores an event and then moves it to the given time, as the
// backfill will find it. The hourly bucket StoreEvent already wrote for it is
// in the current hour; tests that use storeMovedTo drop the hourly projections
// afterwards and let the backfill place the event.
func storeMovedTo(t *testing.T, s *Store, at time.Time, sessionID, eventType string, data []byte) {
	t.Helper()
	id, err := s.StoreEvent(sessionID, eventType, data)
	if err != nil {
		t.Fatalf("StoreEvent %s/%s: %v", sessionID, eventType, err)
	}
	if _, err := s.writer.Exec(`UPDATE events SET created_at = ? WHERE id = ?`,
		at.UTC().Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("move event %d to %s: %v", id, at, err)
	}
}

// databaseFromBeforeTheHourlyProjections writes events through the current
// code, then takes the database back to the shape it had before the hourly
// projections existed. Reopening it runs the migration as the first start of
// the new binary does.
func databaseFromBeforeTheHourlyProjections(t *testing.T, storeEvents func(s *Store)) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "log-store.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	storeEvents(s)
	for _, statement := range []string{
		`DROP TABLE session_usage_by_hour`,
		`DROP TABLE session_api_call_usage_by_hour`,
		`DELETE FROM projection_backfills WHERE name = 'session_usage_by_hour'`,
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

func TestUsageByHourBackfillPlacesOldEventsInTheirOwnHours(t *testing.T) {
	// Far enough back that no hour here is the hour the test runs in.
	day := time.Now().UTC().Add(-72 * time.Hour).Truncate(24 * time.Hour)
	at := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	dbPath := databaseFromBeforeTheHourlyProjections(t, func(s *Store) {
		// 10:00 hour.
		storeMovedTo(t, s, at(10, 15), "old", "user_message", []byte(`{}`))
		storeMovedTo(t, s, at(10, 16), "old", "api_call", apiCallEvent("sdk", 1, 10, 100, 1))
		storeMovedTo(t, s, at(10, 17), "old", "session_cost", sessionCostEvent(1.00))
		storeMovedTo(t, s, at(10, 45), "old", "api_call", apiCallEvent("agent:custom", 2, 20, 200, 2))
		storeMovedTo(t, s, at(10, 46), "old", "session_cost", sessionCostEvent(1.50))
		storeMovedTo(t, s, at(10, 50), "old", "result", resultEventWithCache(1, 10, 100, 1))
		// 13:00 hour.
		storeMovedTo(t, s, at(13, 5), "old", "user_message", []byte(`{}`))
		storeMovedTo(t, s, at(13, 6), "old", "api_call", apiCallEvent("sdk", 4, 40, 400, 4))
		storeMovedTo(t, s, at(13, 7), "old", "session_cost", sessionCostEvent(4.00))
		storeMovedTo(t, s, at(13, 8), "old", "result", resultEventWithCache(4, 40, 400, 4))
		storeMovedTo(t, s, at(13, 9), "old", "api_call", []byte(`{"type":"api_call"}`))
	})
	s := reopen(t, dbPath)

	// An event stored after the migration is StoreEvent's to add, in the
	// current hour, and the backfill must not add it again.
	mustStore(t, s, "old", "api_call", apiCallEvent("sdk", 7, 70, 700, 7))
	mustStore(t, s, "old", "session_cost", sessionCostEvent(9.00))

	if _, found := aggregateSince(t, s, at(10, 0), "old"); !found {
		t.Fatal("the event stored after the migration is missing before the backfill")
	}

	report, err := s.BackfillUsageByHour()
	if err != nil {
		t.Fatalf("BackfillUsageByHour: %v", err)
	}
	if report.AlreadyComplete || report.SessionHours != 2 || report.APICallSessionHours != 3 {
		t.Errorf("report = %+v, want a first run with 2 session hours and 3 api call session hours", report)
	}

	check := func(when string) {
		t.Helper()
		// From 10:00 on: everything.
		all, _ := aggregateSince(t, s, at(10, 30), "old")
		if all.Turns != 2 || all.InputTokens != 5 || all.OutputTokens != 50 || all.CacheReadTokens != 500 || all.CacheWriteTokens != 5 {
			t.Errorf("%s, from 10:00: turn-level counts = %+v, want 2 turns and both results", when, all)
		}
		wantAll := []APICallUsageRow{
			{QuerySource: "agent:custom", Calls: 1, InputTokens: 2, OutputTokens: 20, CacheReadTokens: 200, CacheWriteTokens: 2},
			{QuerySource: "sdk", Calls: 3, InputTokens: 12, OutputTokens: 120, CacheReadTokens: 1200, CacheWriteTokens: 12},
		}
		if !reflect.DeepEqual(all.APICallUsage, wantAll) {
			t.Errorf("%s, from 10:00: api call usage =\n%+v\nwant\n%+v", when, all.APICallUsage, wantAll)
		}
		if all.SessionCostUSD != 9.00 {
			t.Errorf("%s, from 10:00: cost = %v, want 9.00", when, all.SessionCostUSD)
		}

		// From 11:00 on: the 13:00 hour and the new event. The cost is what was
		// added after the 10:00 hour's last total of 1.50.
		later, _ := aggregateSince(t, s, at(11, 0), "old")
		if later.Turns != 1 || later.OutputTokens != 40 || later.CacheReadTokens != 400 {
			t.Errorf("%s, from 11:00: turn-level counts = %+v, want the 13:00 turn only", when, later)
		}
		wantLater := []APICallUsageRow{
			{QuerySource: "sdk", Calls: 2, InputTokens: 11, OutputTokens: 110, CacheReadTokens: 1100, CacheWriteTokens: 11},
		}
		if !reflect.DeepEqual(later.APICallUsage, wantLater) {
			t.Errorf("%s, from 11:00: api call usage =\n%+v\nwant\n%+v", when, later.APICallUsage, wantLater)
		}
		if later.SessionCostUSD != 7.50 {
			t.Errorf("%s, from 11:00: cost = %v, want 7.50 (9.00 less the 1.50 recorded before)", when, later.SessionCostUSD)
		}

		// From 14:00 on: only the event stored after the migration.
		newest, _ := aggregateSince(t, s, at(14, 0), "old")
		wantNewest := []APICallUsageRow{
			{QuerySource: "sdk", Calls: 1, InputTokens: 7, OutputTokens: 70, CacheReadTokens: 700, CacheWriteTokens: 7},
		}
		if !reflect.DeepEqual(newest.APICallUsage, wantNewest) || newest.Turns != 0 || newest.OutputTokens != 0 {
			t.Errorf("%s, from 14:00: %+v, want only the new api call", when, newest)
		}
		if newest.SessionCostUSD != 5.00 {
			t.Errorf("%s, from 14:00: cost = %v, want 5.00 (9.00 less the 4.00 recorded before)", when, newest.SessionCostUSD)
		}
	}
	check("after the backfill")

	report, err = s.BackfillUsageByHour()
	if err != nil {
		t.Fatalf("second BackfillUsageByHour: %v", err)
	}
	if !report.AlreadyComplete {
		t.Errorf("second run report = %+v, want AlreadyComplete", report)
	}
	check("after a second backfill run")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = reopen(t, dbPath)
	if _, err := s.BackfillUsageByHour(); err != nil {
		t.Fatalf("BackfillUsageByHour after restart: %v", err)
	}
	check("after a restart")
}

// The backfill writes a few sessions per transaction. A run that dies between
// two of them must be taken up where it stopped: the sessions it wrote are
// not added again, the ones it did not reach are.
func TestAnInterruptedUsageByHourBackfillResumesWithoutAddingTwice(t *testing.T) {
	past := time.Now().UTC().Add(-48 * time.Hour)
	sessions := []string{"sess-a", "sess-b", "sess-c", "sess-d"}
	dbPath := databaseFromBeforeTheHourlyProjections(t, func(s *Store) {
		for i, sessionID := range sessions {
			storeMovedTo(t, s, past, sessionID, "api_call", apiCallEvent("sdk", 1, 10*(i+1), 100, 1))
			storeMovedTo(t, s, past, sessionID, "result", resultEventWithCache(1, int64(10*(i+1)), 100, 1))
		}
	})
	s := reopen(t, dbPath)

	// Fail the write of the third session, one session per transaction: the
	// first two are committed, the third and fourth are not.
	if _, err := s.writer.Exec(`
		CREATE TRIGGER stop_at_sess_c BEFORE INSERT ON session_usage_by_hour
		WHEN NEW.session_id = 'sess-c'
		BEGIN SELECT RAISE(ABORT, 'the run dies here'); END
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := s.backfillUsageByHour(1); err == nil {
		t.Fatal("the interrupted run reported no error")
	}
	if _, found := aggregateSince(t, s, past, "sess-b"); !found {
		t.Fatal("sess-b was not written before the run died")
	}
	if _, found := aggregateSince(t, s, past, "sess-c"); found {
		t.Fatal("sess-c was written by the run that died on it")
	}
	if _, err := s.writer.Exec(`DROP TRIGGER stop_at_sess_c`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	report, err := s.backfillUsageByHour(1)
	if err != nil {
		t.Fatalf("resumed backfill: %v", err)
	}
	if report.SessionHours != 2 || report.APICallSessionHours != 2 {
		t.Errorf("resumed run report = %+v, want the two sessions the first run did not reach", report)
	}
	for i, sessionID := range sessions {
		a, found := aggregateSince(t, s, past, sessionID)
		if !found {
			t.Errorf("%s missing after the resumed run", sessionID)
			continue
		}
		wantOutput := int64(10 * (i + 1))
		if a.OutputTokens != wantOutput || len(a.APICallUsage) != 1 || a.APICallUsage[0].Calls != 1 || a.APICallUsage[0].OutputTokens != wantOutput {
			t.Errorf("%s = %+v, want output %d and one api call, each counted once", sessionID, a, wantOutput)
		}
	}
	if report, err := s.backfillUsageByHour(1); err != nil || !report.AlreadyComplete {
		t.Errorf("after the resumed run: report %+v, err %v; want AlreadyComplete", report, err)
	}
}

func TestAWindowLeavesOutSessionsThatUsedNothingInIt(t *testing.T) {
	past := time.Now().UTC().Add(-48 * time.Hour)
	dbPath := databaseFromBeforeTheHourlyProjections(t, func(s *Store) {
		storeMovedTo(t, s, past, "quiet-since", "api_call", apiCallEvent("sdk", 1, 10, 100, 1))
		storeMovedTo(t, s, past, "quiet-since", "session_cost", sessionCostEvent(2.00))
	})
	s := reopen(t, dbPath)
	// A session that has only taken a message in the window has used nothing.
	mustStore(t, s, "only-a-message", "user_message", []byte(`{}`))
	if _, err := s.BackfillUsageByHour(); err != nil {
		t.Fatalf("BackfillUsageByHour: %v", err)
	}

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
