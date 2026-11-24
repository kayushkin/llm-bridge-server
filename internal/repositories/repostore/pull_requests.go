package repostore

import "sort"

// PullRequest is one GitHub pull request of a registered repo, as the sync
// last read it from GitHub. GitHub owns it; repo-store keeps the copy so that
// no other service on the box calls GitHub for it. Times are unix seconds, and
// 0 for one that has not happened.
type PullRequest struct {
	RepoID int64 `json:"repo_id"`
	// Number is GitHub's number for the pull request, unique within the repo.
	Number int64            `json:"number"`
	Title  string           `json:"title"`
	State  PullRequestState `json:"state"`
	// IsDraft is GitHub's draft flag; a draft is still open.
	IsDraft bool `json:"is_draft"`
	// Author is the GitHub login of whoever opened it.
	Author  string `json:"author"`
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
	HeadSHA string `json:"head_sha"`
	// URL is the pull request's page on GitHub.
	URL       string `json:"url"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	MergedAt  int64  `json:"merged_at"`
	ClosedAt  int64  `json:"closed_at"`
	// SyncedAt is when repo-store last wrote this row from GitHub's answer.
	SyncedAt int64 `json:"synced_at"`
}

// PullRequestState is where a pull request stands. GitHub itself says only
// open or closed; merged is a closed pull request with a merge time, kept as a
// state of its own because "closed without merging" and "merged" are the two
// answers a reader of a closed pull request actually wants.
type PullRequestState string

const (
	PullRequestStateOpen   PullRequestState = "open"
	PullRequestStateClosed PullRequestState = "closed"
	PullRequestStateMerged PullRequestState = "merged"
)

// PullRequestStateQueryAll is the ?state= word for every state at once.
const PullRequestStateQueryAll = "all"

// pullRequestStatesAQueryAccepts is the closed set ?state= is checked against.
var pullRequestStatesAQueryAccepts = map[string]struct{}{
	string(PullRequestStateOpen):   {},
	string(PullRequestStateClosed): {},
	string(PullRequestStateMerged): {},
	PullRequestStateQueryAll:       {},
}

func pullRequestStatesAQueryAcceptsSorted() []string {
	out := make([]string, 0, len(pullRequestStatesAQueryAccepts))
	for state := range pullRequestStatesAQueryAccepts {
		out = append(out, state)
	}
	sort.Strings(out)
	return out
}
