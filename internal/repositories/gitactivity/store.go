package gitactivity

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its ref updates in,
// with the migrations that build them. Until 2026-11-27 they were named
// work_graph and work_graph_module; an install that holds them is renamed at
// start.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "git_activity", Role: "git_activity_module", Migrations: migrations,
		FormerNames: []postgresstorage.FormerName{{Schema: "work_graph", Role: "work_graph_module"}}}, nil
}

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("not found")

// RefUpdate is a stored ref update.
type RefUpdate struct {
	ID                 int64  `json:"id"`
	RecordedAtUnixNano int64  `json:"recorded_at_unix_nano"`
	SessionID          string `json:"session_id"`
	Hook               string `json:"hook"`
	GitSubcommand      string `json:"git_subcommand"`
	// RepoStoreID is 0 when repo-store has no repo at RepositoryPath.
	RepoStoreID    int64  `json:"repo_store_id"`
	RepositoryPath string `json:"repository_path"`
	WorktreePath   string `json:"worktree_path"`
	Ref            string `json:"ref"`
	OldSHA         string `json:"old_sha"`
	NewSHA         string `json:"new_sha"`
}

// Store is the ref updates, in the module's schema.
type Store struct {
	database *sql.DB
}

// New wraps a pool opened on the module (postgresstorage.Open), migrated.
func New(database *sql.DB) *Store { return &Store{database: database} }

