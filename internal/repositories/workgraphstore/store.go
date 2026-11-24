package workgraphstore

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

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

// Store is the SQLite database of ref updates.
type Store struct {
	database      *sql.DB
	dataDirectory string
}

// Open opens or creates work-graph-store.db in dataDirectory.
func Open(dataDirectory string) (*Store, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite3", filepath.Join(dataDirectory, "work-graph-store.db")+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(schemaSQL); err != nil {
		database.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{database: database, dataDirectory: dataDirectory}, nil
}

// Close closes the database.
func (store *Store) Close() error { return store.database.Close() }

// DataDirectory is where the database lives.
func (store *Store) DataDirectory() string { return store.dataDirectory }

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
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			update.RecordedAtUnixNano, update.SessionID, update.Hook, update.GitSubcommand, repoStoreID,
			repositoryPath, update.WorktreePath, update.Ref, update.OldSHA, update.NewSHA); err != nil {
			return err
		}
	}
	for repositoryPath, id := range repoStoreIDs {
		if _, err := transaction.Exec(`UPDATE ref_updates SET repo_store_id = ? WHERE repository_path = ? AND repo_store_id IS NULL`, id, repositoryPath); err != nil {
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
	if filter.RepoStoreID != 0 {
		conditions = append(conditions, "repo_store_id = ?")
		arguments = append(arguments, filter.RepoStoreID)
	}
	if filter.SessionID != "" {
		conditions = append(conditions, "session_id = ?")
		arguments = append(arguments, filter.SessionID)
	}
	if filter.Ref != "" {
		conditions = append(conditions, "ref = ?")
		arguments = append(arguments, filter.Ref)
	}
	if filter.SinceUnixNano != 0 {
		conditions = append(conditions, "recorded_at_unix_nano >= ?")
		arguments = append(arguments, filter.SinceUnixNano)
	}
	query := `SELECT id, recorded_at_unix_nano, session_id, hook, git_subcommand, COALESCE(repo_store_id, 0),
		repository_path, worktree_path, ref, old_sha, new_sha FROM ref_updates`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY recorded_at_unix_nano DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		arguments = append(arguments, filter.Limit)
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
	placeholders := make([]string, len(CommitCreatingSubcommands))
	subcommandArguments := make([]any, len(CommitCreatingSubcommands))
	for index, subcommand := range CommitCreatingSubcommands {
		placeholders[index] = "?"
		subcommandArguments[index] = subcommand
	}
	// SQLite caps the number of bound variables; ask in slices.
	const batch = 400
	for start := 0; start < len(shas); start += batch {
		end := min(start+batch, len(shas))
		shaPlaceholders := strings.Repeat("?,", end-start)
		shaPlaceholders = shaPlaceholders[:len(shaPlaceholders)-1]
		arguments := []any{repoStoreID}
		for _, sha := range shas[start:end] {
			arguments = append(arguments, sha)
		}
		arguments = append(arguments, subcommandArguments...)
		rows, err := store.database.Query(`SELECT new_sha, old_sha, session_id, git_subcommand, recorded_at_unix_nano FROM ref_updates
			WHERE repo_store_id = ? AND new_sha IN (`+shaPlaceholders+`)
			AND hook = 'reference-transaction' AND (ref = 'HEAD' OR ref LIKE 'refs/heads/%')
			AND git_subcommand IN (`+strings.Join(placeholders, ",")+`)
			ORDER BY recorded_at_unix_nano ASC, id ASC`, arguments...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var authorship CommitAuthorship
			var oldSHA string
			if err := rows.Scan(&authorship.SHA, &oldSHA, &authorship.SessionID, &authorship.GitSubcommand, &authorship.RecordedAtUnixNano); err != nil {
				rows.Close()
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
		if err := rows.Close(); err != nil {
			return nil, err
		}
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
		WHERE session_id != '' AND repo_store_id IS NOT NULL AND recorded_at_unix_nano >= ?
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
