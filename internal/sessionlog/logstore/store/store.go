package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge/msg"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the session event log keeps its
// records in, with the migrations that build them. The schema is named for the
// plan's session_log package, which this store becomes.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "session_log", Role: "session_log_module", Migrations: migrations}, nil
}

// readerPoolSize bounds how many queries run at once. Reads here are not
// small — one /messages call over the largest real session scans 13,776 rows
// and serializes 306MB — so an unbounded pool would let a handful of clients
// pin every core. Eight is well above the number of dashboards this serves
// and still leaves the machine room to run the harnesses that feed it.
const readerPoolSize = 8

type Store struct {
	// writer runs every INSERT and UPDATE through a single connection. The turn
	// index depends on it: BuildTurnIndex marks a session current in the same
	// write that reads its last unindexed events, and only a single writer
	// keeps an event from landing between the two (see turn_index.go). It also
	// keeps event ids in the order of their created_at, which the windowed
	// search relies on.
	writer *sql.DB

	// reader runs every SELECT on its own pool, so a long read never holds the
	// writer. Its connections open read only (default_transaction_read_only),
	// so a write routed here fails loudly instead of competing with the writer.
	reader *sql.DB

	// turnIndexLocks keeps two builds of one session's turn index from running at
	// once. See turn_index.go.
	turnIndexLocks sessionLocks

	// dedupCandidateOf and turnIndexVersion come from the server through
	// SetDedupCandidates. See turn_index.go.
	dedupCandidateOf DedupCandidateFunc
	turnIndexVersion int
}

// New builds the store over the module's pools: writer, provisioned and
// migrated (postgresstorage.Provision, Open, Migrate), and reader, from
// OpenReader. The store takes both and closes them in Close.
func New(writer, reader *sql.DB) *Store {
	writer.SetMaxOpenConns(1)
	reader.SetMaxOpenConns(readerPoolSize)
	return &Store{writer: writer, reader: reader, turnIndexVersion: turnIndexRulesVersion * 1000}
}

// OpenReader opens the module's read-only pool on the Postgres at url: the
// module's role and schema, with every transaction read only.
func OpenReader(ctx context.Context, url string, module postgresstorage.Module) (*sql.DB, error) {
	readOnlyURL, err := withRuntimeParameter(url, "default_transaction_read_only", "on")
	if err != nil {
		return nil, err
	}
	return postgresstorage.Open(ctx, readOnlyURL, module)
}

// withRuntimeParameter adds a session setting to a Postgres connection
// string, in either of its two forms. pgx passes a parameter it does not know
// to the server as a setting.
func withRuntimeParameter(connectionString, name, value string) (string, error) {
	if strings.Contains(connectionString, "://") {
		separator := "?"
		if strings.Contains(connectionString, "?") {
			separator = "&"
		}
		return connectionString + separator + name + "=" + value, nil
	}
	if strings.TrimSpace(connectionString) == "" {
		return "", errors.New("log-store: the Postgres connection string is empty")
	}
	return connectionString + " " + name + "=" + value, nil
}

func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}

