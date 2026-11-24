package workgraphstore

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Commit is one commit as git has it, with the session that made it when this
// store saw that happen.
type Commit struct {
	SHA         string   `json:"sha"`
	Parents     []string `json:"parents"`
	CommittedAt int64    `json:"committed_at"`
	AuthorName  string   `json:"author_name"`
	Subject     string   `json:"subject"`
	// MadeBy is nil when no session on this host is known to have made it.
	MadeBy *CommitAuthorship `json:"made_by"`
	// OnOriginBranch says a branch of the origin remote contains the commit
	// (as of the clone's last fetch). repo-store's git_remote, and so its
	// github_url, is origin's URL, so this is what says a link to the commit
	// there will not 404.
	OnOriginBranch bool `json:"on_origin_branch"`
}

// BranchSession is one session's part in a branch's history.
type BranchSession struct {
	SessionID string `json:"session_id"`
	// RefUpdates counts the times the session moved the branch.
	RefUpdates int `json:"ref_updates"`
	// Checkouts counts the times the session checked the branch out.
	Checkouts       int      `json:"checkouts"`
	GitSubcommands  []string `json:"git_subcommands"`
	FirstAtUnixNano int64    `json:"first_at_unix_nano"`
	LastAtUnixNano  int64    `json:"last_at_unix_nano"`
}

// Branch is a local or remote-tracking branch.
type Branch struct {
	// Ref is the full name ("refs/heads/main", "refs/remotes/origin/main").
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	IsRemote    bool   `json:"is_remote"`
	HeadSHA     string `json:"head_sha"`
	CommittedAt int64  `json:"committed_at"`
	// MergedIntoDefault says the default branch contains the head.
	MergedIntoDefault bool `json:"merged_into_default"`
	// WorktreePath is the worktree that has the branch checked out, or "".
	WorktreePath string          `json:"worktree_path"`
	Sessions     []BranchSession `json:"sessions"`
}

// Graph is one repo's commit graph.
type Graph struct {
	Repo Repo `json:"repo"`
	// DefaultBranch is resolved as deploy-gate resolves it: the branch
	// origin/HEAD names, else main, else master; "" when none exists.
	DefaultBranch string   `json:"default_branch"`
	Commits       []Commit `json:"commits"`
	Branches      []Branch `json:"branches"`
	// Truncated says there are older commits than those listed.
	Truncated bool `json:"truncated"`
}

