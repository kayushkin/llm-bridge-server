package usage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneColumns are the columns of each table the import copies from the
// SQLite database the standalone usage-store kept. The import refuses a file
// whose tables have any other columns: a column it does not know is data it
// would drop.
var standaloneColumns = map[string][]string{
	"limit_snapshots": {"id", "snapshot_at", "provider", "window_key", "used_percent", "window_minutes", "resets_at",
		"plan_type", "tier", "source", "raw_json"},
	"spend_keys":   {"provider", "api_key_id", "api_key_name", "api_key_hint", "api_key_status", "raw_json", "fetched_at"},
	"spend_daily":  {"provider", "api_key_id", "date", "total_usd", "fetched_at"},
	"spend_topups": {"id", "provider", "amount_usd", "occurred_at", "note", "created_at"},
}

// standaloneOptionalColumns are tables an older standalone store made and a
// newer one may lack: copied when the file has them.
var standaloneOptionalColumns = map[string][]string{
	"spend_snapshots": {"id", "fetched_at", "provider", "window", "period_start", "period_end", "total_usd", "currency",
		"source", "raw_json"},
}

// standaloneUnwrittenTokenUsageTable is the per-request token accounting table
// the standalone store created and never wrote. Nothing here keeps such rows,
// so the import refuses a file in which it holds any rather than drop them.
const standaloneUnwrittenTokenUsageTable = "usage"

// ImportCounts says how many rows of each table an import copied.
type ImportCounts struct {
	LimitSnapshots          int64 `json:"limit_snapshots"`
	LastLimitSnapshotNumber int64 `json:"last_limit_snapshot_number"`
	SpendKeys               int64 `json:"spend_keys"`
	SpendDays               int64 `json:"spend_days"`
	SpendTopups             int64 `json:"spend_topups"`
	LastSpendTopupNumber    int64 `json:"last_spend_topup_number"`
	SpendSnapshots          int64 `json:"spend_snapshots"`
}

// ImportStandaloneDatabase copies every limit snapshot, spend key, daily
// spend, top-up and old spend snapshot from the SQLite file of a standalone
// usage-store into the module behind db, as they are: numbers, times, raw
// provider answers and notes. It runs in one transaction, so it copies
// everything or nothing.
//
// It refuses a module that already holds any row, a file holding a table it
// does not know, a table with columns it does not know, and a file whose
// token-usage table holds rows. Afterwards snapshots and top-ups number on
// from the highest number the standalone store handed out, so a top-up id the
// usage page showed is never handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()

	present, err := standaloneTables(ctx, source)
	if err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	for name := range present {
		_, copied := standaloneColumns[name]
		_, optional := standaloneOptionalColumns[name]
		if copied || optional || name == "sqlite_sequence" || name == standaloneUnwrittenTokenUsageTable {
			continue
		}
		return counts, fmt.Errorf("%s has a table %s, which the import does not know; it copies only a file it can copy whole", sqlitePath, name)
	}
	for table, want := range standaloneColumns {
		if err := standalonesqlite.CheckColumns(ctx, source, "usage-store", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	for table, want := range standaloneOptionalColumns {
		if !present[table] {
			continue
		}
		if err := standalonesqlite.CheckColumns(ctx, source, "usage-store", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	if present[standaloneUnwrittenTokenUsageTable] {
		var held int64
		if err := source.QueryRowContext(ctx, "SELECT count(*) FROM "+standaloneUnwrittenTokenUsageTable).Scan(&held); err != nil {
			return counts, fmt.Errorf("%s: counting its token usage rows: %w", sqlitePath, err)
		}
		if held > 0 {
			return counts, fmt.Errorf("%s holds %d rows in its token usage table, which this module does not keep; the import refuses to drop them", sqlitePath, held)
		}
	}
	if counts.LastLimitSnapshotNumber, err = lastStandaloneNumber(ctx, source, "limit_snapshots"); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	if counts.LastSpendTopupNumber, err = lastStandaloneNumber(ctx, source, "spend_topups"); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	var lastSpendSnapshotNumber int64
	if present["spend_snapshots"] {
		if lastSpendSnapshotNumber, err = lastStandaloneNumber(ctx, source, "spend_snapshots"); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "usage", "limit_snapshots", "spend_keys", "spend_daily", "spend_topups", "spend_snapshots"); err != nil {
			return err
		}
		var err error
		if counts.LimitSnapshots, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "limit_snapshots", Columns: standaloneColumns["limit_snapshots"], OrderBy: "id",
			Insert: `INSERT INTO limit_snapshots (id, snapshot_at, provider, window_key, used_percent, window_minutes, resets_at,
				plan_type, tier, source, raw_json) OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		}); err != nil {
			return err
		}
		if counts.SpendKeys, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "spend_keys", Columns: standaloneColumns["spend_keys"], OrderBy: "provider, api_key_id",
			Insert: `INSERT INTO spend_keys (provider, api_key_id, api_key_name, api_key_hint, api_key_status, raw_json, fetched_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		}); err != nil {
			return err
		}
		if counts.SpendDays, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "spend_daily", Columns: standaloneColumns["spend_daily"], OrderBy: "provider, api_key_id, date",
			Insert: `INSERT INTO spend_daily (provider, api_key_id, date, total_usd, fetched_at) VALUES ($1, $2, $3, $4, $5)`,
		}); err != nil {
			return err
		}
		if counts.SpendTopups, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "spend_topups", Columns: standaloneColumns["spend_topups"], OrderBy: "id",
			Insert: `INSERT INTO spend_topups (id, provider, amount_usd, occurred_at, note, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6)`,
		}); err != nil {
			return err
		}
		if present["spend_snapshots"] {
			if counts.SpendSnapshots, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
				Name: "spend_snapshots", Columns: standaloneOptionalColumns["spend_snapshots"], OrderBy: "id",
				Insert: `INSERT INTO spend_snapshots (id, fetched_at, provider, "window", period_start, period_end, total_usd, currency,
					source, raw_json) OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			}); err != nil {
				return err
			}
		}
		for table, last := range map[string]int64{"limit_snapshots": counts.LastLimitSnapshotNumber,
			"spend_topups": counts.LastSpendTopupNumber, "spend_snapshots": lastSpendSnapshotNumber} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, table, "id", last); err != nil {
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

// standaloneTables names the tables of the SQLite file.
func standaloneTables(ctx context.Context, source *sql.DB) (map[string]bool, error) {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("listing its tables: %w", err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("listing its tables: %w", err)
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

// lastStandaloneNumber is the highest number an AUTOINCREMENT table of the
// SQLite file handed out: SQLite's own record of it, or the highest id still
// there when that is higher.
func lastStandaloneNumber(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var last int64
	if err := source.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = $1), 0),
		(SELECT COALESCE(MAX(id), 0) FROM `+table+`))`, table).Scan(&last); err != nil {
		return 0, fmt.Errorf("reading the last number %s handed out: %w", table, err)
	}
	return last, nil
}
