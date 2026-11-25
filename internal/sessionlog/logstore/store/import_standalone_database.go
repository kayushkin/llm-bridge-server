package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTable is one table of the standalone log-store's SQLite file: its
// columns as the file holds them, the order rows are read in, and how each
// column's value becomes the value the module stores.
type standaloneTable struct {
	name    string
	columns []string
	orderBy string
	kinds   []valueKind
	// derivedColumns are columns the module keeps that the file does not,
	// each filled by derive from the row's converted values.
	derivedColumns []string
	derive         func(converted []any) ([]any, error)
}

// valueKind says how one SQLite value is copied.
type valueKind int

const (
	integerValue valueKind = iota
	textValue
	// eventBodyValue is an event's JSON, which must be UTF-8 and JSON to be
	// stored at all.
	eventBodyValue
	// timeValue is a DATETIME the store wrote as CURRENT_TIMESTAMP, in UTC.
	timeValue
	// nullableTimeValue is a timeValue that may be NULL.
	nullableTimeValue
	booleanValue
	nullableRealValue
)

// standaloneTables are every table the standalone log-store kept, in the
// order the import copies them. projection_backfills is read but not copied
// (see ImportStandaloneDatabase).
var standaloneTables = []standaloneTable{
	{name: "events", columns: []string{"id", "session_id", "type", "data", "created_at"}, orderBy: "id",
		kinds:          []valueKind{integerValue, textValue, textValue, eventBodyValue, timeValue},
		derivedColumns: []string{"message_id", "harness_message_id", "tool_use_id"},
		derive:         eventColumnsOfImportedBody},
	{name: "sessions", columns: []string{"session_id", "turn_count", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
		"duration_ms", "model", "harness_session_id", "started_at", "last_active", "ended_at"}, orderBy: "session_id",
		kinds: []valueKind{textValue, integerValue, integerValue, integerValue, integerValue, integerValue,
			integerValue, textValue, textValue, timeValue, timeValue, nullableTimeValue}},
	{name: "session_api_call_usage", columns: []string{"session_id", "query_source", "calls", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"},
		orderBy: "session_id, query_source",
		kinds:   []valueKind{textValue, textValue, integerValue, integerValue, integerValue, integerValue, integerValue}},
	{name: "session_usage_by_hour", columns: []string{"session_id", "hour_start_unix", "turn_count", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_write_tokens", "duration_ms", "session_cost_total_usd"}, orderBy: "session_id, hour_start_unix",
		kinds: []valueKind{textValue, integerValue, integerValue, integerValue, integerValue, integerValue, integerValue, integerValue, nullableRealValue}},
	{name: "session_api_call_usage_by_hour", columns: []string{"session_id", "hour_start_unix", "query_source", "calls", "input_tokens",
		"output_tokens", "cache_read_tokens", "cache_write_tokens"}, orderBy: "session_id, hour_start_unix, query_source",
		kinds: []valueKind{textValue, integerValue, textValue, integerValue, integerValue, integerValue, integerValue, integerValue}},
	{name: "turn_index_sessions", columns: []string{"session_id", "index_version", "indexed_through_event_id", "last_turn_id", "next_turn_seq"},
		orderBy: "session_id",
		kinds:   []valueKind{textValue, integerValue, integerValue, textValue, integerValue}},
	{name: "event_turns", columns: []string{"event_id", "session_id", "turn_seq"}, orderBy: "event_id",
		kinds: []valueKind{integerValue, textValue, integerValue}},
	{name: "turns", columns: []string{"session_id", "turn_seq", "turn_id", "first_event_id", "last_event_id", "event_count", "has_user_message",
		"materializer_version", "materialized_through_event_id", "turn_json", "source_groups_json", "aggregate_sources_json", "dedup_fingerprint"},
		orderBy: "session_id, turn_seq",
		kinds: []valueKind{textValue, integerValue, textValue, integerValue, integerValue, integerValue, booleanValue,
			integerValue, integerValue, textValue, textValue, textValue, textValue}},
	{name: "turn_entries", columns: []string{"session_id", "turn_seq", "event_id", "entry_json"}, orderBy: "session_id, turn_seq, event_id",
		kinds: []valueKind{textValue, integerValue, integerValue, textValue}},
	{name: "dedup_candidates", columns: []string{"event_id", "session_id", "turn_seq", "key_hash", "source"}, orderBy: "event_id",
		kinds: []valueKind{integerValue, textValue, integerValue, textValue, textValue}},
}

// projectionBackfillsColumns are the columns of the standalone store's record
// of its two projection backfills.
var projectionBackfillsColumns = []string{"name", "through_event_id", "completed_at", "resume_after_session_id"}

// ImportCounts says how many rows of each table an import copied, and the
// highest event number the module numbers on from.
type ImportCounts struct {
	RowsByTable map[string]int64
	// LastEventNumber is the last event id the standalone store handed out;
	// the next event stored takes the one after it.
	LastEventNumber int64
	Took            time.Duration
}

// ImportStandaloneDatabase copies every table of the SQLite file of a
// standalone log-store into the module behind db, as it is: every event with
// its id, body and time, and every derived table, so the module answers
// what the standalone store answered without rebuilding anything. It runs in
// one transaction, so it copies everything or nothing, and it streams each
// table through COPY.
//
// It refuses a module that already holds any event or derived row, so it can
// never mix two logs or run twice, and it refuses a file it cannot copy whole:
// a table with a column it does not know, an event body that is not UTF-8 or
// not JSON, a value of the wrong type. It also refuses a file whose
// projection backfills (projection_backfills) have not finished: the module
// has no such pass, because its projections exist from its first migration,
// so it would never add what such a file still owes them.
//
// Afterwards events number on from the highest id the file ever handed out,
// so no event id that a chat page, an entry link or a cursor carries is
// handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	started := time.Now()
	counts := ImportCounts{RowsByTable: map[string]int64{}}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	for _, table := range standaloneTables {
		if err := standalonesqlite.CheckColumns(ctx, source, "log-store", table.name, table.columns); err != nil {
			return counts, err
		}
	}
	if err := standalonesqlite.CheckColumns(ctx, source, "log-store", "projection_backfills", projectionBackfillsColumns); err != nil {
		return counts, err
	}
	if err := refuseUnfinishedBackfills(ctx, source); err != nil {
		return counts, err
	}
	lastEventNumber, err := lastStandaloneEventNumber(ctx, source)
	if err != nil {
		return counts, err
	}

	connection, err := db.Conn(ctx)
	if err != nil {
		return counts, fmt.Errorf("taking a connection: %w", err)
	}
	defer connection.Close()
	err = connection.Raw(func(driverConnection any) error {
		pgxConnection := driverConnection.(*stdlib.Conn).Conn()
		return pgx.BeginFunc(ctx, pgxConnection, func(tx pgx.Tx) error {
			for _, table := range standaloneTables {
				var held int64
				if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&held); err != nil {
					return fmt.Errorf("counting %s: %w", table.name, err)
				}
				if held > 0 {
					return fmt.Errorf("the session_log module already holds %d rows in %s; the import copies into an empty module only", held, table.name)
				}
			}
			for _, table := range standaloneTables {
				copied, err := copyStandaloneTable(ctx, source, tx, table)
				if err != nil {
					return err
				}
				counts.RowsByTable[table.name] = copied
			}
			// setval(sequence, n, false) makes the next value n itself.
			if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence('events', 'id'), $1, false)`, lastEventNumber+1); err != nil {
				return fmt.Errorf("numbering events on after %d: %w", lastEventNumber, err)
			}
			return nil
		})
	})
	if err != nil {
		return ImportCounts{RowsByTable: map[string]int64{}}, err
	}
	counts.LastEventNumber = lastEventNumber
	counts.Took = time.Since(started)
	return counts, nil
}

// refuseUnfinishedBackfills refuses a file whose token usage or hourly usage
// backfill has not finished: its projections are short of the events stored
// before them.
func refuseUnfinishedBackfills(ctx context.Context, source *sql.DB) error {
	rows, err := source.QueryContext(ctx, `SELECT name, completed_at FROM projection_backfills ORDER BY name`)
	if err != nil {
		return fmt.Errorf("reading projection_backfills: %w", err)
	}
	defer rows.Close()
	finished := map[string]bool{}
	for rows.Next() {
		var name string
		var completedAt any
		if err := rows.Scan(&name, &completedAt); err != nil {
			return err
		}
		finished[name] = completedAt != nil
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range []string{"session_token_usage", "session_usage_by_hour"} {
		done, recorded := finished[name]
		if !recorded {
			return fmt.Errorf("projection_backfills has no %s row; start the standalone log-store once so it draws the line, and let the pass finish", name)
		}
		if !done {
			return fmt.Errorf("the standalone log-store's %s backfill has not finished; let it finish before importing, or its projections are short", name)
		}
	}
	return nil
}

// lastStandaloneEventNumber is the highest event id the standalone store ever
// handed out: its AUTOINCREMENT counter, or the highest id present if that is
// higher.
func lastStandaloneEventNumber(ctx context.Context, source *sql.DB) (int64, error) {
	var highestPresent int64
	if err := source.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&highestPresent); err != nil {
		return 0, fmt.Errorf("reading the highest event id: %w", err)
	}
	var counter sql.NullInt64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&counter)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("reading the events counter: %w", err)
	}
	if counter.Valid && counter.Int64 > highestPresent {
		return counter.Int64, nil
	}
	return highestPresent, nil
}

// copyStandaloneTable streams one table from the SQLite file into the module
// with COPY, converting each value, and answers how many rows it copied.
func copyStandaloneTable(ctx context.Context, source *sql.DB, tx pgx.Tx, table standaloneTable) (int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT "+joinColumns(table.columns)+" FROM "+table.name+" ORDER BY "+table.orderBy)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", table.name, err)
	}
	defer rows.Close()
	var read int64
	next := func() ([]any, error) {
		values := make([]any, len(table.columns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("reading row %d of %s: %w", read+1, table.name, err)
		}
		read++
		converted := make([]any, len(values))
		for index, value := range values {
			converted[index], err = convertStandaloneValue(table.columns[index], table.kinds[index], value)
			if err != nil {
				return nil, fmt.Errorf("%s row %s=%v: %w", table.name, table.columns[0], values[0], err)
			}
		}
		if table.derive != nil {
			derived, err := table.derive(converted)
			if err != nil {
				return nil, fmt.Errorf("%s row %s=%v: %w", table.name, table.columns[0], values[0], err)
			}
			converted = append(converted, derived...)
		}
		return converted, nil
	}
	destination := append(append([]string{}, table.columns...), table.derivedColumns...)
	copied, err := tx.CopyFrom(ctx, pgx.Identifier{table.name}, destination, &sqliteRowsSource{rows: rows, next: next})
	if err != nil {
		return copied, fmt.Errorf("copying %s: %w", table.name, err)
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("reading %s: %w", table.name, err)
	}
	return copied, nil
}

// sqliteRowsSource feeds COPY from an open SQLite read.
type sqliteRowsSource struct {
	rows   *sql.Rows
	next   func() ([]any, error)
	values []any
	err    error
}

func (s *sqliteRowsSource) Next() bool {
	if s.err != nil || !s.rows.Next() {
		return false
	}
	s.values, s.err = s.next()
	return s.err == nil
}

func (s *sqliteRowsSource) Values() ([]any, error) { return s.values, s.err }

func (s *sqliteRowsSource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

func joinColumns(columns []string) string {
	out := ""
	for index, column := range columns {
		if index > 0 {
			out += ", "
		}
		out += column
	}
	return out
}

// standaloneTimeLayout is how SQLite's CURRENT_TIMESTAMP writes a time, in UTC.
const standaloneTimeLayout = "2006-01-02 15:04:05"

func convertStandaloneValue(column string, kind valueKind, value any) (any, error) {
	switch kind {
	case integerValue:
		return standalonesqlite.Integer(column, value)
	case textValue:
		text, err := standalonesqlite.Text(column, value)
		if err != nil {
			return nil, err
		}
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("%s is not UTF-8", column)
		}
		return text, nil
	case eventBodyValue:
		text, err := standalonesqlite.Text(column, value)
		if err != nil {
			return nil, err
		}
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("%s is not UTF-8", column)
		}
		if !json.Valid([]byte(text)) {
			return nil, fmt.Errorf("%s is not JSON", column)
		}
		return text, nil
	case booleanValue:
		return standalonesqlite.Boolean(column, value)
	case nullableRealValue:
		switch number := value.(type) {
		case nil:
			return nil, nil
		case float64:
			return number, nil
		case int64:
			return float64(number), nil
		}
		return nil, fmt.Errorf("%s is %T %v, not a number", column, value, value)
	case timeValue, nullableTimeValue:
		if value == nil {
			if kind == nullableTimeValue {
				return nil, nil
			}
			return nil, fmt.Errorf("%s is NULL", column)
		}
		switch moment := value.(type) {
		case time.Time:
			return moment.UTC(), nil
		case string:
			parsed, err := time.ParseInLocation(standaloneTimeLayout, moment, time.UTC)
			if err != nil {
				return nil, fmt.Errorf("%s is %q, not a time written as %s: %w", column, moment, standaloneTimeLayout, err)
			}
			return parsed, nil
		}
		return nil, fmt.Errorf("%s is %T %v, not a time", column, value, value)
	}
	return nil, fmt.Errorf("%s: no conversion for kind %d", column, kind)
}

// eventColumnsOfImportedBody reads an imported event's message ids and tool
// use id from its body (the fourth column), as the writer stores them.
func eventColumnsOfImportedBody(converted []any) ([]any, error) {
	columns, err := EventFieldsOfBody([]byte(converted[3].(string)))
	if err != nil {
		return nil, fmt.Errorf("read the message ids from data: %w", err)
	}
	return []any{columns.MessageID, columns.HarnessMessageID, columns.ToolUseID}, nil
}
