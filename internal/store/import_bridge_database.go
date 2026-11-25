package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// importedTables are the tables of a bridge.db the import copies, in the order
// it copies them: a table another points at comes first. Every one is a table
// of this module with the same name. events is not among them: session events
// stay in the archived file, and the session log is their only copy.
var importedTables = []string{
	"sessions", "folders", "source_folders",
	"session_briefs", "session_brief_revisions", "loose_ends",
	"signals", "service_settings", "session_files", "session_actions",
	"chat_links", "chat_link_messages",
	"server_identity",
	"peer_connection_invites", "peer_connections", "peer_grants", "peer_access", "peer_access_levels",
	"peer_user_mappings", "session_peer_origins", "service_principal_tokens", "session_starters",
	"peer_messages", "peer_message_attachments",
	"session_posting_spawns", "runner_spawns",
	"login_sessions", "login_attempts",
	"external_activity_marks", "machine_stats_samples", "machine_disk_deletions",
	"held_messages",
}

// tablesLeftInTheArchive are tables of a bridge.db the import does not copy.
// events stays in the archived file. credential_slots was written by a build
// before 2026-05 and read by nothing since; the import refuses a file whose
// copy of it holds a row, since that row would be lost.
var (
	archivedEventTable     = "events"
	retiredEmptyTables     = []string{"credential_slots"}
	sqliteBookkeepingTable = "sqlite_sequence"
)

// BridgeDatabaseImportCounts says what an import copied.
type BridgeDatabaseImportCounts struct {
	// RowsByTable is how many rows each table received.
	RowsByTable map[string]int64
	// TablesNotInTheFile are tables of this module the file did not have
	// (one written by an older build); they stay empty.
	TablesNotInTheFile []string
	// NumberingContinuesAfter is, for each identity column, the number the
	// next row takes the one after.
	NumberingContinuesAfter map[string]int64
	// HighestArchivedEventID is the highest id the file's events table
	// handed out, recorded so the session log numbers above it.
	HighestArchivedEventID int64
	Took                   time.Duration
}

// postgresColumn is one column of a table of this module, as Postgres holds it.
type postgresColumn struct {
	name     string
	dataType string
	nullable bool
	identity bool
}