func git(ctx context.Context, repositoryPath string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repositoryPath}, arguments...)...)
	output, err := command.Output()
	if err != nil {
		if exitError, isExitError := err.(*exec.ExitError); isExitError {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(exitError.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
	}
	return string(output), nil
}

// DefaultBranchOf resolves a repository's default branch the way deploy-gate
// does, and returns "" when there is none.
func DefaultBranchOf(ctx context.Context, repositoryPath string) string {
	if output, err := git(ctx, repositoryPath, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(output), "origin/")
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := git(ctx, repositoryPath, "show-ref", "--verify", "--quiet", "refs/heads/"+candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// BuildGraph reads a repo's newest maxCommits commits across all branches and
// joins in which session made each commit and touched each branch.
func BuildGraph(ctx context.Context, store *Store, repo Repo, maxCommits int) (Graph, error) {
	graph := Graph{Repo: repo, Commits: []Commit{}, Branches: []Branch{}}
	graph.DefaultBranch = DefaultBranchOf(ctx, repo.Path)

	output, err := git(ctx, repo.Path, "log", "--branches", "--remotes", "--topo-order",
		"-n", strconv.Itoa(maxCommits+1), "--format=%H%x1f%P%x1f%ct%x1f%an%x1f%s%x1e")
	if err != nil {
		return graph, err
	}
	firstParents := map[string]string{}
	for _, record := range strings.Split(output, "\x1e") {
		fields := strings.Split(strings.TrimLeft(record, "\n"), "\x1f")
		if len(fields) != 5 {
			continue
		}
		if len(graph.Commits) == maxCommits {
			graph.Truncated = true
			break
		}
		committedAt, _ := strconv.ParseInt(fields[2], 10, 64)
		commit := Commit{SHA: fields[0], Parents: strings.Fields(fields[1]), CommittedAt: committedAt, AuthorName: fields[3], Subject: fields[4]}
		if commit.Parents == nil {
			commit.Parents = []string{}
		}
		firstParents[commit.SHA] = ""
		if len(commit.Parents) > 0 {
			firstParents[commit.SHA] = commit.Parents[0]
		}
		graph.Commits = append(graph.Commits, commit)
	}
	// Every listed commit is on a local or a remote-tracking branch, so one
	// that is on no origin branch is on a local branch or another remote's,
	// and is one of these.
	output, err = git(ctx, repo.Path, "rev-list", "--branches", "--remotes", "--not", "--remotes=origin")
	if err != nil {
		return graph, err
	}
	notOnOrigin := map[string]bool{}
	for _, sha := range strings.Fields(output) {
		notOnOrigin[sha] = true
	}
	for index := range graph.Commits {
		graph.Commits[index].OnOriginBranch = !notOnOrigin[graph.Commits[index].SHA]
	}

	authorships, err := store.CommitAuthorships(repo.ID, firstParents)
	if err != nil {
		return graph, err
	}
	for index := range graph.Commits {
		if authorship, found := authorships[graph.Commits[index].SHA]; found {
			graph.Commits[index].MadeBy = &authorship
		}
	}

	graph.Branches, err = readBranches(ctx, store, repo, graph.DefaultBranch)
	return graph, err
}

func readBranches(ctx context.Context, store *Store, repo Repo, defaultBranch string) ([]Branch, error) {
	output, err := git(ctx, repo.Path, "for-each-ref", "--format=%(refname)%1f%(objectname)%1f%(committerdate:unix)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	merged := map[string]bool{}
	if defaultBranch != "" {
		mergedOutput, err := git(ctx, repo.Path, "for-each-ref", "--format=%(refname)", "--merged", "refs/heads/"+defaultBranch, "refs/heads", "refs/remotes")
		if err != nil {
			return nil, err
		}
		for _, ref := range strings.Fields(mergedOutput) {
			merged[ref] = true
		}
	}
	worktrees, err := worktreesByBranch(ctx, repo.Path)
	if err != nil {
		return nil, err
	}
	sessions, err := store.BranchSessions(repo.ID)
	if err != nil {
		return nil, err
	}

	branches := []Branch{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 3 || strings.HasSuffix(fields[0], "/HEAD") {
			continue
		}
		committedAt, _ := strconv.ParseInt(fields[2], 10, 64)
		ref := fields[0]
		branch := Branch{Ref: ref, HeadSHA: fields[1], CommittedAt: committedAt, MergedIntoDefault: merged[ref], WorktreePath: worktrees[ref], Sessions: sessions[ref]}
		if strings.HasPrefix(ref, "refs/remotes/") {
			branch.IsRemote = true
			branch.Name = strings.TrimPrefix(ref, "refs/remotes/")
		} else {
			branch.Name = strings.TrimPrefix(ref, "refs/heads/")
		}
		if branch.Sessions == nil {
			branch.Sessions = []BranchSession{}
		}
		branches = append(branches, branch)
	}
	return branches, nil
}

func worktreesByBranch(ctx context.Context, repositoryPath string) (map[string]string, error) {
	output, err := git(ctx, repositoryPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	byBranch := map[string]string{}
	var path string
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(line, "worktree "); found {
			path = value
		} else if value, found := strings.CutPrefix(line, "branch "); found {
			byBranch[value] = path
		}
	}
	return byBranch, nil
}

// branchSessionsQuery reads one repo's ref updates that a session made, the
// earliest first. It is read by index ref_updates_by_repo_store_id, which
// also holds its order.
const branchSessionsQuery = `SELECT ref, session_id, hook, git_subcommand, recorded_at_unix_nano FROM ref_updates
	WHERE repo_store_id = ? AND session_id != '' AND ref != 'HEAD' ORDER BY recorded_at_unix_nano ASC, id ASC`

// BranchSessions returns, for each branch ref of a repo, the sessions that
// moved it or checked it out, the earliest first. Updates no session made are
// left out.
func (store *Store) BranchSessions(repoStoreID int64) (map[string][]BranchSession, error) {
	rows, err := store.database.Query(branchSessionsQuery, repoStoreID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ ref, sessionID string }
	indexes := map[key]int{}
	byRef := map[string][]BranchSession{}
	for rows.Next() {
		var ref, sessionID, hook, subcommand string
		var recordedAt int64
		if err := rows.Scan(&ref, &sessionID, &hook, &subcommand, &recordedAt); err != nil {
			return nil, err
		}
		k := key{ref, sessionID}
		index, seen := indexes[k]
		if !seen {
			index = len(byRef[ref])
			indexes[k] = index
			byRef[ref] = append(byRef[ref], BranchSession{SessionID: sessionID, GitSubcommands: []string{}, FirstAtUnixNano: recordedAt})
		}
		session := &byRef[ref][index]
		if hook == HookPostCheckout {
			session.Checkouts++
		} else {
			session.RefUpdates++
		}
		if subcommand != "" && !slices.Contains(session.GitSubcommands, subcommand) {
			session.GitSubcommands = append(session.GitSubcommands, subcommand)
			sort.Strings(session.GitSubcommands)
		}
		session.LastAtUnixNano = recordedAt
	}
	return byRef, rows.Err()
}
