package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Two per-session token projections were added together on 2026-11-17, after a
// session whose model calls read 2.1 billion cached tokens and whose subagents
// made nine calls in ten showed on the usage page as 192 thousand tokens:
//
//   - sessions.cache_read_tokens and sessions.cache_write_tokens, summed from
//     result events beside the input and output tokens already kept there.
//   - session_api_call_usage, the api_call events summed per session and query
//     source. A result event counts only the main conversation's finished
//     turns; an api_call event is written for every model call the harness
//     reports, and its query source says who made it.
//
// Both are derived from events and can be rebuilt (see
// tokenUsageBackfillName). They are filled from two sides that never count the
// same event: StoreEvent adds each new event as it is stored, and one
// background pass adds the events stored before these projections existed.
// The line between the two is an event id, written down once, in the same
// transaction that creates the projections and so before any new event can be
// stored: the backfill owes every event up to it, StoreEvent every event after.

// tokenUsageBackfillName is the projection_backfills row for the pass that
// adds the events stored before the token usage projections existed.
//
// To rebuild both projections from events: stop the service, DROP TABLE
// session_api_call_usage, set sessions.cache_read_tokens and
// cache_write_tokens to 0, and delete this row. The next start draws a new
// line at the newest event and the background pass fills in everything
// behind it.
const tokenUsageBackfillName = "session_token_usage"

// migrateTokenUsageProjections creates the token usage projections and draws
// the line the backfill works up to. It is quick — no event is read — so it
// can run before the service answers; the reading is BackfillTokenUsage's.
func (s *Store) migrateTokenUsageProjections() error {
	tx, err := s.writer.Begin()
	if err != nil {
		return fmt.Errorf("begin token usage migration: %w", err)
	}
	defer tx.Rollback()

	for _, column := range []string{"cache_read_tokens", "cache_write_tokens"} {
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, column).Scan(&present); err != nil {
			return fmt.Errorf("inspect sessions columns: %w", err)
		}
		if present != 0 {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add sessions.%s: %w", column, err)
		}
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS session_api_call_usage (
			session_id         TEXT    NOT NULL,
			query_source       TEXT    NOT NULL,
			calls              INTEGER NOT NULL DEFAULT 0,
			input_tokens       INTEGER NOT NULL DEFAULT 0,
			output_tokens      INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, query_source)
		);
		CREATE TABLE IF NOT EXISTS projection_backfills (
			name             TEXT PRIMARY KEY,
			through_event_id INTEGER NOT NULL,
			completed_at     DATETIME
		);
	`); err != nil {
		return fmt.Errorf("create token usage tables: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO projection_backfills (name, through_event_id)
		SELECT ?, COALESCE(MAX(id), 0) FROM events
	`, tokenUsageBackfillName); err != nil {
		return fmt.Errorf("draw the token usage backfill line: %w", err)
	}
	return tx.Commit()
}

// tokenUsageBackfillLine reads the event id the backfill works up to, and
// whether the backfill has finished.
func (s *Store) tokenUsageBackfillLine() (throughEventID int64, completed bool, err error) {
	var completedAt sql.NullString
	err = s.writer.QueryRow(
		`SELECT through_event_id, completed_at FROM projection_backfills WHERE name = ?`,
		tokenUsageBackfillName,
	).Scan(&throughEventID, &completedAt)
	if err != nil {
		return 0, false, fmt.Errorf("read the token usage backfill line: %w", err)
	}
	return throughEventID, completedAt.Valid, nil
}

// eventIDTheTokenUsageBackfillStillOwes is the newest event id the pending
// backfill will add to the cache columns, or 0 once it has finished. A
// sessions row rebuilt from events while the backfill is pending counts cache
// tokens only after this id, because the backfill adds the rest on top.
func (s *Store) eventIDTheTokenUsageBackfillStillOwes() (int64, error) {
	throughEventID, completed, err := s.tokenUsageBackfillLine()
	if err != nil {
		return 0, err
	}
	if completed {
		return 0, nil
	}
	return throughEventID, nil
}

