package repostore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// repo-store is the one service in the ecosystem that reads pull requests from
// GitHub. It reaches GitHub through the gh program, logged in under the HOME
// the service runs with, rather than a token of its own. There is one way in
// and no second one to fall back to.

// GitHubAPI answers GET requests against GitHub's REST API. Path is relative
// to the API root, query included: "repos/example-owner/dash/pulls?state=all".
type GitHubAPI interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

// GitHubCommandLine reaches GitHub by running `gh api <path>`, as whatever
// account gh is logged in to under the process's HOME.
type GitHubCommandLine struct {
	// Program is the gh executable, a full path or a name on PATH.
	Program string
}

// NewGitHubCommandLine finds program and returns a GitHubCommandLine that runs
// it. It fails when program cannot be found, so a service that cannot reach
// GitHub says so at startup rather than on every sync.
func NewGitHubCommandLine(program string) (*GitHubCommandLine, error) {
	resolved, err := exec.LookPath(program)
	if err != nil {
		return nil, fmt.Errorf("find the gh program %q: %w", program, err)
	}
	return &GitHubCommandLine{Program: resolved}, nil
}

func (commandLine *GitHubCommandLine) Get(ctx context.Context, path string) ([]byte, error) {
	command := exec.CommandContext(ctx, commandLine.Program, "api", "--method", "GET", path)
	var standardError bytes.Buffer
	command.Stderr = &standardError
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(standardError.String()))
	}
	return output, nil
}

// githubPullRequestsPerPage is the most GitHub's list endpoint returns at once.
const githubPullRequestsPerPage = 100

