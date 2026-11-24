package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The sessions row and session_api_call_usage hold a session's totals since
// it began, which cannot answer "what was used in the last day": a session
// started on Monday that makes five thousand model calls on Tuesday shows
// Tuesday's work only as Monday's session. These two projections hold the
// same counts per session and clock hour, so a window of time can be summed:
//
//   - session_usage_by_hour: per session and hour, the user messages (turns),
//     the result events' tokens and duration, and the highest session_cost
//     total seen in the hour.
//   - session_api_call_usage_by_hour: per session, hour and query source, the
//     api_call events' calls and tokens.
//
// An hour, not a day: a rolling "last 24 hours" is then right to within the
// hour, and no time zone has to decide where a day begins.
//
// An event belongs to the hour of its own events.created_at, the time this
// store wrote it down. Both sides read that one column — StoreEvent for a new
// event, the backfill for an old one — so a rebuild from events lands every
// event in the bucket it is in now.
//
// They are filled like the whole-life token projections (see token_usage.go):
// StoreEvent adds each new event, and one background pass adds the events
// stored before, up to an event id written down when the tables are created.

// usageByHourBackfillName is the projection_backfills row for the pass that
// adds the events stored before the hourly projections existed.
//
// To rebuild them from events: stop the service, DROP TABLE
// session_usage_by_hour and session_api_call_usage_by_hour, and delete this
// row. The next start draws a new line and the background pass fills in
// everything behind it.
const usageByHourBackfillName = "session_usage_by_hour"

// hourStartUnixSQL turns events.created_at into the unix time its clock hour
// began. created_at is UTC text, which strftime reads as UTC.
const hourStartUnixSQL = `(CAST(strftime('%s', created_at) AS INTEGER) / 3600) * 3600`

// hourStartUnixOf is the unix time the clock hour holding t began.
func hourStartUnixOf(t time.Time) int64 {
	return t.Unix() / 3600 * 3600
}

