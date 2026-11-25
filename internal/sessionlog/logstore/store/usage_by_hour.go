package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// StoreEvent adds each new event. The SQLite store added the events stored
// before these tables existed in a background pass; the Postgres module has
// them from its first migration (see token_usage.go).

// hourStartUnixSQL turns events.created_at into the unix time its clock hour
// began.
const hourStartUnixSQL = `(floor(extract(epoch FROM created_at))::bigint / 3600) * 3600`

// hourStartUnixOf is the unix time the clock hour holding t began.
func hourStartUnixOf(t time.Time) int64 {
	return t.Unix() / 3600 * 3600
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
// the highest one seen is kept.
const addToUsageByHourSQL = `
	INSERT INTO session_usage_by_hour (
		session_id, hour_start_unix, turn_count,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		duration_ms, session_cost_total_usd
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (session_id, hour_start_unix) DO UPDATE SET
		turn_count         = session_usage_by_hour.turn_count         + excluded.turn_count,
		input_tokens       = session_usage_by_hour.input_tokens       + excluded.input_tokens,
		output_tokens      = session_usage_by_hour.output_tokens      + excluded.output_tokens,
		cache_read_tokens  = session_usage_by_hour.cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = session_usage_by_hour.cache_write_tokens + excluded.cache_write_tokens,
		duration_ms        = session_usage_by_hour.duration_ms        + excluded.duration_ms,
		session_cost_total_usd = CASE
			WHEN excluded.session_cost_total_usd IS NULL THEN session_usage_by_hour.session_cost_total_usd
			WHEN session_usage_by_hour.session_cost_total_usd IS NULL THEN excluded.session_cost_total_usd
			WHEN excluded.session_cost_total_usd > session_usage_by_hour.session_cost_total_usd THEN excluded.session_cost_total_usd
			ELSE session_usage_by_hour.session_cost_total_usd
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
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (session_id, hour_start_unix, query_source) DO UPDATE SET
		calls              = session_api_call_usage_by_hour.calls              + excluded.calls,
		input_tokens       = session_api_call_usage_by_hour.input_tokens       + excluded.input_tokens,
		output_tokens      = session_api_call_usage_by_hour.output_tokens      + excluded.output_tokens,
		cache_read_tokens  = session_api_call_usage_by_hour.cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = session_api_call_usage_by_hour.cache_write_tokens + excluded.cache_write_tokens`

func apiCallUsageByHourArguments(sessionID string, hourStartUnix int64, u APICallUsageRow) []any {
	return []any{sessionID, hourStartUnix, u.QuerySource, u.Calls,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}
}

// addEventToUsageByHour adds one newly stored event to its session's hour.
// Only four event types carry anything the hourly projections count; every
// other type returns at once.
func (s *Store) addEventToUsageByHour(db execer, eventID int64, sessionID, eventType string, data []byte) error {
	switch eventType {
	case "user_message", "result", "api_call", "session_cost":
	default:
		return nil
	}
	var hourStartUnix int64
	if err := db.QueryRow(
		`SELECT `+hourStartUnixSQL+` FROM events WHERE id = $1`, eventID,
	).Scan(&hourStartUnix); err != nil {
		return fmt.Errorf("read the hour of event %d: %w", eventID, err)
	}

	switch eventType {
	case "user_message":
		_, err := db.Exec(addToUsageByHourSQL,
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
		_, err := db.Exec(addToUsageByHourSQL,
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
		_, err := db.Exec(addToUsageByHourSQL,
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
		_, err := db.Exec(addToAPICallUsageByHourSQL,
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
		SELECT session_id, query_source, SUM(calls)::bigint,
		       SUM(input_tokens)::bigint, SUM(output_tokens)::bigint, SUM(cache_read_tokens)::bigint, SUM(cache_write_tokens)::bigint
		FROM session_api_call_usage_by_hour
		WHERE hour_start_unix >= $1
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
			SELECT session_id FROM session_usage_by_hour WHERE hour_start_unix >= $1
			UNION
			SELECT session_id FROM session_api_call_usage_by_hour WHERE hour_start_unix >= $1
		)
		SELECT a.session_id,
		       COALESCE((SELECT model FROM sessions s WHERE s.session_id = a.session_id), ''),
		       COALESCE(SUM(u.turn_count), 0)::bigint,
		       COALESCE(SUM(u.input_tokens), 0)::bigint,
		       COALESCE(SUM(u.output_tokens), 0)::bigint,
		       COALESCE(SUM(u.cache_read_tokens), 0)::bigint,
		       COALESCE(SUM(u.cache_write_tokens), 0)::bigint,
		       COALESCE(SUM(u.duration_ms), 0)::bigint,
		       MAX(u.session_cost_total_usd),
		       (SELECT MAX(b.session_cost_total_usd) FROM session_usage_by_hour b
		         WHERE b.session_id = a.session_id AND b.hour_start_unix < $1)
		FROM active a
		LEFT JOIN session_usage_by_hour u
		       ON u.session_id = a.session_id AND u.hour_start_unix >= $1
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
