package modelstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneTables are the tables of the standalone store's SQLite file that
// the import copies, each with the columns it must have and how its rows go
// into the module. Order matters: a row is copied after the rows it points at.
var standaloneTables = []standalonesqlite.Table{
	{
		Name:    "providers",
		Columns: []string{"id", "name", "base_url", "auth_type", "rowid"},
		OrderBy: "rowid",
		Insert:  `INSERT INTO providers (id, name, base_url, auth_type, listed_order) VALUES ($1, $2, $3, $4, $5)`,
	},
	{
		Name:    "models",
		Columns: []string{"id", "provider", "name", "short_name", "max_tokens", "input_cost", "output_cost", "enabled", "priority"},
		OrderBy: "id",
		Insert: `INSERT INTO models (id, provider, name, short_name, max_tokens, input_cost, output_cost, enabled, priority)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		Convert: func(row []any) ([]any, error) {
			enabled, err := standalonesqlite.Boolean("enabled", row[7])
			if err != nil {
				return nil, err
			}
			row[7] = enabled
			return row, nil
		},
	},
	{
		Name:    "model_aliases",
		Columns: []string{"alias", "model_id", "rowid"},
		OrderBy: "rowid",
		Insert:  `INSERT INTO model_aliases (alias, model_id, listed_order) VALUES ($1, $2, $3)`,
	},
	{
		Name:    "model_roles",
		Columns: []string{"role", "model_id"},
		OrderBy: "role",
		Insert:  `INSERT INTO model_roles (role, model_id) VALUES ($1, $2)`,
	},
	{
		Name:    "model_role_fallbacks",
		Columns: []string{"role", "position", "model_id"},
		OrderBy: "role, position",
		Insert:  `INSERT INTO model_role_fallbacks (role, position, model_id) VALUES ($1, $2, $3)`,
	},
	{
		Name:    "model_health",
		Columns: []string{"model", "last_success_at", "last_success_ms", "avg_response_ms", "last_error_at", "last_error", "consecutive_errors"},
		OrderBy: "model",
		Insert: `INSERT INTO model_health (model, last_success_at, last_success_ms, avg_response_ms, last_error_at, last_error, consecutive_errors)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
	},
	{
		Name:    "usage",
		Columns: []string{"date", "agent", "orchestrator", "model", "provider", "input_tokens", "output_tokens", "requests", "cost_usd"},
		OrderBy: "date, agent, orchestrator, model",
		Insert: `INSERT INTO usage (date, agent, orchestrator, model, provider, input_tokens, output_tokens, requests, cost_usd)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
	},
}

// tablesLeftInTheFile are tables a standalone file may hold that the import
// does not copy, and why. Both held credentials before auth-store became the
// one place credentials live (model-store 76cc62c removed the code that read
// them), so copying them would put API keys and tokens in a second database
// that nothing reads. They stay in the SQLite file, and the counts say how many
// rows were left there.
var tablesLeftInTheFile = []string{"credentials", "model_credentials"}

// ImportCounts says how many rows of each table an import copied, and how many
// rows of each table it left in the file.
type ImportCounts struct {
	Copied     map[string]int64 `json:"copied"`
	LeftInFile map[string]int64 `json:"left_in_file"`
}

// ImportStandaloneDatabase copies the registry from the SQLite file of a
// standalone model-store into the module behind db: providers, models,
// aliases, roles with their fallbacks, health and the old usage rows, every
// value as it is. Providers and aliases keep the order the file listed them in.
// It runs in one transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, a file with a table it would
// copy whose columns are not exactly the ones it knows, a table in the file it
// neither copies nor knows to leave, an enabled flag that is not 0 or 1, and a
// row the schema refuses: a model of a provider, or an alias, role or fallback
// of a model, the file does not hold (the standalone store did not enforce its
// foreign keys; this module does).
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	counts := ImportCounts{Copied: map[string]int64{}, LeftInFile: map[string]int64{}}
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	known := map[string]bool{}
	for _, table := range standaloneTables {
		known[table.Name] = true
	}
	for _, table := range tablesLeftInTheFile {
		known[table] = true
	}
	tables, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return counts, fmt.Errorf("listing the tables of %s: %w", sqlitePath, err)
	}
	var unknown []string
	present := map[string]bool{}
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return counts, err
		}
		present[name] = true
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if err := tables.Err(); err != nil {
		tables.Close()
		return counts, err
	}
	tables.Close()
	if len(unknown) > 0 {
		return counts, fmt.Errorf("%s holds tables the import does not know, so it would drop their rows: %v", sqlitePath, unknown)
	}
	// The tables to copy, as this file holds them. A file the store made after
	// its credential code went has providers without base_url and auth_type,
	// and no usage table; those columns take the module's defaults.
	var toCopy []standalonesqlite.Table
	for _, table := range standaloneTables {
		if table.Name == "usage" && !present["usage"] {
			continue
		}
		columns := table.Columns
		if columns[len(columns)-1] == "rowid" {
			columns = columns[:len(columns)-1]
		}
		if table.Name == "providers" {
			if err := standalonesqlite.CheckColumns(ctx, source, "model-store", "providers", []string{"id", "name"}); err == nil {
				table.Columns = []string{"id", "name", "NULL", "'api_key'", "rowid"}
				columns = nil
			}
		}
		if columns != nil {
			if err := standalonesqlite.CheckColumns(ctx, source, "model-store", table.Name, columns); err != nil {
				return counts, err
			}
		}
		toCopy = append(toCopy, table)
	}
	for _, table := range tablesLeftInTheFile {
		if !present[table] {
			continue
		}
		var held int64
		if err := source.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
			return counts, fmt.Errorf("counting %s: %w", table, err)
		}
		counts.LeftInFile[table] = held
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		names := make([]string, 0, len(standaloneTables))
		for _, table := range standaloneTables {
			names = append(names, table.Name)
		}
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "models", names...); err != nil {
			return err
		}
		for _, table := range toCopy {
			copied, err := standalonesqlite.Copy(ctx, source, tx, table)
			if err != nil {
				return err
			}
			counts.Copied[table.Name] = copied
		}
		for _, table := range []string{"providers", "model_aliases"} {
			var last int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(listed_order), 0) FROM "+table).Scan(&last); err != nil {
				return err
			}
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table, "listed_order", last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}
