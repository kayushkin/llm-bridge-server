package workgraphstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// A commit mention is how a chat message names one commit so the chat can
// draw it as a reference chip: `<repo>@<sha>`, where the repo is repo-store's
// name for it (`dash@8487d32`) or its GitHub owner/name
// (`kayushkin/dash@8487d32`, the form kanban card links use). The sha may be
// abbreviated to 7 characters; the answer carries it in full.
var commitMention = regexp.MustCompile(`^(?:([\w.-]+)/)?([\w.-]+)@([0-9a-fA-F]{7,40})$`)

// ErrAmbiguous is a mention that names more than one repo or commit.
var ErrAmbiguous = errors.New("ambiguous")

// CommitAnswer is GET /commits/{mention...}: the commit, and the repo it is in as
// repo-store has it.
type CommitAnswer struct {
	Repo   Repo   `json:"repo"`
	Commit Commit `json:"commit"`
}

// ResolveCommitMention finds the repo and the commit a mention names. A mention
// of a repo or commit nobody has wraps ErrNotFound; a short sha that fits two
// commits, or a GitHub owner/name that two repo-store repos share, wraps
// ErrAmbiguous; anything that is not a mention wraps errBadRequest.
func ResolveCommitMention(ctx context.Context, store *Store, repoStore *RepoStoreClient, mention string) (CommitAnswer, error) {
	match := commitMention.FindStringSubmatch(mention)
	if match == nil {
		return CommitAnswer{}, fmt.Errorf("%w: a commit mention is <repo>@<sha> or <owner>/<repo>@<sha>", errBadRequest)
	}
	owner, name, sha := match[1], match[2], strings.ToLower(match[3])

	var repo Repo
	var err error
	if owner == "" {
		repo, err = repoStore.RepoByName(ctx, name)
	} else {
		repo, err = repoStore.RepoByGitHubURL(ctx, "https://github.com/"+owner+"/"+name)
	}
	if err != nil {
		return CommitAnswer{}, err
	}
	if repo.Path == "" {
		return CommitAnswer{}, fmt.Errorf("repo-store has no path for repo %d, so there is no clone to read the commit from", repo.ID)
	}

	commit, err := readCommit(ctx, repo.Path, sha)
	if err != nil {
		return CommitAnswer{}, err
	}
	firstParent := ""
	if len(commit.Parents) > 0 {
		firstParent = commit.Parents[0]
	}
	authorships, err := store.CommitAuthorships(repo.ID, map[string]string{commit.SHA: firstParent})
	if err != nil {
		return CommitAnswer{}, err
	}
	if authorship, found := authorships[commit.SHA]; found {
		commit.MadeBy = &authorship
	}
	return CommitAnswer{Repo: repo, Commit: commit}, nil
}

// readCommit reads one commit from a clone. sha is hex, checked by the caller,
// so it cannot be read by git as an option.
func readCommit(ctx context.Context, repositoryPath, sha string) (Commit, error) {
	output, err := git(ctx, repositoryPath, "rev-parse", "--verify", "--end-of-options", sha+"^{commit}")
	if err != nil {
		if strings.Contains(err.Error(), "ambiguous") {
			return Commit{}, fmt.Errorf("%w: %s fits more than one object in %s", ErrAmbiguous, sha, repositoryPath)
		}
		return Commit{}, fmt.Errorf("%w: no commit %s in %s", ErrNotFound, sha, repositoryPath)
	}
	fullSHA := strings.TrimSpace(output)
	output, err = git(ctx, repositoryPath, "show", "-s", "--format=%H%x1f%P%x1f%ct%x1f%an%x1f%s", fullSHA)
	if err != nil {
		return Commit{}, err
	}
	fields := strings.Split(strings.TrimRight(output, "\n"), "\x1f")
	if len(fields) != 5 {
		return Commit{}, fmt.Errorf("git show %s printed %d fields, want 5", fullSHA, len(fields))
	}
	committedAt, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Commit{}, fmt.Errorf("git show %s: commit time %q: %w", fullSHA, fields[2], err)
	}
	parents := strings.Fields(fields[1])
	if parents == nil {
		parents = []string{}
	}
	output, err = git(ctx, repositoryPath, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", fullSHA, "refs/remotes/origin/")
	if err != nil {
		return Commit{}, err
	}
	return Commit{SHA: fields[0], Parents: parents, CommittedAt: committedAt, AuthorName: fields[3], Subject: fields[4],
		OnOriginBranch: strings.TrimSpace(output) != ""}, nil
}

// RepoByName returns the repo repo-store has under name, or an error wrapping
// ErrNotFound. repo-store keeps names unique.
func (client *RepoStoreClient) RepoByName(ctx context.Context, name string) (Repo, error) {
	var repo Repo
	return repo, client.get(ctx, "/repos/by-name/"+url.PathEscape(name), &repo)
}

// RepoByGitHubURL returns the one repo whose github_url is githubURL, compared
// without regard to case as GitHub compares owner and repo names.
func (client *RepoStoreClient) RepoByGitHubURL(ctx context.Context, githubURL string) (Repo, error) {
	repos, err := client.Repos(ctx)
	if err != nil {
		return Repo{}, err
	}
	var found []Repo
	for _, repo := range repos {
		if repo.GitHubURL != "" && strings.EqualFold(repo.GitHubURL, githubURL) {
			found = append(found, repo)
		}
	}
	switch len(found) {
	case 0:
		return Repo{}, fmt.Errorf("%w: repo-store has no repo at %s", ErrNotFound, githubURL)
	case 1:
		return found[0], nil
	default:
		ids := make([]string, len(found))
		for index, repo := range found {
			ids[index] = strconv.FormatInt(repo.ID, 10)
		}
		return Repo{}, fmt.Errorf("%w: repo-store repos %s all have %s", ErrAmbiguous, strings.Join(ids, ", "), githubURL)
	}
}