// RebuildMissingSessionRows adds the sessions row of every session that has
// events and no row, from its events. StoreEvent seeds the row of a new
// session after the event commits, so a failed seed leaves a session without
// one; this repairs it. It reads every session id in the events index, so the
// server runs it beside its work at start, not before it.
func (s *Store) RebuildMissingSessionRows() (int64, error) {
	result, err := s.writer.Exec(`
		INSERT INTO sessions (
			session_id, turn_count, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens,
			duration_ms, model, harness_session_id, started_at, last_active, ended_at
		)
		SELECT
			e.session_id,
			COUNT(*) FILTER (WHERE e.type = 'user_message'),
			COALESCE(SUM(json_number(e.data->'result'->'usage'->'input_tokens')) FILTER (WHERE e.type = 'result'), 0)::bigint,
			COALESCE(SUM(json_number(e.data->'result'->'usage'->'output_tokens')) FILTER (WHERE e.type = 'result'), 0)::bigint,
			COALESCE(SUM(json_number(e.data->'result'->'usage'->'cache_read_tokens')) FILTER (WHERE e.type = 'result'), 0)::bigint,
			COALESCE(SUM(json_number(e.data->'result'->'usage'->'cache_write_tokens')) FILTER (WHERE e.type = 'result'), 0)::bigint,
			COALESCE(SUM(json_number(e.data->'result'->'duration_ms')) FILTER (WHERE e.type = 'result'), 0)::bigint,
			COALESCE((
				SELECT e2.data->'result'->>'model'
				FROM events e2
				WHERE e2.session_id = e.session_id AND e2.type = 'result'
				  AND e2.data->'result'->>'model' IS NOT NULL
				ORDER BY e2.id DESC LIMIT 1
			), ''),
			COALESCE((
				SELECT e3.data->>'harness_session_id'
				FROM events e3
				WHERE e3.session_id = e.session_id
				  AND COALESCE(e3.data->>'harness_session_id', '') <> ''
				ORDER BY e3.id DESC LIMIT 1
			), ''),
			MIN(e.created_at),
			MAX(e.created_at),
			MAX(e.created_at) FILTER (WHERE e.type IN ('result', 'error'))
		FROM events e
		WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = e.session_id)
		GROUP BY e.session_id
		ON CONFLICT (session_id) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("rebuild missing sessions rows: %w", err)
	}
	return result.RowsAffected()
}

// StoreEvent persists a raw event and updates the per-session projection.
// The projection is a derived cache; failure to update is logged but does
// not fail the underlying event insert (the event is still queryable; the
// next StoreEvent call rolls forward correctly, and RebuildMissingSessionRows
// adds a row whose seed failed).
//
// ctx bounds the insert: an insert it cancels may still have been stored, as
// a commit that is cut off cannot say whether it landed.
func (s *Store) StoreEvent(ctx context.Context, sessionID, eventType string, data []byte) (int64, error) {
	// The event and its place in the turn index are written together, so the index
	// can never hold an event the table does not, or miss one it does.
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO events (session_id, type, data) VALUES ($1, $2, $3) RETURNING id`,
		sessionID, eventType, string(data),
	).Scan(&id); err != nil {
		return 0, err
	}
	if err := s.indexEventInTx(tx, sessionID, id, eventType, data); err != nil {
		return 0, fmt.Errorf("index event in its turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.updateSessionProjection(sessionID, eventType, data); err != nil {
		log.Printf("[log-store] update sessions projection for %s/%s: %v", sessionID, eventType, err)
	}
	// Its own call, so a failure in one projection does not skip the other.
	if err := s.addEventToUsageByHour(id, sessionID, eventType, data); err != nil {
		log.Printf("[log-store] update hourly usage for %s/%s: %v", sessionID, eventType, err)
	}
	return id, nil
}

// updateSessionProjection rolls forward the per-session sessions row for one
// new event. Idempotent on re-application of the same event because
// last_active uses CURRENT_TIMESTAMP and turn_count / token sums are
// monotonic only when StoreEvent is called once per event (the caller's job).
func (s *Store) updateSessionProjection(sessionID, eventType string, data []byte) error {
	// Ensure the row exists. INSERT OR IGNORE is a no-op for established
	// sessions; for the first event of a new session it seeds started_at /
	// last_active to now().
	if _, err := s.writer.Exec(
		`INSERT INTO sessions (session_id) VALUES ($1) ON CONFLICT (session_id) DO NOTHING`,
		sessionID,
	); err != nil {
		return fmt.Errorf("seed sessions row: %w", err)
	}
	// Bump last_active on every event regardless of type.
	if _, err := s.writer.Exec(
		`UPDATE sessions SET last_active = now() WHERE session_id = $1`,
		sessionID,
	); err != nil {
		return fmt.Errorf("touch last_active: %w", err)
	}
	// Roll the harness-native id forward from whatever this event reports.
	// It is written on every event type rather than on session start because
	// log-store has no session-start event: the first frame of a session is
	// just an ordinary event, and a resumed session reports a new harness
	// uuid partway through. Last writer wins, matching the migration's
	// MAX(id) backfill and the gateway's own row.
	if harnessSessionID := harnessSessionIDOf(data); harnessSessionID != "" {
		if _, err := s.writer.Exec(
			`UPDATE sessions SET harness_session_id = $1
			 WHERE session_id = $2 AND harness_session_id <> $1`,
			harnessSessionID, sessionID,
		); err != nil {
			return fmt.Errorf("set harness_session_id: %w", err)
		}
	}
	switch eventType {
	case "user_message":
		_, err := s.writer.Exec(
			`UPDATE sessions SET turn_count = turn_count + 1 WHERE session_id = $1`,
			sessionID,
		)
		return err
	case "result":
		// Parse usage / cost / model / duration from the result event.
		// Failures here are silent: the projection just doesn't get the
		// numbers from this event. Better to log and continue than to
		// block event ingestion on a malformed result.
		var ev struct {
			Result struct {
				Usage struct {
					InputTokens      int64 `json:"input_tokens"`
					OutputTokens     int64 `json:"output_tokens"`
					CacheReadTokens  int64 `json:"cache_read_tokens"`
					CacheWriteTokens int64 `json:"cache_write_tokens"`
				} `json:"usage"`
				DurationMS int64  `json:"duration_ms"`
				Model      string `json:"model"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("[log-store] decode result event %s: %v", sessionID, err)
			// Still set ended_at — the event terminated the turn even if
			// the body was malformed.
			_, err := s.writer.Exec(
				`UPDATE sessions SET ended_at = now() WHERE session_id = $1`,
				sessionID,
			)
			return err
		}
		_, err := s.writer.Exec(
			`UPDATE sessions SET
				input_tokens = input_tokens + $1,
				output_tokens = output_tokens + $2,
				cache_read_tokens = cache_read_tokens + $3,
				cache_write_tokens = cache_write_tokens + $4,
				duration_ms = duration_ms + $5,
				model = CASE WHEN $6 <> '' THEN $6 ELSE model END,
				ended_at = now()
			WHERE session_id = $7`,
			ev.Result.Usage.InputTokens,
			ev.Result.Usage.OutputTokens,
			ev.Result.Usage.CacheReadTokens,
			ev.Result.Usage.CacheWriteTokens,
			ev.Result.DurationMS,
			ev.Result.Model,
			sessionID,
		)
		return err
	case "api_call":
		return s.addAPICallToSessionUsage(sessionID, data)
	case "error":
		_, err := s.writer.Exec(
			`UPDATE sessions SET ended_at = now() WHERE session_id = $1`,
			sessionID,
		)
		return err
	}
	return nil
}

// ListEventsSinceID returns a session's events after a row ID (0 for all of them),
// oldest first. Each event's JSON is decorated with an "event_id" field carrying
// its row id, so a client can track the high-water mark and dedupe against a
// stream replay. If types is non-empty, only events of those types are returned.
func (s *Store) ListEventsSinceID(sessionID string, afterID int64, types []string) ([]json.RawMessage, error) {
	q := `SELECT id, data FROM events WHERE session_id = $1 AND id > $2`
	args := []any{sessionID, afterID}
	if len(types) > 0 {
		q += ` AND type = ANY($3)`
		args = append(args, types)
	}
	q += ` ORDER BY id ASC`
	rows, err := s.reader.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []json.RawMessage
	for rows.Next() {
		var rowID int64
		var data string
		if err := rows.Scan(&rowID, &data); err != nil {
			return nil, err
		}
		events = append(events, injectEventID([]byte(data), rowID))
	}
	return events, rows.Err()
}

// injectEventID splices an "event_id":<rowID> field into an event's top-level
// JSON object. Returns the original bytes unchanged if data isn't a JSON object.
func injectEventID(data []byte, rowID int64) json.RawMessage {
	trimmed := data
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\n' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return data
	}
	prefix := fmt.Sprintf(`{"event_id":%d,`, rowID)
	// Handle empty object "{}" — drop the trailing comma.
	if len(trimmed) >= 2 && trimmed[1] == '}' {
		prefix = fmt.Sprintf(`{"event_id":%d`, rowID)
	}
	out := make([]byte, 0, len(prefix)+len(trimmed)-1)
	out = append(out, prefix...)
	out = append(out, trimmed[1:]...)
	return out
}

// harnessSessionIDOf reads the harness-native session id an event carries.
// Returns "" for a malformed body or an event that names no harness id —
// both are ordinary, so neither is an error: 1,675 of this host's 11,640
// sessions have no harness id anywhere in their event stream.
func harnessSessionIDOf(data []byte) string {
	var ev struct {
		HarnessSessionID string `json:"harness_session_id"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return ""
	}
	return ev.HarnessSessionID
}

// SearchSessions returns session IDs with events whose raw data contains query.
// Match count is the number of matching events per session.
//
// Each hit reports both ids it has. session_id is log-store's own key and is
// whatever the writer handed it — for a third of this host's sessions that is
// a bridge_id llm-bridge-server has since deleted as a phantom, so taking it
// straight to GET /sessions/{id} yields a 404 for a live session.
// harness_session_id is the harness-native id from the session's events, and
// it is what the gateway keys its own unique index on. A consumer that 404s
// on the first should retry on the second before calling a hit unrenderable.
func (s *Store) SearchSessions(query string, limit int) ([]SearchHit, error) {
	return s.SearchSessionsInWindow(query, limit, time.Time{}, time.Time{})
}

// SearchSessionsInWindow is SearchSessions bounded to events created inside
// [since, until); a zero time leaves that side unbounded.
//
// The bound exists because the unwindowed scan is a raw LIKE over EVERY event
// blob — measured 2026-09-01 at 47–56s against 2.3M events / 5.9GB, which is
// unusable for any interactive caller. Most interactive questions are about a
// MOMENT ("who committed this on Aug 22"), and the events table is append-only
// with ids monotonic in created_at, so a time window translates to an id range
// found by binary search over ~21 point reads — no created_at index needed,
// and the LIKE then scans only the slice inside the range (the same query,
// windowed to a day, answers in ~1s).
//
// Monotonicity is real, not assumed: id is the SQLite rowid handed out at
// INSERT and created_at is stamped by the same INSERT's CURRENT_TIMESTAMP, so
// a later row can never carry an earlier created_at beyond same-second ties —
// and the binary search treats ties conservatively (>= on the left bound).
func (s *Store) SearchSessionsInWindow(query string, limit int, since, until time.Time) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 100
	}
	idConditions := ""
	args := []any{query}
	nextArgument := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	if !since.IsZero() {
		lo, ok, err := s.firstEventIDAtOrAfter(since)
		if err != nil {
			return nil, err
		}
		if !ok {
			return []SearchHit{}, nil // every event predates the window
		}
		idConditions += " AND id >= " + nextArgument(lo)
	}
	if !until.IsZero() {
		hi, ok, err := s.firstEventIDAtOrAfter(until)
		if err != nil {
			return nil, err
		}
		if ok {
			idConditions += " AND id < " + nextArgument(hi)
		}
		// No event at or after `until` means the window's right edge is past
		// the end of the table — unbounded on that side, correctly.
	}
	idConditions += injectedUserMessageExclusion(nextArgument)
	limitArgument := nextArgument(limit)
	// The join sits outside the LIMIT, not inside it, so the projection is
	// looked up at most `limit` times rather than once per matched group.
	//
	// ILIKE with no escape character is SQLite's LIKE: case is ignored, % and
	// _ in the query match as wildcards, and a backslash is an ordinary
	// character. (SQLite folds only ASCII letters; ILIKE folds every letter.)
	//
	// LEFT JOIN, not JOIN: a session with events but no projection row must
	// still appear as a hit with an empty harness id, exactly as it did
	// before this column existed. An inner join would silently drop it.
	rows, err := s.reader.Query(
		`SELECT hit.session_id, hit.match_count, COALESCE(s.harness_session_id, '')
		 FROM (
			SELECT session_id, COUNT(*) AS match_count, MAX(id) AS last_event_id
			FROM events
			WHERE data::text ILIKE '%' || $1 || '%' ESCAPE ''`+idConditions+`
			GROUP BY session_id
			ORDER BY last_event_id DESC
			LIMIT `+limitArgument+`
		 ) hit
		 LEFT JOIN sessions s ON s.session_id = hit.session_id
		 ORDER BY hit.last_event_id DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.SessionID, &h.MatchCount, &h.HarnessSessionID); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// injectedUserMessageExclusion leaves out of a search the user messages that
// are not what a person wrote: Codex writes the environment, the AGENTS.md
// instructions it loaded and similar markers into a conversation as user
// messages, and an imported Codex session stores each as a user_message
// event. A raw match on those would find nearly every Codex session for any
// word the instructions happen to contain. The list of how such a message
// begins is llm-bridge's (msg.CodexInjectedTextPrefixes), the same one the
// harness wrapper and the server use, so the three agree on what counts.
//
// The condition is appended after the match, so Postgres reads the event's
// JSON only for rows whose raw text already matched. nextArgument adds a value
// to the query and answers its placeholder.
func injectedUserMessageExclusion(nextArgument func(any) string) string {
	prefixes := msg.CodexInjectedTextPrefixes()
	if len(prefixes) == 0 {
		return ""
	}
	tests := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		tests[i] = "starts_with(ltrim(COALESCE(data->'result'->>'text', ''), E' \\t\\n\\r'), " + nextArgument(prefix) + ")"
	}
	return " AND NOT (type = 'user_message' AND (" + strings.Join(tests, " OR ") + "))"
}

// firstEventIDAtOrAfter finds the smallest event id whose created_at is at or
// after t, by binary search over the id space — ~21 point reads against the
// primary key instead of a full scan against the unindexed created_at column.
// ok=false means no event is that late (t is past the end of the table).
func (s *Store) firstEventIDAtOrAfter(t time.Time) (int64, bool, error) {
	// SQLite kept created_at to the second and compared it to t written to the
	// second. Truncating t keeps that: an event in the same second as t is at
	// or after it.
	want := t.UTC().Truncate(time.Second)
	var loID, hiID int64
	err := s.reader.QueryRow(`SELECT COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM events`).Scan(&loID, &hiID)
	if err != nil {
		return 0, false, err
	}
	if hiID == 0 {
		return 0, false, nil
	}
	at := func(id int64) (int64, time.Time, error) {
		var gotID int64
		var created time.Time
		err := s.reader.QueryRow(
			`SELECT id, created_at FROM events WHERE id >= $1 ORDER BY id LIMIT 1`, id,
		).Scan(&gotID, &created)
		return gotID, created, err
	}
	if _, created, err := at(hiID); err != nil || created.Before(want) {
		return 0, false, err // the newest event still predates t
	}
	lo, hi := loID, hiID // invariant: answer is in (lo, hi] once at(lo) < want
	if _, created, err := at(lo); err != nil {
		return 0, false, err
	} else if !created.Before(want) {
		return lo, true, nil
	}
	// The search is over the id space, not the ids present: at(x) reads the
	// first event at or after x, so "at(x) is at or after t" only turns from
	// false to true as x grows, gaps or not. hi may end up an id no event
	// has, which is as good a bound as the next one present. Postgres leaves
	// gaps in an identity column (a rolled-back insert keeps its number), and
	// the earlier form, which set hi to the id found, never ended on one.
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		gotID, created, err := at(mid)
		if err != nil {
			return 0, false, err
		}
		if !created.Before(want) {
			hi = mid
		} else {
			lo = gotID
		}
	}
	return hi, true, nil
}

// SearchHit is a single session match from SearchSessions.
type SearchHit struct {
	SessionID  string `json:"session_id"`
	MatchCount int    `json:"match_count"`
	// HarnessSessionID is the harness-native id (a Claude Code UUID, a Codex
	// thread_id) taken from the session's own events. Empty when the events
	// name none. It is the key that resolves a hit against llm-bridge-server
	// when session_id does not.
	HarnessSessionID string `json:"harness_session_id"`
}

// HeldSession names one log-store session that already holds a transcript,
// as reported by SessionsHoldingHarnessSessionID.
type HeldSession struct {
	// SessionID is log-store's own key for the transcript — whatever the
	// writer handed it. Usually an llm-bridge-server bridge id, and for a
	// third of this host's rows a bridge id the gateway has since deleted.
	SessionID string `json:"session_id"`
	// EventCount is how many events this session holds. Counted from the
	// events table rather than read off the projection, because the caller
	// is asking whether a transcript is already durable and the projection
	// counts turns, not events.
	EventCount int `json:"event_count"`
	// LastActive is the projection's last-activity stamp for the session.
	LastActive string `json:"last_active"`
}

// lastActiveText writes a last-activity stamp as the SQLite store answered
// it: the driver handed back its DATETIME as a time, and database/sql wrote
// that into a string as RFC 3339 with nanoseconds.
func lastActiveText(lastActive time.Time) string {
	return lastActive.UTC().Format(time.RFC3339Nano)
}

// SessionsHoldingHarnessSessionID answers "do you already hold this
// harness session's transcript?" — the question llm-bridge-server's
// discovery has to ask before importing one.
//
// Discovery decides a session is new by asking its OWN database, then writes
// the answer's consequence into log-store. A gateway booted with a fresh
// database and the default log-store URL therefore re-imports every
// transcript on disk; that is how 2,863 duplicate sessions reached this
// host's production log-store on 2026-08-01. The dedupe key has to live in
// the store that holds the writes, and the only id both services agree on is
// the harness-native one.
//
// An empty id is refused rather than answered. Sessions whose events name no
// harness id all carry ” in the projection, so treating ” as a lookup key
// would report thousands of unrelated transcripts as a match and suppress a
// real import (the partial index deliberately excludes them too).
//
// Newest activity first, so a caller that wants one row gets the live one.
func (s *Store) SessionsHoldingHarnessSessionID(harnessSessionID string) ([]HeldSession, error) {
	if harnessSessionID == "" {
		return nil, fmt.Errorf("SessionsHoldingHarnessSessionID: harness_session_id is required")
	}
	rows, err := s.reader.Query(
		`SELECT s.session_id,
		        (SELECT COUNT(*) FROM events e WHERE e.session_id = s.session_id),
		        s.last_active
		 FROM sessions s
		 WHERE s.harness_session_id = $1
		 ORDER BY s.last_active DESC`,
		harnessSessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var held []HeldSession
	for rows.Next() {
		var h HeldSession
		var lastActive time.Time
		if err := rows.Scan(&h.SessionID, &h.EventCount, &lastActive); err != nil {
			return nil, err
		}
		h.LastActive = lastActiveText(lastActive)
		held = append(held, h)
	}
	return held, rows.Err()
}

// SessionAggregateRow is one session's summed totals as returned by
// ListSessionAggregates. JSON marshalling lives on msg.SessionAggregate;
// the handler converts row → response struct.
//
// InputTokens, OutputTokens, CacheReadTokens and CacheWriteTokens are the
// result events' sums; APICallUsage is the api_call events' sums, one row per
// query source, and holds every model call the harness reported — a
// subagent's included, which no result event counts.
type SessionAggregateRow struct {
	SessionID        string
	Turns            int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	DurationMS       int64
	Model            string
	APICallUsage     []APICallUsageRow
	// SessionCostUSD is set only by ListSessionAggregatesSince: how much the
	// session's session_cost total grew inside the window.
	SessionCostUSD float64
}

// ListSessionAggregates returns per-session token totals from the sessions
// projection table and the per-query-source API call totals, both maintained
// by StoreEvent. A session with no tokens in either is omitted: it has no
// usage to report.
//
// Turns counts user_message events (one per user turn). The previous
// implementation accidentally counted result events as "turns"; the
// projection's turn_count field is the corrected definition.
func (s *Store) ListSessionAggregates() ([]SessionAggregateRow, error) {
	apiCallUsageBySession, err := s.apiCallUsageBySession()
	if err != nil {
		return nil, err
	}
	rows, err := s.reader.Query(`
		SELECT session_id, turn_count, input_tokens, output_tokens,
		       cache_read_tokens, cache_write_tokens, duration_ms, model
		FROM sessions
		WHERE input_tokens > 0 OR output_tokens > 0
		   OR cache_read_tokens > 0 OR cache_write_tokens > 0
		   OR session_id IN (SELECT session_id FROM session_api_call_usage)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionAggregateRow
	for rows.Next() {
		var r SessionAggregateRow
		if err := rows.Scan(&r.SessionID, &r.Turns, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.DurationMS, &r.Model); err != nil {
			return nil, err
		}
		r.APICallUsage = apiCallUsageBySession[r.SessionID]
		out = append(out, r)
	}
	return out, rows.Err()
}

// TurnState reports per-session in-flight turn information by inspecting the
// events table for the latest user_message and the latest terminator
// (result/error).
//
// A turn is in-flight when a user_message exists with no later terminator.
// LastUserMessageEventID and LastTerminatorEventID are 0 when no such event
// exists; bridge-server callers compare them to decide whether to recover.
type TurnState struct {
	LastUserMessageEventID int64 `json:"last_user_message_event_id"`
	LastTerminatorEventID  int64 `json:"last_terminator_event_id"`
	InFlight               bool  `json:"in_flight"`
}

// TurnState returns the latest user_message and terminator row IDs for a
// session along with the derived in-flight flag.
func (s *Store) TurnState(sessionID string) (TurnState, error) {
	var ts TurnState
	row := s.reader.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1 AND type = 'user_message'`,
		sessionID,
	)
	if err := row.Scan(&ts.LastUserMessageEventID); err != nil {
		return ts, err
	}
	row = s.reader.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1 AND type IN ('result','error')`,
		sessionID,
	)
	if err := row.Scan(&ts.LastTerminatorEventID); err != nil {
		return ts, err
	}
	ts.InFlight = ts.LastUserMessageEventID > ts.LastTerminatorEventID && ts.LastUserMessageEventID > 0
	return ts, nil
}

// SessionIDs returns all distinct session IDs that have events.
func (s *Store) SessionIDs() ([]string, error) {
	rows, err := s.reader.Query(`SELECT DISTINCT session_id FROM events ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