// githubPullRequest is the part of GitHub's pull request object the store
// keeps. Times are RFC 3339; merged_at and closed_at are null until they
// happen.
type githubPullRequest struct {
	Number  int64   `json:"number"`
	Title   string  `json:"title"`
	State   string  `json:"state"`
	Draft   bool    `json:"draft"`
	HTMLURL string  `json:"html_url"`
	Created string  `json:"created_at"`
	Updated string  `json:"updated_at"`
	Merged  *string `json:"merged_at"`
	Closed  *string `json:"closed_at"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func unixSecondsOfGitHubTime(field, value string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", field, value, err)
	}
	return parsed.Unix(), nil
}

func (github githubPullRequest) toPullRequest() (PullRequest, error) {
	pullRequest := PullRequest{
		Number: github.Number, Title: github.Title, IsDraft: github.Draft, Author: github.User.Login,
		HeadRef: github.Head.Ref, BaseRef: github.Base.Ref, HeadSHA: github.Head.SHA, URL: github.HTMLURL,
	}
	var err error
	if pullRequest.CreatedAt, err = unixSecondsOfGitHubTime("created_at", github.Created); err != nil {
		return PullRequest{}, fmt.Errorf("pull request %d: %w", github.Number, err)
	}
	if pullRequest.UpdatedAt, err = unixSecondsOfGitHubTime("updated_at", github.Updated); err != nil {
		return PullRequest{}, fmt.Errorf("pull request %d: %w", github.Number, err)
	}
	if github.Merged != nil {
		if pullRequest.MergedAt, err = unixSecondsOfGitHubTime("merged_at", *github.Merged); err != nil {
			return PullRequest{}, fmt.Errorf("pull request %d: %w", github.Number, err)
		}
	}
	if github.Closed != nil {
		if pullRequest.ClosedAt, err = unixSecondsOfGitHubTime("closed_at", *github.Closed); err != nil {
			return PullRequest{}, fmt.Errorf("pull request %d: %w", github.Number, err)
		}
	}
	switch {
	case github.State == "open":
		pullRequest.State = PullRequestStateOpen
	case github.State == "closed" && pullRequest.MergedAt != 0:
		pullRequest.State = PullRequestStateMerged
	case github.State == "closed":
		pullRequest.State = PullRequestStateClosed
	default:
		return PullRequest{}, fmt.Errorf("pull request %d: GitHub reports state %q, which is neither open nor closed", github.Number, github.State)
	}
	return pullRequest, nil
}

// ownerAndNameOfGitHubURL splits https://github.com/<owner>/<name>, the form
// GitHubURLOfRemote derives, into owner and name.
func ownerAndNameOfGitHubURL(githubURL string) (string, string, error) {
	rest, found := strings.CutPrefix(githubURL, "https://"+githubHost+"/")
	owner, name, split := strings.Cut(rest, "/")
	if !found || !split || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%q is not a GitHub repository page", githubURL)
	}
	return owner, name, nil
}

// FetchPullRequestsUpdatedSince reads a repository's pull requests from GitHub,
// most recently updated first, a page at a time, and stops at the first one
// updated before since. Since 0 reads every pull request the repository has.
// A pull request updated in the same second as since is read again rather
// than missed.
func FetchPullRequestsUpdatedSince(ctx context.Context, github GitHubAPI, owner, name string, since int64) ([]PullRequest, error) {
	var out []PullRequest
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/pulls?state=all&sort=updated&direction=desc&per_page=%d&page=%d", owner, name, githubPullRequestsPerPage, page)
		body, err := github.Get(ctx, path)
		if err != nil {
			return nil, err
		}
		var pageOfPullRequests []githubPullRequest
		if err := json.Unmarshal(body, &pageOfPullRequests); err != nil {
			return nil, fmt.Errorf("GET %s answered something that is not a list of pull requests: %w", path, err)
		}
		for _, githubPullRequest := range pageOfPullRequests {
			pullRequest, err := githubPullRequest.toPullRequest()
			if err != nil {
				return nil, fmt.Errorf("GET %s: %w", path, err)
			}
			if pullRequest.UpdatedAt < since {
				return out, nil
			}
			out = append(out, pullRequest)
		}
		if len(pageOfPullRequests) < githubPullRequestsPerPage {
			return out, nil
		}
	}
}

// ErrRepoHasNoGitHubURL refuses a pull-request sync of a repo whose git remote
// is not on GitHub: there is nowhere to read its pull requests from.
var ErrRepoHasNoGitHubURL = errors.New("repo has no github_url")

// ErrGitHubRequestFailed wraps a sync that failed in talking to GitHub — gh
// could not run, GitHub refused, or the answer did not parse — as against one
// that failed in the store.
var ErrGitHubRequestFailed = errors.New("reading pull requests from GitHub failed")

// pullRequestSyncTimeout bounds one repo's sync, so a gh that hangs holds up
// one repo for a while rather than the loop for ever.
const pullRequestSyncTimeout = 2 * time.Minute

// PullRequestSyncer copies repos' pull requests from GitHub into the store.
// One repo is synced at a time per repo: the background loop and
// POST /repos/{id}/pull-requests/sync can ask for the same repo at once, and
// the second waits for the first rather than reading GitHub twice and racing
// to move the cursor.
type PullRequestSyncer struct {
	store  *Store
	github GitHubAPI

	repoLocksMutex sync.Mutex
	repoLocks      map[int64]*sync.Mutex
}

func NewPullRequestSyncer(store *Store, github GitHubAPI) *PullRequestSyncer {
	return &PullRequestSyncer{store: store, github: github, repoLocks: map[int64]*sync.Mutex{}}
}

func (syncer *PullRequestSyncer) lockOfRepo(repoID int64) *sync.Mutex {
	syncer.repoLocksMutex.Lock()
	defer syncer.repoLocksMutex.Unlock()
	lock, exists := syncer.repoLocks[repoID]
	if !exists {
		lock = &sync.Mutex{}
		syncer.repoLocks[repoID] = lock
	}
	return lock
}

// SyncRepo reads from GitHub the repo's pull requests updated since its last
// successful sync started — all of them the first time, or when its
// github_url has changed since — and stores them. It returns how many it
// wrote. On failure nothing is written and the cursor stays, so the next sync
// asks for the same span again.
func (syncer *PullRequestSyncer) SyncRepo(ctx context.Context, repo *Repo) (int, error) {
	if repo.GitHubURL == "" {
		return 0, fmt.Errorf("%w: repo %d (%s) has git_remote %q, which is not on GitHub", ErrRepoHasNoGitHubURL, repo.ID, repo.Name, repo.GitRemote)
	}
	owner, name, err := ownerAndNameOfGitHubURL(repo.GitHubURL)
	if err != nil {
		return 0, err
	}
	lock := syncer.lockOfRepo(repo.ID)
	lock.Lock()
	defer lock.Unlock()

	cursor, synced, err := syncer.store.PullRequestSyncCursorOf(repo.ID)
	if err != nil {
		return 0, err
	}
	var since int64
	if synced && cursor.GitHubURL == repo.GitHubURL {
		since = cursor.StartedAt
	}
	startedAt := now()
	ctx, cancel := context.WithTimeout(ctx, pullRequestSyncTimeout)
	defer cancel()
	pullRequests, err := FetchPullRequestsUpdatedSince(ctx, syncer.github, owner, name, since)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrGitHubRequestFailed, err)
	}
	if err := syncer.store.RecordPullRequestSync(repo.ID, PullRequestSyncCursor{GitHubURL: repo.GitHubURL, StartedAt: startedAt}, pullRequests); err != nil {
		return 0, err
	}
	return len(pullRequests), nil
}

// SyncEveryRepo syncs every registered repo that has a github_url, one after
// another. A repo whose sync fails is logged with its id, name and error and
// does not stop the others; the pass ends with one line saying how it went.
func (syncer *PullRequestSyncer) SyncEveryRepo(ctx context.Context) {
	startedAt := time.Now()
	repos, err := syncer.store.ListRepos(ListFilter{})
	if err != nil {
		log.Printf("[repo-store] pull-request sync: cannot list repos, so no repo was synced: %v", err)
		return
	}
	reposWithGitHubURL, pullRequestsWritten, failures := 0, 0, 0
	for index := range repos {
		repo := &repos[index]
		if repo.GitHubURL == "" {
			continue
		}
		if ctx.Err() != nil {
			log.Printf("[repo-store] pull-request sync stopped before repo %d (%s): %v", repo.ID, repo.Name, ctx.Err())
			return
		}
		reposWithGitHubURL++
		written, err := syncer.SyncRepo(ctx, repo)
		if err != nil {
			failures++
			log.Printf("[repo-store] pull-request sync of repo %d (%s, %s) FAILED: %v", repo.ID, repo.Name, repo.GitHubURL, err)
			continue
		}
		pullRequestsWritten += written
	}
	log.Printf("[repo-store] pull-request sync: %d repos with a github_url, %d pull requests written, %d repos failed, took %s",
		reposWithGitHubURL, pullRequestsWritten, failures, time.Since(startedAt).Round(time.Second))
}

// RunEvery syncs every repo now and then once per interval until ctx ends.
func (syncer *PullRequestSyncer) RunEvery(ctx context.Context, interval time.Duration) {
	log.Printf("[repo-store] pull-request sync runs every %s", interval)
	syncer.SyncEveryRepo(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncer.SyncEveryRepo(ctx)
		}
	}
}