// addAPICallToSessionUsage adds one newly stored api_call event to its
// session's total for its query source.
func (s *Store) addAPICallToSessionUsage(sessionID string, data []byte) error {
	var ev struct {
		APICall *msg.APICallEvent `json:"api_call"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("decode api_call event: %w", err)
	}
	if ev.APICall == nil {
		return errors.New("api_call event carries no api_call body")
	}
	_, err := s.writer.Exec(addToAPICallUsageSQL,
		sessionID, ev.APICall.QuerySource, 1,
		ev.APICall.InputTokens, ev.APICall.OutputTokens,
		ev.APICall.CacheReadTokens, ev.APICall.CacheCreationTokens,
	)
	return err
}

// addToAPICallUsageSQL adds to a (session, query source) total, creating it
// at the first call. Both StoreEvent and the backfill write through it, so the
// two sides of the line add up in one row.
const addToAPICallUsageSQL = `
	INSERT INTO session_api_call_usage (
		session_id, query_source, calls,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (session_id, query_source) DO UPDATE SET
		calls              = calls              + excluded.calls,
		input_tokens       = input_tokens       + excluded.input_tokens,
		output_tokens      = output_tokens      + excluded.output_tokens,
		cache_read_tokens  = cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens`

// APICallUsageRow is one session's api_call events of one query source,
// summed. QuerySource is the event's own value, empty when it named none.
type APICallUsageRow struct {
	QuerySource      string
	Calls            int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// apiCallUsageBySession reads every session's API call totals, each session's
// rows ordered by query source.
func (s *Store) apiCallUsageBySession() (map[string][]APICallUsageRow, error) {
	rows, err := s.reader.Query(`
		SELECT session_id, query_source, calls,
		       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		FROM session_api_call_usage
		ORDER BY session_id, query_source
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bySession := make(map[string][]APICallUsageRow)
	for rows.Next() {
		var sessionID string
		var r APICallUsageRow
		if err := rows.Scan(&sessionID, &r.QuerySource, &r.Calls,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens); err != nil {
			return nil, err
		}
		bySession[sessionID] = append(bySession[sessionID], r)
	}
	return bySession, rows.Err()
}

// TokenUsageBackfillReport says what one BackfillTokenUsage call did.
type TokenUsageBackfillReport struct {
	// AlreadyComplete is true when an earlier run finished the backfill and
	// this call read nothing.
	AlreadyComplete bool
	// ThroughEventID is the newest event id the backfill covers.
	ThroughEventID int64
	// APICallTotals is how many (session, query source) totals it added to,
	// SessionsWithCacheTokens how many sessions rows.
	APICallTotals           int
	SessionsWithCacheTokens int
	Took                    time.Duration
}

// BackfillTokenUsage adds the events stored before the token usage
// projections existed: every api_call and result event up to the line
// migrateTokenUsageProjections drew. It does its work once; a later call
// reports AlreadyComplete.
//
// It runs after the service is answering, never inside New. Reading the
// events took 64 seconds on this store's largest database (4.9 million
// events, 20 GB, 2026-11-17), and llm-bridge-server drops an event it has
// failed to push here for 30 seconds — so a start that waited on this would
// lose events.
//
// The reads go through the reader pool and hold no lock. The sums are then
// written in one transaction together with the mark that says the backfill
// is done, so a run that dies part-way has added nothing and the next start
// begins again from the same line.
func (s *Store) BackfillTokenUsage() (TokenUsageBackfillReport, error) {
	started := time.Now()
	throughEventID, completed, err := s.tokenUsageBackfillLine()
	if err != nil {
		return TokenUsageBackfillReport{}, err
	}
	report := TokenUsageBackfillReport{ThroughEventID: throughEventID, AlreadyComplete: completed}
	if completed {
		return report, nil
	}

	type apiCallTotal struct {
		sessionID string
		usage     APICallUsageRow
	}
	var apiCallTotals []apiCallTotal
	// INDEXED BY, because without it SQLite reads the whole events table —
	// every tool payload in it — to find the two event types wanted here.
	// The index is walked instead and only matching rows are fetched.
	apiCallRows, err := s.reader.Query(`
		SELECT session_id,
		       COALESCE(json_extract(data, '$.api_call.query_source'), ''),
		       COUNT(*),
		       COALESCE(SUM(json_extract(data, '$.api_call.input_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.output_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.cache_read_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.cache_creation_tokens')), 0)
		FROM events INDEXED BY idx_events_session_type
		WHERE type = 'api_call' AND id <= ?
		  AND json_type(data, '$.api_call') = 'object'
		GROUP BY 1, 2
	`, throughEventID)
	if err != nil {
		return report, fmt.Errorf("read api_call events: %w", err)
	}
	for apiCallRows.Next() {
		var t apiCallTotal
		if err := apiCallRows.Scan(&t.sessionID, &t.usage.QuerySource, &t.usage.Calls,
			&t.usage.InputTokens, &t.usage.OutputTokens,
			&t.usage.CacheReadTokens, &t.usage.CacheWriteTokens); err != nil {
			apiCallRows.Close()
			return report, fmt.Errorf("scan api_call totals: %w", err)
		}
		apiCallTotals = append(apiCallTotals, t)
	}
	if err := apiCallRows.Close(); err != nil {
		return report, fmt.Errorf("read api_call events: %w", err)
	}
	if err := apiCallRows.Err(); err != nil {
		return report, fmt.Errorf("read api_call events: %w", err)
	}

	type cacheTotal struct {
		sessionID        string
		cacheReadTokens  int64
		cacheWriteTokens int64
	}
	var cacheTotals []cacheTotal
	resultRows, err := s.reader.Query(`
		SELECT session_id,
		       COALESCE(SUM(json_extract(data, '$.result.usage.cache_read_tokens')), 0)  AS cache_read_tokens,
		       COALESCE(SUM(json_extract(data, '$.result.usage.cache_write_tokens')), 0) AS cache_write_tokens
		FROM events INDEXED BY idx_events_session_type
		WHERE type = 'result' AND id <= ?
		GROUP BY 1
		HAVING cache_read_tokens != 0 OR cache_write_tokens != 0
	`, throughEventID)
	if err != nil {
		return report, fmt.Errorf("read result events: %w", err)
	}
	for resultRows.Next() {
		var t cacheTotal
		if err := resultRows.Scan(&t.sessionID, &t.cacheReadTokens, &t.cacheWriteTokens); err != nil {
			resultRows.Close()
			return report, fmt.Errorf("scan result cache totals: %w", err)
		}
		cacheTotals = append(cacheTotals, t)
	}
	if err := resultRows.Close(); err != nil {
		return report, fmt.Errorf("read result events: %w", err)
	}
	if err := resultRows.Err(); err != nil {
		return report, fmt.Errorf("read result events: %w", err)
	}

	tx, err := s.writer.Begin()
	if err != nil {
		return report, fmt.Errorf("begin token usage backfill write: %w", err)
	}
	defer tx.Rollback()
	addAPICallTotal, err := tx.Prepare(addToAPICallUsageSQL)
	if err != nil {
		return report, fmt.Errorf("prepare api call total write: %w", err)
	}
	defer addAPICallTotal.Close()
	for _, t := range apiCallTotals {
		if _, err := addAPICallTotal.Exec(t.sessionID, t.usage.QuerySource, t.usage.Calls,
			t.usage.InputTokens, t.usage.OutputTokens,
			t.usage.CacheReadTokens, t.usage.CacheWriteTokens); err != nil {
			return report, fmt.Errorf("add api call total for %s: %w", t.sessionID, err)
		}
	}
	// UPDATE only. A session whose sessions row is missing is rebuilt whole
	// from its events at the next start, by which time this backfill is marked
	// complete and that rebuild counts every event; seeding a row here would
	// stop that rebuild and leave the row's other columns empty.
	addCacheTotal, err := tx.Prepare(`
		UPDATE sessions SET
			cache_read_tokens  = cache_read_tokens  + ?,
			cache_write_tokens = cache_write_tokens + ?
		WHERE session_id = ?`)
	if err != nil {
		return report, fmt.Errorf("prepare cache total write: %w", err)
	}
	defer addCacheTotal.Close()
	for _, t := range cacheTotals {
		if _, err := addCacheTotal.Exec(t.cacheReadTokens, t.cacheWriteTokens, t.sessionID); err != nil {
			return report, fmt.Errorf("add cache total for %s: %w", t.sessionID, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE projection_backfills SET completed_at = CURRENT_TIMESTAMP WHERE name = ?`,
		tokenUsageBackfillName,
	); err != nil {
		return report, fmt.Errorf("mark the token usage backfill complete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit token usage backfill: %w", err)
	}
	report.APICallTotals = len(apiCallTotals)
	report.SessionsWithCacheTokens = len(cacheTotals)
	report.Took = time.Since(started)
	log.Printf("[log-store] token usage backfill done through event %d: %d api call totals, %d sessions with cache tokens, %s",
		throughEventID, report.APICallTotals, report.SessionsWithCacheTokens, report.Took.Round(time.Millisecond))
	return report, nil
}
