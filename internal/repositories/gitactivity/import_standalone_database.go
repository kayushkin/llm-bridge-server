package gitactivity

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneRefUpdateColumns are the columns of the one table the standalone
// work-graph-store kept in its SQLite file.
var standaloneRefUpdateColumns = []string{"id", "recorded_at_unix_nano", "session_id", "hook", "git_subcommand", "repo_store_id",
	"repository_path", "worktree_path", "ref", "old_sha", "new_sha"}

// ImportCounts says how many ref updates an import copied.
type ImportCounts struct {
	RefUpdates         int64 `json:"ref_updates"`
	WithARepoStoreID   int64 `json:"with_a_repo_store_id"`
	LastRefUpdateID    int64 `json:"last_ref_update_id"`
	NewestRecordedUnix int64 `json:"newest_recorded_at_unix_nano"`
}

// ImportStandaloneDatabase copies every ref update from the SQLite file of a
// standalone work-graph-store into the module behind db, keeping ids, times
// and repo-store ids, in one transaction. It refuses a module that already
// holds any row. Afterwards the ids carry on after the highest one copied.
//
// It copies records only. The spool the git hook appends to is not touched:
// the standalone service goes on reading it until it stops.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	if err := standalonesqlite.CheckColumns(ctx, source, "work-graph-store", "ref_updates", standaloneRefUpdateColumns); err != nil {
		return counts, fmt.Errorf("%s: %w", sqlitePath, err)
	}
	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "git_activity", "ref_updates"); err != nil {
			return err
		}
		var err error
		counts.RefUpdates, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "ref_updates", Columns: standaloneRefUpdateColumns, OrderBy: "id",
			Insert: `INSERT INTO ref_updates (id, recorded_at_unix_nano, session_id, hook, git_subcommand, repo_store_id,
				repository_path, worktree_path, ref, old_sha, new_sha)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			Convert: func(row []any) ([]any, error) {
				if row[5] != nil {
					if _, err := standalonesqlite.Integer("repo_store_id", row[5]); err != nil {
						return nil, err
					}
				}
				return row, nil
			},
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(repo_store_id), COALESCE(MAX(id), 0), COALESCE(MAX(recorded_at_unix_nano), 0) FROM ref_updates`).
			Scan(&counts.WithARepoStoreID, &counts.LastRefUpdateID, &counts.NewestRecordedUnix); err != nil {
			return err
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "ref_updates", "id", counts.LastRefUpdateID)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}