// migrateUsageByHourProjections creates the hourly projections and draws the
// line their backfill works up to. No event is read here.
func (s *Store) migrateUsageByHourProjections() error {
	tx, err := s.writer.Begin()
	if err != nil {
		return fmt.Errorf("begin hourly usage migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS session_usage_by_hour (
			session_id             TEXT    NOT NULL,
			hour_start_unix        INTEGER NOT NULL,
			turn_count             INTEGER NOT NULL DEFAULT 0,
			input_tokens           INTEGER NOT NULL DEFAULT 0,
			output_tokens          INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens      INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens     INTEGER NOT NULL DEFAULT 0,
			duration_ms            INTEGER NOT NULL DEFAULT 0,
			session_cost_total_usd REAL,
			PRIMARY KEY (session_id, hour_start_unix)
		);
		CREATE INDEX IF NOT EXISTS idx_session_usage_by_hour_hour
			ON session_usage_by_hour(hour_start_unix);
		CREATE TABLE IF NOT EXISTS session_api_call_usage_by_hour (
			session_id         TEXT    NOT NULL,
			hour_start_unix    INTEGER NOT NULL,
			query_source       TEXT    NOT NULL,
			calls              INTEGER NOT NULL DEFAULT 0,
			input_tokens       INTEGER NOT NULL DEFAULT 0,
			output_tokens      INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, hour_start_unix, query_source)
		);
		CREATE INDEX IF NOT EXISTS idx_session_api_call_usage_by_hour_hour
			ON session_api_call_usage_by_hour(hour_start_unix);
	`); err != nil {
		return fmt.Errorf("create hourly usage tables: %w", err)
	}
	if err := drawBackfillLine(tx, usageByHourBackfillName); err != nil {
		return err
	}
	return tx.Commit()
}

// hourlyUsage is what one session did in one hour, less its api calls: the
// counts session_usage_by_hour holds. SessionCostTotalUSD is the highest
// session_cost total seen, invalid when the hour held no session_cost event.
type hourlyUsage struct {
	TurnCount           int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
	DurationMS          int64
	SessionCostTotalUSD sql.NullFloat64
}

// addToUsageByHourSQL adds to a session's hour, creating it at the first
// event. Counts add up; the session_cost total is a running total already, so
// the highest one seen is kept. StoreEvent and the backfill both write
// through it.
const addToUsageByHourSQL = `
	INSERT INTO session_usage_by_hour (
		session_id, hour_start_unix, turn_count,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		duration_ms, session_cost_total_usd
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (session_id, hour_start_unix) DO UPDATE SET
		turn_count         = turn_count         + excluded.turn_count,
		input_tokens       = input_tokens       + excluded.input_tokens,
		output_tokens      = output_tokens      + excluded.output_tokens,
		cache_read_tokens  = cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
		duration_ms        = duration_ms        + excluded.duration_ms,
		session_cost_total_usd = CASE
			WHEN excluded.session_cost_total_usd IS NULL THEN session_cost_total_usd
			WHEN session_cost_total_usd IS NULL THEN excluded.session_cost_total_usd
			WHEN excluded.session_cost_total_usd > session_cost_total_usd THEN excluded.session_cost_total_usd
			ELSE session_cost_total_usd
		END`

func usageByHourArguments(sessionID string, hourStartUnix int64, u hourlyUsage) []any {
	return []any{sessionID, hourStartUnix, u.TurnCount,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens,
		u.DurationMS, u.SessionCostTotalUSD}
}

// addToAPICallUsageByHourSQL adds to a (session, hour, query source) total.
const addToAPICallUsageByHourSQL = `
	INSERT INTO session_api_call_usage_by_hour (
		session_id, hour_start_unix, query_source, calls,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (session_id, hour_start_unix, query_source) DO UPDATE SET
		calls              = calls              + excluded.calls,
		input_tokens       = input_tokens       + excluded.input_tokens,
		output_tokens      = output_tokens      + excluded.output_tokens,
		cache_read_tokens  = cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens`

func apiCallUsageByHourArguments(sessionID string, hourStartUnix int64, u APICallUsageRow) []any {
	return []any{sessionID, hourStartUnix, u.QuerySource, u.Calls,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}
}

// addEventToUsageByHour adds one newly stored event to its session's hour.
// Only four event types carry anything the hourly projections count; every
// other type returns at once.
func (s *Store) addEventToUsageByHour(eventID int64, sessionID, eventType string, data []byte) error {
	switch eventType {
	case "user_message", "result", "api_call", "session_cost":
	default:
		return nil
	}
	var hourStartUnix int64
	if err := s.writer.QueryRow(
		`SELECT `+hourStartUnixSQL+` FROM events WHERE id = ?`, eventID,
	).Scan(&hourStartUnix); err != nil {
		return fmt.Errorf("read the hour of event %d: %w", eventID, err)
	}

	switch eventType {
	case "user_message":
		_, err := s.writer.Exec(addToUsageByHourSQL,
			usageByHourArguments(sessionID, hourStartUnix, hourlyUsage{TurnCount: 1})...)
		return err
	case "result":
		var ev struct {
			Result struct {
				Usage struct {
					InputTokens      int64 `json:"input_tokens"`
					OutputTokens     int64 `json:"output_tokens"`
					CacheReadTokens  int64 `json:"cache_read_tokens"`
					CacheWriteTokens int64 `json:"cache_write_tokens"`
				} `json:"usage"`
				DurationMS int64 `json:"duration_ms"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode result event: %w", err)
		}
		_, err := s.writer.Exec(addToUsageByHourSQL,
			usageByHourArguments(sessionID, hourStartUnix, hourlyUsage{
				InputTokens:      ev.Result.Usage.InputTokens,
				OutputTokens:     ev.Result.Usage.OutputTokens,
				CacheReadTokens:  ev.Result.Usage.CacheReadTokens,
				CacheWriteTokens: ev.Result.Usage.CacheWriteTokens,
				DurationMS:       ev.Result.DurationMS,
			})...)
		return err
	case "session_cost":
		var ev struct {
			SessionCost *msg.SessionCostEvent `json:"session_cost"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode session_cost event: %w", err)
		}
		if ev.SessionCost == nil {
			return errors.New("session_cost event carries no session_cost body")
		}
		_, err := s.writer.Exec(addToUsageByHourSQL,
			usageByHourArguments(sessionID, hourStartUnix, hourlyUsage{
				SessionCostTotalUSD: sql.NullFloat64{Float64: ev.SessionCost.TotalUSD, Valid: true},
			})...)
		return err
	default: // api_call
		var ev struct {
			APICall *msg.APICallEvent `json:"api_call"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("decode api_call event: %w", err)
		}
		if ev.APICall == nil {
			return errors.New("api_call event carries no api_call body")
		}
		_, err := s.writer.Exec(addToAPICallUsageByHourSQL,
			apiCallUsageByHourArguments(sessionID, hourStartUnix, APICallUsageRow{
				QuerySource:      ev.APICall.QuerySource,
				Calls:            1,
				InputTokens:      int64(ev.APICall.InputTokens),
				OutputTokens:     int64(ev.APICall.OutputTokens),
				CacheReadTokens:  int64(ev.APICall.CacheReadTokens),
				CacheWriteTokens: int64(ev.APICall.CacheCreationTokens),
			})...)
		return err
	}
}

