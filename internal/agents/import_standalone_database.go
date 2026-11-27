package agents

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTable is one table of the standalone store's SQLite file, as its
// own start-up migrations left it, and how it is copied.
type standaloneTable struct {
	name    string
	columns []string
	// orderBy orders the read, so a failure names the same row each time.
	orderBy string
	// identity is the integer id column the module numbers by identity, ""
	// for a table keyed otherwise.
	identity string
	// optional tables are missing from a file that never needed them; every
	// other table must be there.
	optional bool
}

// standaloneTables are the tables the import copies, in an order that puts
// every row after the rows its foreign keys name. The columns are those of a
// standalone agent-store file once its start-up migrations had run; the
// import refuses a table with any other columns, because a column it does not
// know is data it would drop and a missing one is a file the standalone store
// never finished migrating.
var standaloneTables = []standaloneTable{
	{name: "agents", identity: "id", orderBy: "id", columns: []string{"id", "slug", "display_name", "emoji", "projects", "description", "role", "enabled", "created_at", "updated_at", "workspace", "parent_agent_id"}},
	{name: "harness", orderBy: "id", columns: []string{"id", "display_name", "default_agent_id", "config_path", "api_endpoint", "created_at", "updated_at", "emoji"}},
	{name: "agent_harness", orderBy: "agent_id, harness_id", columns: []string{"agent_id", "harness_id", "harness_agent_id", "enabled", "model_primary", "model_fallbacks", "workspace_path", "created_at", "updated_at", "thinking_budget", "context_budget", "context_tags", "max_turns", "max_input_tokens", "max_response_time", "system_prompt", "project", "shelved", "is_default", "subagent_allow"}},
	{name: "agent_harness_tools", orderBy: "agent_id, harness_id, tool_name", columns: []string{"agent_id", "harness_id", "tool_name"}},
	{name: "agent_skills", orderBy: "agent_id, skill_id", columns: []string{"agent_id", "skill_id"}},
	{name: "agent_system_prompt_refs", orderBy: "harness_id, host_agent_id, referenced_agent_id, prompt_location", columns: []string{"harness_id", "host_agent_id", "referenced_agent_id", "prompt_location", "created_at"}},
	{name: "agent_nature", identity: "id", orderBy: "id", columns: []string{"id", "agent_id", "kind", "content", "content_hash", "priority", "source_path", "created_at", "updated_at"}},
	{name: "agent_name_aliases", orderBy: "agent_id, alias", columns: []string{"agent_id", "alias", "context", "created_at"}},
	{name: "file_distributions", identity: "id", orderBy: "id", columns: []string{"id", "agent_id", "harness_id", "file_path", "content_hash", "distributed_at", "source_nature_ids"}},
	{name: "file_scans", identity: "id", orderBy: "id", columns: []string{"id", "file_distribution_id", "scanned_at", "current_hash", "status", "diff_summary", "ingested"}},
	{name: "projects", identity: "id", orderBy: "id", columns: []string{"id", "slug", "name", "path", "description", "created_at", "updated_at"}},
	{name: "project_nature", orderBy: "project_id, nature_id", columns: []string{"project_id", "nature_id", "priority"}},
	{name: "agent_status", orderBy: "agent_id, harness_id", columns: []string{"agent_id", "harness_id", "status", "task", "session_id", "started_at", "updated_at"}},
	{name: "tracked_file_ignore_rules", identity: "id", orderBy: "id", columns: []string{"id", "kind", "path_pattern", "reason", "enabled", "created_at"}},
	{name: "tracked_files", identity: "id", orderBy: "id", columns: []string{"id", "path", "scope", "agent_slug", "enabled", "fs_hash", "size", "mtime", "last_scanned_at", "status", "created_at", "updated_at", "ignored_by_rule_id"}},
	{name: "tracked_file_versions", identity: "id", orderBy: "id", columns: []string{"id", "tracked_file_id", "sha256", "size", "content_bytes", "source", "machine_id", "note", "created_at"}},
	{name: "machine_seed_profiles", orderBy: "machine_id", columns: []string{"machine_id", "scopes", "created_at", "updated_at"}},
	{name: "machine_seed_state", orderBy: "machine_id, tracked_file_id", columns: []string{"machine_id", "tracked_file_id", "observed_sha", "observed_at", "last_pushed_version_id", "last_pushed_at"}},
	{name: "prompt_collections", identity: "id", orderBy: "id", columns: []string{"id", "slug", "title", "scope", "root_path", "description", "created_at", "updated_at", "retired_at"}},
	{name: "prompt_sections_legacy_per_file", identity: "id", orderBy: "id", optional: true, columns: []string{"id", "collection_id", "title", "heading", "body", "applies_to", "priority", "enabled", "source_path", "created_at", "updated_at"}},
	{name: "prompt_sections", identity: "id", orderBy: "id", columns: []string{"id", "collection_id", "title", "heading", "body", "tags", "position", "enabled", "created_at", "updated_at"}},
	{name: "prompt_section_revisions", identity: "id", orderBy: "id", columns: []string{"id", "section_id", "collection_id", "operation", "title", "heading", "body", "tags", "position", "enabled", "source", "drift_id", "note", "created_at"}},
	{name: "prompt_collection_outputs", identity: "id", orderBy: "id", columns: []string{"id", "collection_id", "relative_path", "enabled", "accounted_sha256", "accounted_at", "created_at", "updated_at"}},
	{name: "prompt_harness_deliveries", orderBy: "harness", columns: []string{"harness", "delivery", "native_relative_path", "note", "updated_at"}},
	{name: "prompt_drifts", identity: "id", orderBy: "id", columns: []string{"id", "collection_id", "output_id", "accounted_sha256", "disk_sha256", "disk_content", "status", "held_reason", "operations", "annotation", "created_at", "resolved_at"}},
}

