package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge/msg"
)

// Two per-session token projections, added on 2026-11-17 after a session whose
// model calls read 2.1 billion cached tokens and whose subagents made nine calls
// in ten showed on the usage page as 192 thousand tokens:
//
//   - sessions.cache_read_tokens and sessions.cache_write_tokens, summed from
//     result events beside the input and output tokens already kept there.
//   - session_api_call_usage, the api_call events summed per session and query
//     source. A result event counts only the main conversation's finished
//     turns; an api_call event is written for every model call the harness
//     reports, and its query source says who made it.
//
// Both are derived from events. StoreEvent adds each new event as it is
// stored. In SQLite they were added after millions of events were stored, so a
// background pass added the older events; in Postgres they exist from the
// module's first migration, and the import copies them from a SQLite file
// whose pass has finished, so nothing here has older events to add.

// addAPICallToSessionUsage adds one newly stored api_call event to its
// session's total for its query source.
func (s *Store) addAPICallToSessionUsage(db statementWriter, sessionID string, data []byte) error {
	var ev struct {
		APICall *msg.APICallEvent `json:"api_call"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("decode api_call event: %w", err)
	}
	if ev.APICall == nil {
		return errors.New("api_call event carries no api_call body")
	}
	_, err := db.Exec(addToAPICallUsageSQL,
		sessionID, ev.APICall.QuerySource, 1,
		ev.APICall.InputTokens, ev.APICall.OutputTokens,
		ev.APICall.CacheReadTokens, ev.APICall.CacheCreationTokens,
	)
	return err
}

// addToAPICallUsageSQL adds to a (session, query source) total, creating it
// at the first call.
const addToAPICallUsageSQL = `
	INSERT INTO session_api_call_usage (
		session_id, query_source, calls,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
	) VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (session_id, query_source) DO UPDATE SET
		calls              = session_api_call_usage.calls              + excluded.calls,
		input_tokens       = session_api_call_usage.input_tokens       + excluded.input_tokens,
		output_tokens      = session_api_call_usage.output_tokens      + excluded.output_tokens,
		cache_read_tokens  = session_api_call_usage.cache_read_tokens  + excluded.cache_read_tokens,
		cache_write_tokens = session_api_call_usage.cache_write_tokens + excluded.cache_write_tokens`

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
