package hookstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

var standaloneHookColumns = []string{"id", "harness", "event", "matcher", "command", "scope_kind", "scope_id", "enabled", "created_at", "updated_at"}

// ImportCounts says how many rows an import copied.
type ImportCounts struct {
	Hooks         int64 `json:"hooks"`
	DisabledHooks int64 `json:"disabled_hooks"`
}

// ImportStandaloneDatabase copies every hook of the SQLite hook-store file
// into the module behind db, as it is: ids, commands, scopes, the enabled
// switch and times, cut to the microsecond Postgres keeps. It runs in one
// transaction, so it copies everything or nothing, and it refuses a module
// that already holds a hook and a file it cannot copy whole.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	var otherTables int64
	if err := source.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name != 'hooks'`).Scan(&otherTables); err != nil {
		return counts, fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	if otherTables > 0 {
		return counts, fmt.Errorf("%s has %d tables besides hooks, which the import does not know; it copies only a file it can copy whole", sqlitePath, otherTables)
	}
	if err := standalonesqlite.CheckColumns(ctx, source, "hook-store", "hooks", standaloneHookColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "hooks", "hooks"); err != nil {
			return err
		}
		var err error
		counts.Hooks, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "hooks", Columns: standaloneHookColumns, OrderBy: "rowid",
			Insert: `INSERT INTO hooks (id, harness, event, matcher, command, scope_kind, scope_id, enabled, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: convertStandaloneHook,
		})
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM hooks WHERE NOT enabled`).Scan(&counts.DisabledHooks)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func convertStandaloneHook(row []any) ([]any, error) {
	values := make([]any, len(row))
	for index, column := range standaloneHookColumns {
		var err error
		switch column {
		case "enabled":
			values[index], err = standalonesqlite.Boolean(column, row[index])
		case "created_at", "updated_at":
			at, isTime := row[index].(time.Time)
			if !isTime {
				return nil, fmt.Errorf("%s is %T %v, not a time", column, row[index], row[index])
			}
			values[index] = at.UTC()
		default:
			values[index], err = standalonesqlite.Text(column, row[index])
		}
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}
