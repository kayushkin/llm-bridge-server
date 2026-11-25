package rulestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// ImportCounts says how many rows ImportStandaloneDatabase copied, by table.
type ImportCounts struct {
	Rules       int64
	Revisions   int64
	Activations int64
	Subjects    int64
	Runs        int64
}

// standaloneTables are the standalone rule-store's tables, in the order an
// import copies them (each after the ones it refers to), with their columns.
var standaloneTables = []struct {
	name    string
	columns []string
	orderBy string
}{
	{"rules", []string{"id", "seq", "name", "description", "source", "state", "active_revision_id", "mode", "created_by", "created_at", "updated_at"}, "seq"},
	{"rule_revisions", []string{"id", "seq", "rule_id", "revision", "definition", "note", "created_by", "created_at"}, "seq"},
	{"rule_activations", []string{"id", "seq", "rule_id", "action", "revision_id", "mode", "acted_by", "reason", "acted_at"}, "seq"},
	{"subjects", []string{"subject_type", "subject_id", "state", "state_episode", "state_observed_at", "snapshot", "observed_at"}, "subject_type, subject_id"},
	{"rule_runs", []string{"id", "seq", "rule_id", "rule_revision_id", "subject_type", "subject_id", "trigger_key", "mode", "cause", "facts", "planned_effects", "outcome", "decided_at"}, "seq"},
}

// ImportStandaloneDatabase copies a standalone rule-store's SQLite file (a
// copy made with sqlite3 .backup) into the module behind pool: every rule,
// revision, activation, subject copy and run, with its id and number, in one
// transaction. Numbering goes on after the highest number of each table. It
// refuses a module that already holds a row.
func ImportStandaloneDatabase(ctx context.Context, pool *sql.DB, path string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(path)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	tableNames := make([]string, 0, len(standaloneTables))
	for _, table := range standaloneTables {
		if err := standalonesqlite.CheckColumns(ctx, source, "rule-store", table.name, table.columns); err != nil {
			return counts, err
		}
		tableNames = append(tableNames, table.name)
	}

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return counts, err
	}
	defer tx.Rollback()
	if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "rule-store", tableNames...); err != nil {
		return counts, err
	}
	copied := map[string]*int64{"rules": &counts.Rules, "rule_revisions": &counts.Revisions, "rule_activations": &counts.Activations,
		"subjects": &counts.Subjects, "rule_runs": &counts.Runs}
	for _, table := range standaloneTables {
		placeholders := make([]string, len(table.columns))
		for index := range placeholders {
			placeholders[index] = fmt.Sprintf("$%d", index+1)
		}
		count, err := standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name:    table.name,
			Columns: table.columns,
			OrderBy: table.orderBy,
			Insert:  "INSERT INTO " + table.name + " (" + strings.Join(table.columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")",
		})
		if err != nil {
			return counts, err
		}
		*copied[table.name] = count
		if table.orderBy != "seq" {
			continue
		}
		var lastNumber sql.NullInt64
		if err := source.QueryRowContext(ctx, "SELECT MAX(seq) FROM "+table.name).Scan(&lastNumber); err != nil {
			return counts, fmt.Errorf("reading the highest number of %s: %w", table.name, err)
		}
		if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table.name, "seq", lastNumber.Int64); err != nil {
			return counts, err
		}
	}
	return counts, tx.Commit()
}
