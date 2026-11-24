package repostore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PullRequestListFilter narrows ListPullRequests. RepoID 0 means every repo;
// State is a PullRequestState or PullRequestStateQueryAll; Limit must be
// positive.
type PullRequestListFilter struct {
	RepoID int64
	State  string
	Limit  int
}

const pullRequestColumns = `repo_id, number, title, state, is_draft, author, head_ref, base_ref, head_sha, url, created_at, updated_at, merged_at, closed_at, synced_at`

// ListPullRequests returns the stored pull requests the filter names, the most
// recently updated first. A RepoID that names no repo is ErrNotFound, so a
// caller with a stale id hears about it rather than reading an empty list.
func (s *Store) ListPullRequests(filter PullRequestListFilter) ([]PullRequest, error) {
	if _, known := pullRequestStatesAQueryAccepts[filter.State]; !known {
		return nil, fmt.Errorf("pull requests: unknown state %q; accepted: %s", filter.State, strings.Join(pullRequestStatesAQueryAcceptsSorted(), ", "))
	}
	if filter.Limit <= 0 {
		return nil, fmt.Errorf("pull requests: limit must be positive, got %d", filter.Limit)
	}
	query := `SELECT ` + pullRequestColumns + ` FROM pull_requests WHERE 1=1`
	var arguments []any
	if filter.RepoID != 0 {
		if _, err := s.GetRepo(filter.RepoID); err != nil {
			return nil, err
		}
		query += ` AND repo_id=?`
		arguments = append(arguments, filter.RepoID)
	}
	if filter.State != PullRequestStateQueryAll {
		query += ` AND state=?`
		arguments = append(arguments, filter.State)
	}
	query += ` ORDER BY updated_at DESC, repo_id, number DESC LIMIT ?`
	arguments = append(arguments, filter.Limit)
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PullRequest{}
	for rows.Next() {
		var pullRequest PullRequest
		var isDraft int
		if err := rows.Scan(&pullRequest.RepoID, &pullRequest.Number, &pullRequest.Title, &pullRequest.State, &isDraft, &pullRequest.Author,
			&pullRequest.HeadRef, &pullRequest.BaseRef, &pullRequest.HeadSHA, &pullRequest.URL,
			&pullRequest.CreatedAt, &pullRequest.UpdatedAt, &pullRequest.MergedAt, &pullRequest.ClosedAt, &pullRequest.SyncedAt); err != nil {
			return nil, err
		}
		pullRequest.IsDraft = isDraft == 1
		out = append(out, pullRequest)
	}
	return out, rows.Err()
}

// PullRequestSyncCursor is where a repo's last successful pull-request sync
// started, and which GitHub repository it read.
type PullRequestSyncCursor struct {
	GitHubURL  string
	StartedAt  int64
	FinishedAt int64
}

// PullRequestSyncCursorOf returns the repo's cursor, and false when no sync of
// it has succeeded yet.
func (s *Store) PullRequestSyncCursorOf(repoID int64) (PullRequestSyncCursor, bool, error) {
	var cursor PullRequestSyncCursor
	err := s.db.QueryRow(`SELECT github_url, started_at, finished_at FROM pull_request_syncs WHERE repo_id=?`, repoID).
		Scan(&cursor.GitHubURL, &cursor.StartedAt, &cursor.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestSyncCursor{}, false, nil
	}
	if err != nil {
		return PullRequestSyncCursor{}, false, err
	}
	return cursor, true, nil
}

// RecordPullRequestSync writes what one successful sync of a repo read from
// GitHub and moves the repo's cursor to where that sync started, in one
// transaction: a cursor that moved without its rows would make the next sync
// skip them. Each pull request's RepoID is set to repoID and its SyncedAt to
// the time of the write.
func (s *Store) RecordPullRequestSync(repoID int64, cursor PullRequestSyncCursor, pullRequests []PullRequest) error {
	for _, pullRequest := range pullRequests {
		if err := validatePullRequest(pullRequest); err != nil {
			return err
		}
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	syncedAt := now()
	for index := range pullRequests {
		pullRequest := &pullRequests[index]
		pullRequest.RepoID, pullRequest.SyncedAt = repoID, syncedAt
		if _, err := transaction.Exec(`
			INSERT INTO pull_requests (`+pullRequestColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo_id, number) DO UPDATE SET
				title=excluded.title, state=excluded.state, is_draft=excluded.is_draft, author=excluded.author,
				head_ref=excluded.head_ref, base_ref=excluded.base_ref, head_sha=excluded.head_sha, url=excluded.url,
				created_at=excluded.created_at, updated_at=excluded.updated_at, merged_at=excluded.merged_at,
				closed_at=excluded.closed_at, synced_at=excluded.synced_at`,
			repoID, pullRequest.Number, pullRequest.Title, string(pullRequest.State), boolInt(pullRequest.IsDraft), pullRequest.Author,
			pullRequest.HeadRef, pullRequest.BaseRef, pullRequest.HeadSHA, pullRequest.URL,
			pullRequest.CreatedAt, pullRequest.UpdatedAt, pullRequest.MergedAt, pullRequest.ClosedAt, syncedAt); err != nil {
			return fmt.Errorf("write pull request %d of repo %d: %w", pullRequest.Number, repoID, err)
		}
	}
	if _, err := transaction.Exec(`
		INSERT INTO pull_request_syncs (repo_id, github_url, started_at, finished_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (repo_id) DO UPDATE SET github_url=excluded.github_url, started_at=excluded.started_at, finished_at=excluded.finished_at`,
		repoID, cursor.GitHubURL, cursor.StartedAt, syncedAt); err != nil {
		return fmt.Errorf("move the pull-request sync cursor of repo %d: %w", repoID, err)
	}
	return transaction.Commit()
}

func validatePullRequest(pullRequest PullRequest) error {
	if pullRequest.Number <= 0 {
		return fmt.Errorf("pull request: number must be positive, got %d", pullRequest.Number)
	}
	switch pullRequest.State {
	case PullRequestStateOpen, PullRequestStateClosed, PullRequestStateMerged:
	default:
		return fmt.Errorf("pull request %d: unknown state %q", pullRequest.Number, pullRequest.State)
	}
	if pullRequest.UpdatedAt <= 0 {
		return fmt.Errorf("pull request %d: updated_at is missing", pullRequest.Number)
	}
	return nil
}