// leftBehindTables are tables an older standalone store made and that the
// module has no copy of: the pre-2026-05-11 names of harness, agent_harness
// and agent_harness_tools, which the standalone store's rename migration left
// in place, empty, beside the renamed tables, and a memories table from before
// memory-store was a store of its own. Nothing reads them. The import copies
// a file only while every one of them is empty or absent, so it drops no row.
var leftBehindTables = []string{"orchestrators", "agent_orchestrators", "agent_tools", "memories", "memory_tags"}

// ImportCounts says how many rows of each table an import copied, and the
// number each identity column carries on from.
type ImportCounts struct {
	Rows map[string]int64 `json:"rows"`
	// NumberingContinuesAfter is, per table with an identity id, the highest
	// number the standalone store had handed out (its sqlite_sequence entry,
	// or the highest id copied when that is higher). The module's next id is
	// one more, so a deleted id is never handed out again, as AUTOINCREMENT
	// promised.
	NumberingContinuesAfter map[string]int64 `json:"numbering_continues_after"`
}

// ImportStandaloneDatabase copies every row of the SQLite file of a
// standalone agent-store into the module behind db, as it is: every id, every
// time, every flag and every byte of file content, prompt text and JSON. It
// runs in one transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, so it can never mix two
// stores or run twice; a file with a table it does not know the columns of,
// or that lacks a table it needs; a file whose left-behind tables hold a row;
// and a row the schema refuses (a foreign key the file does not satisfy, a
// NUL byte in text). Afterwards each identity column numbers on from the
// standalone store's own high-water mark.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	counts := ImportCounts{Rows: map[string]int64{}, NumberingContinuesAfter: map[string]int64{}}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	present, err := standaloneTableNames(ctx, source)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	if err := refuseUnknownTables(present); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	var copied []standaloneTable
	for _, table := range standaloneTables {
		if table.optional && !present[table.name] {
			continue
		}
		if err := standalonesqlite.CheckColumns(ctx, source, "agent-store", table.name, table.columns); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
		copied = append(copied, table)
	}
	for _, table := range leftBehindTables {
		if !present[table] {
			continue
		}
		var held int64
		if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&held); err != nil {
			return counts, fmt.Errorf("%s: counting %s: %w", sqlitePath, table, err)
		}
		if held > 0 {
			return counts, fmt.Errorf("%s: table %s holds %d rows, and the module has no copy of that table; the import drops no row", sqlitePath, table, held)
		}
	}
	highWaterMarks, err := standaloneSequences(ctx, source, present)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		names := make([]string, len(standaloneTables))
		for index, table := range standaloneTables {
			names[index] = table.name
		}
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "agents", names...); err != nil {
			return err
		}
		for _, table := range copied {
			copiedRows, err := copyStandaloneTable(ctx, source, tx, table)
			if err != nil {
				return err
			}
			counts.Rows[table.name] = copiedRows
			if table.identity == "" {
				continue
			}
			var highestID sql.NullInt64
			if err := source.QueryRowContext(ctx, `SELECT MAX(`+table.identity+`) FROM "`+table.name+`"`).Scan(&highestID); err != nil {
				return fmt.Errorf("reading the highest id of %s: %w", table.name, err)
			}
			last := highWaterMarks[table.name]
			if highestID.Int64 > last {
				last = highestID.Int64
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table.name, table.identity, last); err != nil {
				return err
			}
			counts.NumberingContinuesAfter[table.name] = last
		}
		// agents names its parent in its own table, so its parents were
		// copied as NULL and are set now that every agent is there.
		return copyAgentParents(ctx, source, tx)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// copyStandaloneTable copies one table row by row. agents.parent_agent_id is