// ImportBridgeDatabase copies every table of the bridge's SQLite database at
// sqlitePath (a copy made with sqlite3 .backup, or one whose events table was
// left out) into the module behind db, as they are: ids, numbers, times and
// every column. It runs in one transaction, so it copies everything or
// nothing, and it records the highest id the file's events table handed out
// (its AUTOINCREMENT counter, or the highest id present) without copying an
// event.
//
// It refuses a module that already holds a row, so it can never mix two
// servers' records or run after a server has started on the module; a table
// in the file it does not know; a column of a known table that this module's
// table lacks, whose values it would drop; and a value it cannot store as the
// module's column holds it. Each identity column numbers on from the highest
// number copied, or the file's AUTOINCREMENT counter where that is higher, so
// no id that chats and links already carry is handed out again.
func ImportBridgeDatabase(ctx context.Context, db *sql.DB, sqlitePath string, sourceDescription string) (BridgeDatabaseImportCounts, error) {
	started := time.Now()
	counts := BridgeDatabaseImportCounts{RowsByTable: map[string]int64{}, NumberingContinuesAfter: map[string]int64{}}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	fileTables, err := sqliteTables(ctx, source)
	if err != nil {
		return counts, err
	}
	for _, table := range fileTables {
		switch {
		case slices.Contains(importedTables, table), table == archivedEventTable, table == sqliteBookkeepingTable:
		case slices.Contains(retiredEmptyTables, table):
			var held int64
			if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&held); err != nil {
				return counts, fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return counts, fmt.Errorf("table %s holds %d rows, and nothing reads it any more; the import copies only what this module keeps, so it will not drop them", table, held)
			}
		default:
			return counts, fmt.Errorf("the file has a table %s the import does not know; it copies only a database it can copy whole", table)
		}
	}
	if !slices.Contains(fileTables, "sessions") {
		return counts, fmt.Errorf("it has no table sessions, so it is not a bridge database")
	}
	highestArchivedEventID, err := highestEventIDOfFile(ctx, source, slices.Contains(fileTables, archivedEventTable))
	if err != nil {
		return counts, err
	}
	sqliteCounters, err := sqliteAutoincrementCounters(ctx, source, slices.Contains(fileTables, sqliteBookkeepingTable))
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
			for _, table := range append(append([]string{}, importedTables...), "archived_event_numbering") {
				var held int64
				if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
					return fmt.Errorf("counting %s: %w", table, err)
				}
				if held > 0 {
					return fmt.Errorf("the bridge core module already holds %d rows in %s; the import copies into an empty module only, before the first start", held, table)
				}
			}
			for _, table := range importedTables {
				columns, err := postgresColumnsOf(ctx, tx, table)
				if err != nil {
					return err
				}
				if !slices.Contains(fileTables, table) {
					counts.TablesNotInTheFile = append(counts.TablesNotInTheFile, table)
				} else {
					copied, err := copyBridgeTable(ctx, source, tx, table, columns)
					if err != nil {
						return err
					}
					counts.RowsByTable[table] = copied
				}
				for _, column := range columns {
					if !column.identity {
						continue
					}
					var highestCopied int64
					if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(`+column.name+`), 0) FROM `+table).Scan(&highestCopied); err != nil {
						return fmt.Errorf("reading the highest %s of %s: %w", column.name, table, err)
					}
					last := max(highestCopied, sqliteCounters[table])
					// setval(sequence, n, false) makes the next value n itself.
					if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence($1, $2), $3, false)`, table, column.name, last+1); err != nil {
						return fmt.Errorf("numbering %s.%s on after %d: %w", table, column.name, last, err)
					}
					counts.NumberingContinuesAfter[table+"."+column.name] = last
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO archived_event_numbering (only_row, highest_archived_event_id, recorded_at, source_description) VALUES (1, $1, now(), $2)`,
				highestArchivedEventID, sourceDescription); err != nil {
				return fmt.Errorf("recording the archived events' highest id: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return BridgeDatabaseImportCounts{RowsByTable: map[string]int64{}, NumberingContinuesAfter: map[string]int64{}}, err
	}
	counts.HighestArchivedEventID = highestArchivedEventID
	counts.Took = time.Since(started)
	return counts, nil
}

func sqliteTables(ctx context.Context, source *sql.DB) ([]string, error) {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing the file's tables: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// highestEventIDOfFile is the highest id the file's events table handed out:
// its AUTOINCREMENT counter, or the highest id present if that is higher. A
// file whose events table was left out keeps the counter. A file with neither
// is refused: the session log could then number over ids clients hold.
func highestEventIDOfFile(ctx context.Context, source *sql.DB, hasEventTable bool) (int64, error) {
	var highestPresent int64
	if hasEventTable {
		if err := source.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&highestPresent); err != nil {
			return 0, fmt.Errorf("reading the highest event id: %w", err)
		}
	}
	var counter sql.NullInt64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&counter)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") {
		return 0, fmt.Errorf("reading the events counter: %w", err)
	}
	if !hasEventTable && !counter.Valid {
		return 0, fmt.Errorf("the file has neither an events table nor its counter in sqlite_sequence, so it says nothing of the ids its events had; copy the file with its sqlite_sequence table")
	}
	return max(highestPresent, counter.Int64), nil
}

// sqliteAutoincrementCounters is each table's AUTOINCREMENT counter, which is
// above its highest id when the newest rows were deleted.
func sqliteAutoincrementCounters(ctx context.Context, source *sql.DB, hasBookkeeping bool) (map[string]int64, error) {
	counters := map[string]int64{}
	if !hasBookkeeping {
		return counters, nil
	}
	rows, err := source.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, fmt.Errorf("reading sqlite_sequence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, fmt.Errorf("reading sqlite_sequence: %w", err)
		}
		counters[name] = seq
	}
	return counters, rows.Err()
}

func postgresColumnsOf(ctx context.Context, tx pgx.Tx, table string) ([]postgresColumn, error) {
	rows, err := tx.Query(ctx, `SELECT column_name, data_type, is_nullable = 'YES', is_identity = 'YES'
		FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var columns []postgresColumn
	for rows.Next() {
		var column postgresColumn
		if err := rows.Scan(&column.name, &column.dataType, &column.nullable, &column.identity); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("this module has no table %s", table)
	}
	return columns, nil
}

