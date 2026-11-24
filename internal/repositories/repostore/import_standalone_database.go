package repostore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// standaloneColumns are the columns of each table the standalone repo-store
// kept in its SQLite file.
var standaloneColumns = map[string][]string{
	"repos":              {"id", "name", "path", "git_remote", "description", "provenance", "tags", "tag_overrides", "created_at", "updated_at"},
	"deployments":        {"id", "repo_id", "commit_sha", "branch", "default_branch", "source_path", "on_default", "dirty", "override_reason", "refusals", "deployed_by", "created_at"},
	"pull_requests":      {"repo_id", "number", "title", "state", "is_draft", "author", "head_ref", "base_ref", "head_sha", "url", "created_at", "updated_at", "merged_at", "closed_at", "synced_at"},
	"pull_request_syncs": {"repo_id", "github_url", "started_at", "finished_at"},
}

// ImportCounts says how many rows of each table an import copied, and where
// each numbered table carries on.
type ImportCounts struct {
	Repos            int64 `json:"repos"`
	LastRepoID       int64 `json:"last_repo_id"`
	Deployments      int64 `json:"deployments"`
	LastDeploymentID int64 `json:"last_deployment_id"`
	PullRequests     int64 `json:"pull_requests"`
	PullRequestSyncs int64 `json:"pull_request_syncs"`
}

// ImportStandaloneDatabase copies every repo, deployment, pull request and
// sync cursor from the SQLite file of a standalone repo-store into the module
// behind db, as they are: ids, times and values. It runs in one transaction,
// so it copies everything or nothing, and it refuses a module that already
// holds any row.
//
// Repo and deployment ids are kept, and each table numbers on after the
// highest id the standalone store ever handed out (its sqlite_sequence), so
// an id that work-graph-store, project-store or the deploy ledger already
// carries is never given to another repo.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := standalonesqlite.Open(sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	for table, want := range standaloneColumns {
		if err := standalonesqlite.CheckColumns(ctx, source, "repo-store", table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	lastRepoID, err := lastHandedOut(ctx, source, "repos")
	if err != nil {
		return counts, err
	}
	lastDeploymentID, err := lastHandedOut(ctx, source, "deployments")
	if err != nil {
		return counts, err
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		if err := standalonesqlite.RefuseRowsInModule(ctx, tx, "repos", "repos", "deployments", "pull_requests", "pull_request_syncs"); err != nil {
			return err
		}
		var err error
		if counts.Repos, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "repos", Columns: standaloneColumns["repos"], OrderBy: "id",
			Insert: `INSERT INTO repos (id, name, path, git_remote, description, provenance, tags, tag_overrides, created_at, updated_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			Convert: func(row []any) ([]any, error) {
				for index, column := range []string{"tags", "tag_overrides"} {
					text, err := standalonesqlite.Text(column, row[6+index])
					if err != nil {
						return nil, err
					}
					if err := checkStoredStrings(column, text); err != nil {
						return nil, err
					}
				}
				return row, nil
			},
		}); err != nil {
			return err
		}
		if counts.Deployments, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "deployments", Columns: standaloneColumns["deployments"], OrderBy: "id",
			Insert: `INSERT INTO deployments (id, repo_id, commit_sha, branch, default_branch, source_path, on_default, dirty, override_reason, refusals, deployed_by, created_at)
				OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			Convert: func(row []any) ([]any, error) {
				converted := append([]any{}, row...)
				for _, index := range []int{6, 7} {
					value, err := standalonesqlite.Boolean(standaloneColumns["deployments"][index], row[index])
					if err != nil {
						return nil, err
					}
					converted[index] = value
				}
				refusals, err := standalonesqlite.Text("refusals", row[9])
				if err != nil {
					return nil, err
				}
				var list []string
				if err := json.Unmarshal([]byte(refusals), &list); err != nil || list == nil {
					return nil, fmt.Errorf("refusals %q is not a JSON list of strings", refusals)
				}
				return converted, nil
			},
		}); err != nil {
			return err
		}
		if counts.PullRequests, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "pull_requests", Columns: standaloneColumns["pull_requests"], OrderBy: "repo_id, number",
			Insert: `INSERT INTO pull_requests (repo_id, number, title, state, is_draft, author, head_ref, base_ref, head_sha, url, created_at, updated_at, merged_at, closed_at, synced_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			Convert: func(row []any) ([]any, error) {
				converted := append([]any{}, row...)
				isDraft, err := standalonesqlite.Boolean("is_draft", row[4])
				if err != nil {
					return nil, err
				}
				converted[4] = isDraft
				return converted, nil
			},
		}); err != nil {
			return err
		}
		if counts.PullRequestSyncs, err = standalonesqlite.Copy(ctx, source, tx, standalonesqlite.Table{
			Name: "pull_request_syncs", Columns: standaloneColumns["pull_request_syncs"], OrderBy: "repo_id",
			Insert: `INSERT INTO pull_request_syncs (repo_id, github_url, started_at, finished_at) VALUES ($1, $2, $3, $4)`,
		}); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT GREATEST($1::bigint, COALESCE(MAX(id), 0)) FROM repos`, lastRepoID).Scan(&counts.LastRepoID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT GREATEST($1::bigint, COALESCE(MAX(id), 0)) FROM deployments`, lastDeploymentID).Scan(&counts.LastDeploymentID); err != nil {
			return err
		}
		if err := postgresstorage.ContinueNumberingAfter(ctx, tx, "repos", "id", counts.LastRepoID); err != nil {
			return err
		}
		return postgresstorage.ContinueNumberingAfter(ctx, tx, "deployments", "id", counts.LastDeploymentID)
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// lastHandedOut is the highest id an AUTOINCREMENT table ever gave out,
// deleted rows included, from SQLite's own record of it. 0 when the table has
// never had a row.
func lastHandedOut(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var last int64
	err := source.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&last)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the last %s id the standalone store handed out: %w", table, err)
	}
	return last, nil
}

// checkStoredStrings refuses a tags column that is neither empty nor a JSON
// list of strings: the store reads it back as one.
func checkStoredStrings(column, text string) error {
	if text == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		return fmt.Errorf("%s %q is not a JSON list of strings: %w", column, text, err)
	}
	return nil
}
