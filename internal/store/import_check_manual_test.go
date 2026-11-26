package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// TestImportedBridgeDatabaseHoldsEveryRowOfTheFile compares, row by row and
// value by value, a bridge.db with the bridge core module it was imported
// into. It is for landing day and runs only when both are named:
//
//	BRIDGE_IMPORT_CHECK_SQLITE=<the file imported>
//	BRIDGE_IMPORT_CHECK_POSTGRES_URL=<the Postgres it was imported into>
//
// A time is compared to the microsecond, the precision Postgres keeps (the
// import truncates the nanoseconds a Go time has), and
// text that was not valid UTF-8 as the import made it. It prints counts only,
// never a value.
func TestImportedBridgeDatabaseHoldsEveryRowOfTheFile(t *testing.T) {
	sqlitePath, postgresURL := os.Getenv("BRIDGE_IMPORT_CHECK_SQLITE"), os.Getenv("BRIDGE_IMPORT_CHECK_POSTGRES_URL")
	if sqlitePath == "" || postgresURL == "" {
		t.Skip("set BRIDGE_IMPORT_CHECK_SQLITE and BRIDGE_IMPORT_CHECK_POSTGRES_URL to check an import")
	}
	ctx := context.Background()
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	imported, err := postgresstorage.Open(ctx, postgresURL, module, postgresstorage.ScanTimestampsInUTC)
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	fileTables, err := sqliteTables(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range importedTables {
		if !slices.Contains(fileTables, table) {
			continue
		}
		fileColumns, err := sqliteColumnsOf(ctx, source, table)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for _, column := range fileColumns {
			if !slices.Contains(retiredEmptyColumns[table], column) {
				columns = append(columns, column)
			}
		}
		key := keyColumnsOf(table)
		order := strings.Join(key, ", ")
		fileRows := readNormalizedRows(t, source, `SELECT `+strings.Join(columns, ", ")+` FROM `+table+` ORDER BY `+order+` COLLATE BINARY`)
		moduleRows := readNormalizedRows(t, imported, `SELECT `+strings.Join(columns, ", ")+` FROM `+table+` ORDER BY `+order)
		if len(fileRows) != len(moduleRows) {
			t.Errorf("%s: the file has %d rows, the module %d", table, len(fileRows), len(moduleRows))
			continue
		}
		differing := map[string]int{}
		for index := range fileRows {
			for column := range columns {
				if fileRows[index][column] != moduleRows[index][column] {
					differing[columns[column]]++
				}
			}
		}
		if len(differing) > 0 {
			t.Errorf("%s: %d rows; values differ in %v (count of rows per column)", table, len(fileRows), differing)
			continue
		}
		fmt.Printf("%-28s %d rows, %d columns, every value equal\n", table+":", len(fileRows), len(columns))
	}
}

// keyColumnsOf is what orders a table's rows the same way in both databases.
func keyColumnsOf(table string) []string {
	switch table {
	case "sessions":
		return []string{"bridge_id"}
	case "folders":
		return []string{"name"}
	case "source_folders":
		return []string{"source"}
	case "session_briefs", "session_peer_origins", "session_starters", "session_posting_spawns", "runner_spawns":
		return []string{"session_id"}
	case "session_files":
		return []string{"file_id"}
	case "service_settings":
		return []string{"key"}
	case "server_identity":
		return []string{"only_row"}
	case "peer_access_levels":
		return []string{"level_key"}
	case "external_activity_marks":
		return []string{"bridge_session_id"}
	case "machine_stats_samples":
		return []string{"machine_id", "collected_at"}
	case "machine_disk_deletions":
		return []string{"number"}
	case "held_messages":
		return []string{"held_message_id"}
	}
	return []string{"id"}
}

// readNormalizedRows reads every row as text, a time as UTC to the
// microsecond and a flag as 0 or 1, so the two databases' values compare.
func readNormalizedRows(t *testing.T, db *sql.DB, query string) [][]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(values))
		for index, value := range values {
			row[index] = normalizedValue(value)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalizedValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case time.Time:
		return typed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case bool:
		if typed {
			return "1"
		}
		return "0"
	case []byte:
		return normalizedValue(string(typed))
	case string:
		text, _, _ := strings.Cut(typed, " m=")
		for _, layout := range sqliteTimeLayouts {
			if parsed, err := time.Parse(layout, text); err == nil && len(text) >= len("2006-01-02 15:04:05") && strings.Count(text, ":") >= 2 && strings.HasPrefix(text, "20") {
				return parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
			}
		}
		return strings.ToValidUTF8(typed, string(utf8.RuneError))
	}
	return fmt.Sprint(value)
}
