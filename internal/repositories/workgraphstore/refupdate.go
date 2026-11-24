// Package workgraphstore records which agent session moved which git ref, and
// draws each repo's commit graph with those sessions on it. Git owns commits
// and branches; this store owns only the link between a ref update and the
// session that made it, which git does not keep.
package workgraphstore

import (
	"strings"
)

// ZeroSHA is git's name for "no commit": the old side of a ref that was just
// created, or the new side of one just deleted.
const ZeroSHA = "0000000000000000000000000000000000000000"

// Hook names as git calls them, and as RefUpdate.Hook records them.
const (
	HookReferenceTransaction = "reference-transaction"
	HookPostCheckout         = "post-checkout"
)

// SessionEnvironmentVariable is how llm-bridge-server tells an agent its own
// session id. A git command run by anything else records an empty session.
const SessionEnvironmentVariable = "LLM_BRIDGE_SESSION_ID"

// SpooledRefUpdate is one line the hook appends to the spool: one ref moving in
// one repository, in one git command, in one session. The service reads these
// and stores them as RefUpdate rows.
type SpooledRefUpdate struct {
	RecordedAtUnixNano int64  `json:"recorded_at_unix_nano"`
	SessionID          string `json:"session_id"`
	Hook               string `json:"hook"`
	// GitSubcommand is the git command that moved the ref ("commit", "rebase",
	// "push"), without its arguments, which can hold a commit message or a URL
	// with a password in it.
	GitSubcommand      string `json:"git_subcommand"`
	GitCommonDirectory string `json:"git_common_directory"`
	WorktreePath       string `json:"worktree_path"`
	// Ref is a full ref name ("refs/heads/main", "HEAD"). For a post-checkout
	// line it is the branch the worktree now has checked out, or HEAD when the
	// checkout left it detached.
	Ref    string `json:"ref"`
	OldSHA string `json:"old_sha"`
	NewSHA string `json:"new_sha"`
}

// RecordedRefPrefixes are the refs worth a row. Tags are left out: fetching a
// module's tags into Go's cache would bury everything else. ORIG_HEAD,
// CHERRY_PICK_HEAD and the stash are git's own scratch space.
var RecordedRefPrefixes = []string{"refs/heads/", "refs/remotes/"}

// IsRecordedRef says whether a ref update deserves a row.
func IsRecordedRef(ref string) bool {
	if ref == "HEAD" {
		return true
	}
	for _, prefix := range RecordedRefPrefixes {
		if strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

// gitOptionsTakingAValue are git's global options whose value is the next
// argument, so the subcommand is not that argument.
var gitOptionsTakingAValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true, "--super-prefix": true, "--config-env": true, "--list-cmds": true}

// GitSubcommandOf returns the subcommand in a git command line ("git -C x
// commit -m y" is "commit"), or "" when there is none.
func GitSubcommandOf(arguments []string) string {
	if len(arguments) == 0 {
		return ""
	}
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]
		if gitOptionsTakingAValue[argument] {
			index++
			continue
		}
		if strings.HasPrefix(argument, "-") {
			continue
		}
		return argument
	}
	return ""
}

// CommitCreatingSubcommands are the git commands that can make commits.
// Every other command (fetch, reset, checkout, push) only moves a ref onto
// commits that already exist.
var CommitCreatingSubcommands = []string{"commit", "cherry-pick", "revert", "am", "commit-tree", "merge", "rebase", "pull"}

// FastForwardingSubcommands are the commit-making commands that can also move
// a ref onto commits they did not make: a fast-forward merge or pull, or a
// rebase stepping onto its upstream. Their update names a commit's maker only
// when it moved the ref from the commit's first parent to the commit.
var FastForwardingSubcommands = []string{"merge", "rebase", "pull"}

// RepositoryPathOf returns the repository a git common directory belongs to:
// the directory holding ".git" for an ordinary clone, and the directory itself
// for a bare one. Every worktree of a clone shares its common directory, so
// this is the main clone's path from any of them.
func RepositoryPathOf(gitCommonDirectory string) string {
	trimmed := strings.TrimRight(gitCommonDirectory, "/")
	if strings.HasSuffix(trimmed, "/.git") {
		return strings.TrimSuffix(trimmed, "/.git")
	}
	return trimmed
}