func sqliteColumnsOf(ctx context.Context, source *sql.DB, table string) ([]string, error) {
	rows, err := source.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("reading the columns of %s in the file: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// copyBridgeTable streams one table of the file into the module with COPY.
// Every column the file has must be a column of the module's table; a column
// only the module has (one a later build added) takes its default.
func copyBridgeTable(ctx context.Context, source *sql.DB, tx pgx.Tx, table string, destination []postgresColumn) (int64, error) {
	fileColumns, err := sqliteColumnsOf(ctx, source, table)
	if err != nil {
		return 0, err
	}
	byName := map[string]postgresColumn{}
	for _, column := range destination {
		byName[column.name] = column
	}
	columns := make([]postgresColumn, len(fileColumns))
	for index, name := range fileColumns {
		column, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("table %s of the file has a column %s this module's table lacks; the import copies only a table it can copy whole", table, name)
		}
		columns[index] = column
	}
	rows, err := source.QueryContext(ctx, `SELECT `+strings.Join(fileColumns, ", ")+` FROM `+table+` ORDER BY rowid`)
	if err != nil && strings.Contains(err.Error(), "no such column: rowid") {
		// A WITHOUT ROWID table orders by its key.
		rows, err = source.QueryContext(ctx, `SELECT `+strings.Join(fileColumns, ", ")+` FROM `+table+` ORDER BY 1, 2`)
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", table, err)
	}
	defer rows.Close()
	var read int64
	next := func() ([]any, error) {
		values := make([]any, len(fileColumns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("reading row %d of %s: %w", read+1, table, err)
		}
		read++
		converted := make([]any, len(values))
		for index, value := range values {
			converted[index], err = convertBridgeValue(columns[index], value)
			if err != nil {
				return nil, fmt.Errorf("%s row %d (%s=%v): %w", table, read, fileColumns[0], values[0], err)
			}
		}
		return converted, nil
	}
	copied, err := tx.CopyFrom(ctx, pgx.Identifier{table}, fileColumns, &bridgeRowsSource{rows: rows, next: next})
	if err != nil {
		return copied, fmt.Errorf("copying %s: %w", table, err)
	}
	if err := rows.Err(); err != nil {
		return copied, fmt.Errorf("reading %s: %w", table, err)
	}
	return copied, nil
}

// bridgeRowsSource feeds COPY from an open SQLite read.
type bridgeRowsSource struct {
	rows   *sql.Rows
	next   func() ([]any, error)
	values []any
	err    error
}

func (s *bridgeRowsSource) Next() bool {
	if s.err != nil || !s.rows.Next() {
		return false
	}
	s.values, s.err = s.next()
	return s.err == nil
}

func (s *bridgeRowsSource) Values() ([]any, error) { return s.values, s.err }

func (s *bridgeRowsSource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}

// sqliteTimeLayouts are the ways the bridge's SQLite database wrote a time:
// Go's time.String (how the driver writes a time.Time), SQLite's
// CURRENT_TIMESTAMP, and RFC 3339.
var sqliteTimeLayouts = []string{sqliteTimeLayout, "2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00"}

// convertBridgeValue turns one SQLite value into what the module's column
// holds, refusing any it cannot store as it is.
func convertBridgeValue(column postgresColumn, value any) (any, error) {
	if value == nil {
		if !column.nullable {
			return nil, fmt.Errorf("%s is NULL, and the module's column takes no NULL", column.name)
		}
		return nil, nil
	}
	switch column.dataType {
	case "text":
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case []byte:
			text = string(typed)
		default:
			return nil, fmt.Errorf("%s is %T %v, not text", column.name, value, value)
		}
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("%s is not UTF-8", column.name)
		}
		if strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("%s holds a NUL character, which Postgres text cannot", column.name)
		}
		return text, nil
	case "bigint":
		return standalonesqlite.Integer(column.name, value)
	case "boolean":
		return standalonesqlite.Boolean(column.name, value)
	case "double precision":
		switch number := value.(type) {
		case float64:
			return number, nil
		case int64:
			return float64(number), nil
		}
		return nil, fmt.Errorf("%s is %T %v, not a number", column.name, value, value)
	case "timestamp with time zone":
		switch moment := value.(type) {
		case time.Time:
			return moment.UTC(), nil
		case string:
			// time.String adds the monotonic clock reading when the time
			// had one; it says nothing of the instant.
			text, _, _ := strings.Cut(moment, " m=")
			for _, layout := range sqliteTimeLayouts {
				if parsed, err := time.Parse(layout, text); err == nil {
					return parsed.UTC(), nil
				}
			}
			return nil, fmt.Errorf("%s is %q, a time in no layout the bridge wrote", column.name, moment)
		}
		return nil, fmt.Errorf("%s is %T %v, not a time", column.name, value, value)
	}
	return nil, fmt.Errorf("%s: the import has no conversion to %s", column.name, column.dataType)
}
