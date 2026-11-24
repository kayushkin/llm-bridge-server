package toolstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneToolColumns and standaloneInstanceToolColumns are the columns of
// the SQLite database the standalone tool-store kept, as its last schema made
// them. The import refuses a file whose tables have any other columns: a
// column it does not know is data it would drop, and a column it misses is a
// database the standalone store never finished migrating.
var (
	standaloneToolColumns = []string{"id", "name", "display_name", "description", "kind",
		"input_schema", "env_keys", "credentials", "tags",
		"mcp_transport", "mcp_command", "mcp_args", "mcp_url",
		"cli_command", "cli_args_template", "cli_working_dir", "cli_timeout_ms",
		"local_symbol", "harness", "harness_tool_name", "last_seen_at",
		"enabled", "created_at", "updated_at"}
	standaloneInstanceToolColumns = []string{"instance_id", "tool_id", "created_at"}
)

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	Tools         int   `json:"tools"`
	InstanceTools int   `json:"instance_tools"`
	LastToolID    int64 `json:"last_tool_id"`
}

// ImportStandaloneDatabase copies every tool and every instance opt-in from
// the SQLite file of a standalone tool-store into the module behind db, as
// they are: ids, names, specs, tags, enabled flags and times. Tool ids are
// kept, because grant-store and bundle-store hold them. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// registries or run twice, and it refuses a file it cannot copy whole: a table
// with a column it does not know or lacks, an enabled flag that is not 0 or 1,
// or an opt-in naming a tool the file does not hold. Afterwards tools number
// on from the highest id copied, so no id another store holds is handed out
// again, even one whose row was deleted before the copy.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	if err := checkStandaloneColumns(ctx, source, "tools", standaloneToolColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	if err := checkStandaloneColumns(ctx, source, "instance_tools", standaloneInstanceToolColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	// SQLite's AUTOINCREMENT never hands out a number twice, and remembers
	// the highest it gave in sqlite_sequence even after that row is deleted.
	// The module must number on from there, not from the highest row left.
	lastHandedOut, err := standaloneLastToolID(ctx, source)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"tools", "instance_tools"} {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return fmt.Errorf("the tools module already holds %d rows in %s; the import copies into an empty module only", held, table)
			}
		}
		var err error
		if counts.Tools, counts.LastToolID, err = importTools(ctx, source, tx); err != nil {
			return err
		}
		if counts.InstanceTools, err = importInstanceTools(ctx, source, tx); err != nil {
			return err
		}
		if lastHandedOut > counts.LastToolID {
			counts.LastToolID = lastHandedOut
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "tools", "id", counts.LastToolID)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("it has no table %s, so it is not a tool-store database", table)
	}
	has := map[string]bool{}
	for _, name := range found {
		has[name] = true
	}
	if table == "tools" {
		switch {
		case !has["harness"]:
			return fmt.Errorf("its tools table was made before the harness kind; start the standalone tool-store on it once, which rebuilds the table, and import the result")
		case !has["last_seen_at"]:
			return fmt.Errorf("its tools table was made before last_seen_at; start the standalone tool-store on it once, which adds the column, and import the result")
		}
	}
	sortedWant := append([]string{}, want...)
	sort.Strings(sortedWant)
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %s, and the import knows %s; it copies only a table it can copy whole",
			table, strings.Join(found, ", "), strings.Join(sortedWant, ", "))
	}
	return nil
}

// standaloneLastToolID is the highest tool id the standalone store ever handed
// out, from sqlite_sequence; 0 when it handed out none.
func standaloneLastToolID(ctx context.Context, source *sql.DB) (int64, error) {
	var sequenceTables int
	if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&sequenceTables); err != nil {
		return 0, fmt.Errorf("looking for sqlite_sequence: %w", err)
	}
	if sequenceTables == 0 {
		return 0, nil
	}
	var last sql.NullInt64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'tools'`).Scan(&last)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the last tool id from sqlite_sequence: %w", err)
	}
	return last.Int64, nil
}

func importTools(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, lastID int64, err error) {
	rows, err := source.QueryContext(ctx, `SELECT `+strings.Join(standaloneToolColumns, ", ")+` FROM tools ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("reading tools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, cliTimeoutMs, lastSeenAt, enabled, createdAt, updatedAt int64
			name, displayName, description, kind                        string
			inputSchema, envKeys, credentials, tags                     string
			mcpTransport, mcpCommand, mcpArgs, mcpURL                   string
			cliCommand, cliArgsTemplate, cliWorkingDir                  string
			localSymbol, harness, harnessToolName                       string
		)
		if err := rows.Scan(&id, &name, &displayName, &description, &kind,
			&inputSchema, &envKeys, &credentials, &tags,
			&mcpTransport, &mcpCommand, &mcpArgs, &mcpURL,
			&cliCommand, &cliArgsTemplate, &cliWorkingDir, &cliTimeoutMs,
			&localSymbol, &harness, &harnessToolName, &lastSeenAt,
			&enabled, &createdAt, &updatedAt); err != nil {
			return 0, 0, fmt.Errorf("reading tools: %w", err)
		}
		if enabled != 0 && enabled != 1 {
			return 0, 0, fmt.Errorf("tool %d (%s) has enabled %d, which is neither 0 nor 1", id, name, enabled)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tools (`+strings.Join(standaloneToolColumns, ", ")+`)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
			id, name, displayName, description, kind,
			inputSchema, envKeys, credentials, tags,
			mcpTransport, mcpCommand, mcpArgs, mcpURL,
			cliCommand, cliArgsTemplate, cliWorkingDir, cliTimeoutMs,
			localSymbol, harness, harnessToolName, lastSeenAt,
			enabled == 1, createdAt, updatedAt); err != nil {
			return 0, 0, fmt.Errorf("copying tool %d (%s): %w", id, name, err)
		}
		copied++
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading tools: %w", err)
	}
	return copied, lastID, nil
}

func importInstanceTools(ctx context.Context, source *sql.DB, tx *sql.Tx) (copied int, err error) {
	rows, err := source.QueryContext(ctx, `SELECT instance_id, tool_id, created_at FROM instance_tools ORDER BY instance_id, tool_id`)
	if err != nil {
		return 0, fmt.Errorf("reading instance_tools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var instanceID string
		var toolID, createdAt int64
		if err := rows.Scan(&instanceID, &toolID, &createdAt); err != nil {
			return 0, fmt.Errorf("reading instance_tools: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO instance_tools (instance_id, tool_id, created_at) VALUES ($1, $2, $3)`,
			instanceID, toolID, createdAt); err != nil {
			return 0, fmt.Errorf("copying the opt-in of instance %s to tool %d: %w", instanceID, toolID, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading instance_tools: %w", err)
	}
	return copied, nil
}