// copied as NULL, because a parent may come after its child; copyAgentParents
// sets it afterwards.
func copyStandaloneTable(ctx context.Context, source *sql.DB, tx *sql.Tx, table standaloneTable) (int64, error) {
	placeholders := make([]string, len(table.columns))
	for index := range table.columns {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	quoted := make([]string, len(table.columns))
	for index, column := range table.columns {
		quoted[index] = `"` + column + `"`
	}
	copySpec := standalonesqlite.Table{
		Name:    `"` + table.name + `"`,
		Columns: quoted,
		OrderBy: table.orderBy,
		Insert:  `INSERT INTO ` + table.name + ` (` + strings.Join(quoted, ", ") + `) VALUES (` + strings.Join(placeholders, ", ") + `)`,
	}
	if table.name == "agents" {
		parentColumn := indexOf(table.columns, "parent_agent_id")
		copySpec.Convert = func(row []any) ([]any, error) {
			converted := append([]any{}, row...)
			converted[parentColumn] = nil
			return converted, nil
		}
	}
	copied, err := standalonesqlite.Copy(ctx, source, tx, copySpec)
	if err != nil {
		return copied, err
	}
	return copied, nil
}

func copyAgentParents(ctx context.Context, source *sql.DB, tx *sql.Tx) error {
	rows, err := source.QueryContext(ctx, `SELECT id, parent_agent_id FROM agents WHERE parent_agent_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading the parents of agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent int64
		if err := rows.Scan(&id, &parent); err != nil {
			return fmt.Errorf("reading the parents of agents: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET parent_agent_id=$1 WHERE id=$2`, parent, id); err != nil {
			return fmt.Errorf("setting the parent of agent %d to %d: %w", id, parent, err)
		}
	}
	return rows.Err()
}

func indexOf(values []string, value string) int {
	for index, candidate := range values {
		if candidate == value {
			return index
		}
	}
	panic("agentstore: no column " + value)
}

// standaloneTableNames lists the file's tables.
func standaloneTableNames(ctx context.Context, source *sql.DB) (map[string]bool, error) {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, fmt.Errorf("listing the tables: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		present[name] = true
	}
	return present, rows.Err()
}

// refuseUnknownTables refuses a file holding a table that is neither copied
// nor known to be left behind: its rows would be lost without anyone saying so.
func refuseUnknownTables(present map[string]bool) error {
	known := map[string]bool{"sqlite_sequence": true}
	for _, table := range standaloneTables {
		known[table.name] = true
	}
	for _, table := range leftBehindTables {
		known[table] = true
	}
	var unknown []string
	for name := range present {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("it holds tables the import does not know (%s); it copies only a file it can copy whole", strings.Join(unknown, ", "))
	}
	return nil
}

// standaloneSequences reads sqlite_sequence, the highest id AUTOINCREMENT
// handed out per table, deleted rows included.
func standaloneSequences(ctx context.Context, source *sql.DB, present map[string]bool) (map[string]int64, error) {
	marks := map[string]int64{}
	if !present["sqlite_sequence"] {
		return marks, nil
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
		marks[name] = seq
	}
	return marks, rows.Err()
}
