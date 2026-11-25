package snapshotstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

var standaloneSnapshotColumns = []string{"session_id", "tool_use_id", "phase", "file_path", "blob_sha", "size",
	"is_binary", "too_large", "missing", "created_at"}

// ImportCounts says how many rows an import copied.
type ImportCounts struct {
	Snapshots     int64 `json:"snapshots"`
	DistinctBlobs int64 `json:"distinct_blobs"`
}

// ImportStandaloneDatabase copies every snapshot row of the SQLite
// snapshot-store file into the module behind db, as it is, with times cut to
// the microsecond Postgres keeps. The blobs stay in the git repository beside
// the file, which the store keeps using where it is. It runs in one
// transaction, so it copies everything or nothing, and it refuses a module
// that already holds a row and a file it cannot copy whole, including one
// still on the first schema, whose key lacked file_path: open that with a
// server from before the move once, so it migrates it, and import the result.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	var otherTables int64
	if err := source.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name != 'snapshots'`).Scan(&otherTables); err != nil {
		return counts, fmt.Errorf("%s: listing its tables: %w", sqlitePath, err)
	}
	if otherTables > 0 {
		return counts, fmt.Errorf("%s has %d tables besides snapshots, which the import does not know; it copies only a file it can copy whole", sqlitePath, otherTables)
	}
	if err := standalonesqlite.CheckColumns(ctx, source, "snapshot-store", "snapshots", standaloneSnapshotColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	var keyColumns int64
	if err := source.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('snapshots') WHERE pk > 0`).Scan(&keyColumns); err != nil {
		return counts, fmt.Errorf("%s: reading the key of snapshots: %w", sqlitePath, err)
	}
	if keyColumns != 4 {
		return counts, fmt.Errorf("%s: snapshots has a key of %d columns, not the four of (session_id, tool_use_id, phase, file_path); it is on the first schema", sqlitePath, keyColumns)
	}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "snapshots", "snapshots"); err != nil {
			return err
		}
		var err error
		counts.Snapshots, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "snapshots", Columns: standaloneSnapshotColumns, OrderBy: "session_id, tool_use_id, phase, file_path",
			Insert: `INSERT INTO snapshots (session_id, tool_use_id, phase, file_path, blob_sha, size, is_binary, too_large, missing, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: convertStandaloneSnapshot,
		})
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(DISTINCT blob_sha) FROM snapshots WHERE blob_sha <> ''`).Scan(&counts.DistinctBlobs)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func convertStandaloneSnapshot(row []any) ([]any, error) {
	values := make([]any, len(row))
	for index, column := range standaloneSnapshotColumns {
		var err error
		switch column {
		case "size":
			values[index], err = standalonesqlite.Integer(column, row[index])
		case "is_binary", "too_large", "missing":
			values[index], err = standalonesqlite.Boolean(column, row[index])
		case "created_at":
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