// InsertRefUpdates stores updates in one transaction. repoStoreIDs maps a
// repository path to repo-store's id for it; a path missing from it is stored
// with no id. Rows stored earlier for a path that now has an id get it too.
func (store *Store) InsertRefUpdates(updates []SpooledRefUpdate, repoStoreIDs map[string]int64) error {
	transaction, err := store.database.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, update := range updates {
		repositoryPath := RepositoryPathOf(update.GitCommonDirectory)
		var repoStoreID sql.NullInt64
		if id, known := repoStoreIDs[repositoryPath]; known {
			repoStoreID = sql.NullInt64{Int64: id, Valid: true}
		}
		if _, err := transaction.Exec(`INSERT INTO ref_updates
			(recorded_at_unix_nano, session_id, hook, git_subcommand, repo_store_id, repository_path, worktree_path, ref, old_sha, new_sha)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			update.RecordedAtUnixNano, update.SessionID, update.Hook, update.GitSubcommand, repoStoreID,
			repositoryPath, update.WorktreePath, update.Ref, update.OldSHA, update.NewSHA); err != nil {
			return err
		}
	}
	for repositoryPath, id := range repoStoreIDs {
		if _, err := transaction.Exec(`UPDATE ref_updates SET repo_store_id = $1 WHERE repository_path = $2 AND repo_store_id IS NULL`, id, repositoryPath); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

// RefUpdateFilter narrows ListRefUpdates. A zero field does not filter.
type RefUpdateFilter struct {
	RepoStoreID   int64
	SessionID     string
	Ref           string
	SinceUnixNano int64
	Limit         int
}

// ListRefUpdates returns matching updates, newest first.
func (store *Store) ListRefUpdates(filter RefUpdateFilter) ([]RefUpdate, error) {
	var conditions []string
	var arguments []any
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return fmt.Sprintf("$%d", len(arguments))
	}
	if filter.RepoStoreID != 0 {
		conditions = append(conditions, "repo_store_id = "+placeholder(filter.RepoStoreID))
	}
	if filter.SessionID != "" {
		conditions = append(conditions, "session_id = "+placeholder(filter.SessionID))
	}
	if filter.Ref != "" {
		conditions = append(conditions, "ref = "+placeholder(filter.Ref))
	}
	if filter.SinceUnixNano != 0 {
		conditions = append(conditions, "recorded_at_unix_nano >= "+placeholder(filter.SinceUnixNano))
	}
	query := `SELECT id, recorded_at_unix_nano, session_id, hook, git_subcommand, COALESCE(repo_store_id, 0),
		repository_path, worktree_path, ref, old_sha, new_sha FROM ref_updates`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY recorded_at_unix_nano DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT " + placeholder(filter.Limit)
	}
	rows, err := store.database.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	updates := []RefUpdate{}
	for rows.Next() {
		var update RefUpdate
		if err := rows.Scan(&update.ID, &update.RecordedAtUnixNano, &update.SessionID, &update.Hook, &update.GitSubcommand,
			&update.RepoStoreID, &update.RepositoryPath, &update.WorktreePath, &update.Ref, &update.OldSHA, &update.NewSHA); err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

// CommitAuthorship is which session made a commit.
type CommitAuthorship struct {
	SHA                string `json:"sha"`
	SessionID          string `json:"session_id"`
	GitSubcommand      string `json:"git_subcommand"`
	RecordedAtUnixNano int64  `json:"recorded_at_unix_nano"`
}

// CommitAuthorships returns the session that made each commit in firstParents
// (commit sha to its first parent's sha, "" for a root commit), for the commits
// some session made on this host.
//
// The maker is the earliest update that pointed HEAD or a local branch at the
// commit from a command that makes commits. For merge, rebase and pull, which
// can also fast-forward onto commits made elsewhere, the update must also have
// moved the ref from the commit's first parent, as making a commit does. A
// commit fetched from elsewhere, or made before the hook was installed, has no
// maker. The case this gets wrong: a fast-forward of exactly one commit made
// off this host credits whoever fast-forwarded it.
func (store *Store) CommitAuthorships(repoStoreID int64, firstParents map[string]string) (map[string]CommitAuthorship, error) {
	authorships := map[string]CommitAuthorship{}
	shas := make([]string, 0, len(firstParents))
	for sha := range firstParents {
		shas = append(shas, sha)
	}
	// Two arrays, so the statement is the same whatever the number of shas.
	// A graph asks for a few hundred at most.
	rows, err := store.database.Query(`SELECT new_sha, old_sha, session_id, git_subcommand, recorded_at_unix_nano FROM ref_updates
		WHERE repo_store_id = $1 AND new_sha = ANY($2)
		AND hook = 'reference-transaction' AND (ref = 'HEAD' OR ref LIKE 'refs/heads/%')
		AND git_subcommand = ANY($3)
		ORDER BY recorded_at_unix_nano ASC, id ASC`, repoStoreID, shas, CommitCreatingSubcommands)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var authorship CommitAuthorship
		var oldSHA string
		if err := rows.Scan(&authorship.SHA, &oldSHA, &authorship.SessionID, &authorship.GitSubcommand, &authorship.RecordedAtUnixNano); err != nil {
			return nil, err
		}
		firstParent := firstParents[authorship.SHA]
		if firstParent == "" {
			firstParent = ZeroSHA
		}
		if slices.Contains(FastForwardingSubcommands, authorship.GitSubcommand) && oldSHA != firstParent {
			continue
		}
		if _, seen := authorships[authorship.SHA]; !seen {
			authorships[authorship.SHA] = authorship
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return authorships, nil
}

// SessionRepo is how many refs one session moved in one repo, and when it
// last moved one: the measure of which repos a session has worked in.
type SessionRepo struct {
	SessionID      string `json:"session_id"`
	RepoStoreID    int64  `json:"repo_store_id"`
	RefUpdates     int64  `json:"ref_updates"`
	LastAtUnixNano int64  `json:"last_at_unix_nano"`
}

// SessionRepos counts ref updates by session and repo since a time, for
// every session that moved a ref in a repo repo-store knows. Git commands no
// session ran, and repos repo-store has no id for, are left out.
func (store *Store) SessionRepos(sinceUnixNano int64) ([]SessionRepo, error) {
	rows, err := store.database.Query(`SELECT session_id, repo_store_id, COUNT(*), MAX(recorded_at_unix_nano) FROM ref_updates
		WHERE session_id != '' AND repo_store_id IS NOT NULL AND recorded_at_unix_nano >= $1
		GROUP BY session_id, repo_store_id ORDER BY session_id, repo_store_id`, sinceUnixNano)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []SessionRepo{}
	for rows.Next() {
		var repo SessionRepo
		if err := rows.Scan(&repo.SessionID, &repo.RepoStoreID, &repo.RefUpdates, &repo.LastAtUnixNano); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}
