package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The import fills the columns the single copy's readers select by from each
// body, as the writer fills them for a new event, so a session imported at
// landing maps its message ids and finds its wakeups like one written since.
func TestImportFillsTheEventFieldColumnsFromEachBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range standaloneTables {
		columns := strings.Join(table.columns, ", ")
		if table.name == "events" {
			// AUTOINCREMENT, as the standalone store made it, so the file has
			// the counter the import numbers on from.
			columns = strings.Replace(columns, "id, ", "id INTEGER PRIMARY KEY AUTOINCREMENT, ", 1)
		}
		if _, err := source.Exec(`CREATE TABLE ` + table.name + ` (` + columns + `)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Exec(`CREATE TABLE projection_backfills (` + strings.Join(projectionBackfillsColumns, ", ") + `)`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO projection_backfills (name, through_event_id, completed_at) VALUES
		('session_token_usage', 0, '2026-11-01 00:00:00'), ('session_usage_by_hour', 0, '2026-11-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO events (id, session_id, type, data, created_at) VALUES
		(7, 'br_imported', 'tool_call', '{"type":"tool_call","message_id":"message-1","harness_message_id":"harness-1","tool_call":{"tool_id":"tool-1","name":"ScheduleWakeup"}}', '2026-11-01 00:00:00'),
		(8, 'br_imported', 'system', '{"type":"system"}', '2026-11-01 00:00:01')`); err != nil {
		t.Fatal(err)
	}
	source.Close()

	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	writer := database.Open(module)
	if _, err := ImportStandaloneDatabase(context.Background(), writer, path); err != nil {
		t.Fatal(err)
	}
	rows, err := writer.Query(`SELECT id, message_id, harness_message_id, tool_use_id, tool_name FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int64
		var messageID, harnessMessageID, toolUseID, toolName string
		if err := rows.Scan(&id, &messageID, &harnessMessageID, &toolUseID, &toolName); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{messageID, harnessMessageID, toolUseID, toolName}, "|"))
	}
	want := []string{"message-1|harness-1|tool-1|ScheduleWakeup", "|||"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("imported columns %q; want %q", got, want)
	}
}