// ListSessionAggregatesSince returns, for every session that used anything
// from the hour holding `since` on, what it did from then on: its turns, its
// result events' tokens and duration, its api calls per query source, and how
// much its session_cost total grew.
//
// The cost growth is the highest total seen in the window less the highest
// seen before it. A session's first session_cost event already carries
// everything it had cost by then, so a session whose cost was first recorded
// inside the window is counted from zero.
func (s *Store) ListSessionAggregatesSince(since time.Time) ([]SessionAggregateRow, error) {
	windowStartUnix := hourStartUnixOf(since)

	apiCallUsageBySession := make(map[string][]APICallUsageRow)
	apiCallRows, err := s.reader.Query(`
		SELECT session_id, query_source, SUM(calls),
		       SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens)
		FROM session_api_call_usage_by_hour
		WHERE hour_start_unix >= ?
		GROUP BY session_id, query_source
		ORDER BY session_id, query_source
	`, windowStartUnix)
	if err != nil {
		return nil, err
	}
	defer apiCallRows.Close()
	for apiCallRows.Next() {
		var sessionID string
		var r APICallUsageRow
		if err := apiCallRows.Scan(&sessionID, &r.QuerySource, &r.Calls,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens); err != nil {
			return nil, err
		}
		apiCallUsageBySession[sessionID] = append(apiCallUsageBySession[sessionID], r)
	}
	if err := apiCallRows.Err(); err != nil {
		return nil, err
	}

	rows, err := s.reader.Query(`
		WITH active AS (
			SELECT session_id FROM session_usage_by_hour WHERE hour_start_unix >= ?1
			UNION
			SELECT session_id FROM session_api_call_usage_by_hour WHERE hour_start_unix >= ?1
		)
		SELECT a.session_id,
		       COALESCE((SELECT model FROM sessions s WHERE s.session_id = a.session_id), ''),
		       COALESCE(SUM(u.turn_count), 0),
		       COALESCE(SUM(u.input_tokens), 0),
		       COALESCE(SUM(u.output_tokens), 0),
		       COALESCE(SUM(u.cache_read_tokens), 0),
		       COALESCE(SUM(u.cache_write_tokens), 0),
		       COALESCE(SUM(u.duration_ms), 0),
		       MAX(u.session_cost_total_usd),
		       (SELECT MAX(b.session_cost_total_usd) FROM session_usage_by_hour b
		         WHERE b.session_id = a.session_id AND b.hour_start_unix < ?1)
		FROM active a
		LEFT JOIN session_usage_by_hour u
		       ON u.session_id = a.session_id AND u.hour_start_unix >= ?1
		GROUP BY a.session_id
		ORDER BY a.session_id
	`, windowStartUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionAggregateRow
	for rows.Next() {
		var r SessionAggregateRow
		var costAtWindowEnd, costBeforeWindow sql.NullFloat64
		if err := rows.Scan(&r.SessionID, &r.Model, &r.Turns,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.DurationMS, &costAtWindowEnd, &costBeforeWindow); err != nil {
			return nil, err
		}
		if costAtWindowEnd.Valid {
			// costBeforeWindow.Float64 is 0 when no total was recorded before.
			r.SessionCostUSD = costAtWindowEnd.Float64 - costBeforeWindow.Float64
		}
		r.APICallUsage = apiCallUsageBySession[r.SessionID]
		// As on the whole-life read, a session with no usage is left out: here
		// one that only took a message in the window and has spent nothing yet.
		if r.InputTokens == 0 && r.OutputTokens == 0 && r.CacheReadTokens == 0 &&
			r.CacheWriteTokens == 0 && len(r.APICallUsage) == 0 && r.SessionCostUSD == 0 {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByHourBackfillReport says what one BackfillUsageByHour call did.
type UsageByHourBackfillReport struct {
	AlreadyComplete bool
	ThroughEventID  int64
	// SessionHours and APICallSessionHours are how many rows of each hourly
	// projection this call added to. A call that resumes an interrupted one
	// reports only its own part.
	SessionHours        int
	APICallSessionHours int
	Took                time.Duration
}

// usageByHourBackfillSessionsPerWrite is how many sessions' rows go into one
// write transaction. The writer is one connection and every StoreEvent waits
// for it, so the backfill writes in short transactions, not one long one: the
// whole backfill is about 200,000 rows on this store's largest database.
const usageByHourBackfillSessionsPerWrite = 200

// BackfillUsageByHour adds the events stored before the hourly projections
// existed: every user_message, result, session_cost and api_call event up to
// the line migrateUsageByHourProjections drew. It does its work once; a later
// call reports AlreadyComplete.
//
// Like BackfillTokenUsage it runs beside the service, never inside New: the
// read takes minutes on a large database and llm-bridge-server drops an event
// it has failed to push here for 30 seconds.
//
// The reads go through the reader pool and hold no lock. The sums are written
// session by session in short transactions, each of which also records the
// last session it wrote, so a run that dies part-way is taken up after that
// session by the next start and no row is added twice. The events up to the
// line never change, so the second read sums what the first did.
func (s *Store) BackfillUsageByHour() (UsageByHourBackfillReport, error) {
	return s.backfillUsageByHour(usageByHourBackfillSessionsPerWrite)
}

func (s *Store) backfillUsageByHour(sessionsPerWrite int) (UsageByHourBackfillReport, error) {
	started := time.Now()
	line, err := s.backfillLine(usageByHourBackfillName)
	if err != nil {
		return UsageByHourBackfillReport{}, err
	}
	report := UsageByHourBackfillReport{ThroughEventID: line.throughEventID, AlreadyComplete: line.completed}
	if line.completed {
		return report, nil
	}

	type sessionHour struct {
		hourStartUnix int64
		usage         hourlyUsage
	}
	type apiCallSessionHour struct {
		hourStartUnix int64
		usage         APICallUsageRow
	}
	type sessionRows struct {
		hours        []sessionHour
		apiCallHours []apiCallSessionHour
	}
	bySession := make(map[string]*sessionRows)
	rowsOf := func(sessionID string) *sessionRows {
		r := bySession[sessionID]
		if r == nil {
			r = &sessionRows{}
			bySession[sessionID] = r
		}
		return r
	}

	// INDEXED BY for the reason BackfillTokenUsage gives: the index is walked
	// and only rows of the wanted types are fetched from the table.
	hourRows, err := s.reader.Query(`
		SELECT session_id, `+hourStartUnixSQL+` AS hour_start_unix,
		       COALESCE(SUM(type = 'user_message'), 0),
		       COALESCE(SUM(CASE WHEN type = 'result' THEN json_extract(data, '$.result.usage.input_tokens') END), 0),
		       COALESCE(SUM(CASE WHEN type = 'result' THEN json_extract(data, '$.result.usage.output_tokens') END), 0),
		       COALESCE(SUM(CASE WHEN type = 'result' THEN json_extract(data, '$.result.usage.cache_read_tokens') END), 0),
		       COALESCE(SUM(CASE WHEN type = 'result' THEN json_extract(data, '$.result.usage.cache_write_tokens') END), 0),
		       COALESCE(SUM(CASE WHEN type = 'result' THEN json_extract(data, '$.result.duration_ms') END), 0),
		       MAX(CASE WHEN type = 'session_cost' THEN json_extract(data, '$.session_cost.total_usd') END)
		FROM events INDEXED BY idx_events_session_type
		WHERE type IN ('user_message', 'result', 'session_cost') AND id <= ?
		GROUP BY 1, 2
	`, line.throughEventID)
	if err != nil {
		return report, fmt.Errorf("read turn-level events by hour: %w", err)
	}
	for hourRows.Next() {
		var sessionID string
		var h sessionHour
		if err := hourRows.Scan(&sessionID, &h.hourStartUnix, &h.usage.TurnCount,
			&h.usage.InputTokens, &h.usage.OutputTokens,
			&h.usage.CacheReadTokens, &h.usage.CacheWriteTokens,
			&h.usage.DurationMS, &h.usage.SessionCostTotalUSD); err != nil {
			hourRows.Close()
			return report, fmt.Errorf("scan a session hour: %w", err)
		}
		r := rowsOf(sessionID)
		r.hours = append(r.hours, h)
	}
	if err := hourRows.Close(); err != nil {
		return report, fmt.Errorf("read turn-level events by hour: %w", err)
	}
	if err := hourRows.Err(); err != nil {
		return report, fmt.Errorf("read turn-level events by hour: %w", err)
	}

	apiCallRows, err := s.reader.Query(`
		SELECT session_id, `+hourStartUnixSQL+` AS hour_start_unix,
		       COALESCE(json_extract(data, '$.api_call.query_source'), ''),
		       COUNT(*),
		       COALESCE(SUM(json_extract(data, '$.api_call.input_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.output_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.cache_read_tokens')), 0),
		       COALESCE(SUM(json_extract(data, '$.api_call.cache_creation_tokens')), 0)
		FROM events INDEXED BY idx_events_session_type
		WHERE type = 'api_call' AND id <= ?
		  AND json_type(data, '$.api_call') = 'object'
		GROUP BY 1, 2, 3
	`, line.throughEventID)
	if err != nil {
		return report, fmt.Errorf("read api_call events by hour: %w", err)
	}
	for apiCallRows.Next() {
		var sessionID string
		var h apiCallSessionHour
		if err := apiCallRows.Scan(&sessionID, &h.hourStartUnix, &h.usage.QuerySource, &h.usage.Calls,
			&h.usage.InputTokens, &h.usage.OutputTokens,
			&h.usage.CacheReadTokens, &h.usage.CacheWriteTokens); err != nil {
			apiCallRows.Close()
			return report, fmt.Errorf("scan an api call session hour: %w", err)
		}
		r := rowsOf(sessionID)
		r.apiCallHours = append(r.apiCallHours, h)
	}
	if err := apiCallRows.Close(); err != nil {
		return report, fmt.Errorf("read api_call events by hour: %w", err)
	}
	if err := apiCallRows.Err(); err != nil {
		return report, fmt.Errorf("read api_call events by hour: %w", err)
	}

	// Sessions in id order, starting after the last one an interrupted run
	// wrote. SQLite compares TEXT by bytes and so does Go, so "after" means
	// the same thing in the stored mark and in this slice.
	sessionIDs := make([]string, 0, len(bySession))
	for sessionID := range bySession {
		if sessionID > line.resumeAfterSessionID {
			sessionIDs = append(sessionIDs, sessionID)
		}
	}
	sort.Strings(sessionIDs)

	writeSessions := func(batch []string, isLast bool) error {
		tx, err := s.writer.Begin()
		if err != nil {
			return fmt.Errorf("begin hourly usage backfill write: %w", err)
		}
		defer tx.Rollback()
		addHour, err := tx.Prepare(addToUsageByHourSQL)
		if err != nil {
			return fmt.Errorf("prepare session hour write: %w", err)
		}
		defer addHour.Close()
		addAPICallHour, err := tx.Prepare(addToAPICallUsageByHourSQL)
		if err != nil {
			return fmt.Errorf("prepare api call session hour write: %w", err)
		}
		defer addAPICallHour.Close()
		for _, sessionID := range batch {
			r := bySession[sessionID]
			for _, h := range r.hours {
				if _, err := addHour.Exec(usageByHourArguments(sessionID, h.hourStartUnix, h.usage)...); err != nil {
					return fmt.Errorf("add session hour for %s: %w", sessionID, err)
				}
			}
			for _, h := range r.apiCallHours {
				if _, err := addAPICallHour.Exec(apiCallUsageByHourArguments(sessionID, h.hourStartUnix, h.usage)...); err != nil {
					return fmt.Errorf("add api call session hour for %s: %w", sessionID, err)
				}
			}
		}
		if len(batch) > 0 {
			if _, err := tx.Exec(
				`UPDATE projection_backfills SET resume_after_session_id = ? WHERE name = ?`,
				batch[len(batch)-1], usageByHourBackfillName,
			); err != nil {
				return fmt.Errorf("record how far the hourly usage backfill got: %w", err)
			}
		}
		if isLast {
			if _, err := tx.Exec(
				`UPDATE projection_backfills SET completed_at = CURRENT_TIMESTAMP WHERE name = ?`,
				usageByHourBackfillName,
			); err != nil {
				return fmt.Errorf("mark the hourly usage backfill complete: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit hourly usage backfill write: %w", err)
		}
		for _, sessionID := range batch {
			report.SessionHours += len(bySession[sessionID].hours)
			report.APICallSessionHours += len(bySession[sessionID].apiCallHours)
		}
		return nil
	}
	for len(sessionIDs) > sessionsPerWrite {
		if err := writeSessions(sessionIDs[:sessionsPerWrite], false); err != nil {
			return report, err
		}
		sessionIDs = sessionIDs[sessionsPerWrite:]
	}
	// The last write always runs, with no sessions when there are none left:
	// it is what marks the backfill complete.
	if err := writeSessions(sessionIDs, true); err != nil {
		return report, err
	}

	report.Took = time.Since(started)
	log.Printf("[log-store] hourly usage backfill done through event %d: %d session hours, %d api call session hours, %s",
		line.throughEventID, report.SessionHours, report.APICallSessionHours, report.Took.Round(time.Millisecond))
	return report, nil
}
