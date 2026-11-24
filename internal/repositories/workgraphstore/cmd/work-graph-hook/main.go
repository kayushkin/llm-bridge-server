// work-graph-hook is every git hook on this host. Git's global core.hooksPath
// points at a directory of one-line scripts, one per hook name, each of which
// execs this binary with the hook's name and arguments.
//
// For reference-transaction and post-checkout it appends what moved, and which
// agent session moved it, to work-graph-store's spool. Then, for every hook, it
// runs the repository's own hook from <git common dir>/hooks/<name> if there is
// one, because a global core.hooksPath otherwise hides it from git.
//
// Recording never changes what git does: a failure to record is printed and the
// hook carries on, and the exit code is the repository's own hook's, or 0.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	workgraphstore "github.com/kayushkin/work-graph-store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: work-graph-hook <hook name> [hook arguments…]")
		os.Exit(2)
	}
	hookName, hookArguments := os.Args[1], os.Args[2:]

	// git gives a hook with no input /dev/null, so this never waits.
	standardInput, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "work-graph-hook: read standard input: %v\n", err)
	}

	gitCommonDirectory, err := findGitCommonDirectory()
	if err != nil {
		fmt.Fprintf(os.Stderr, "work-graph-hook: find the git directory: %v\n", err)
		os.Exit(0)
	}

	if err := record(hookName, hookArguments, standardInput, gitCommonDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "work-graph-hook: could not record this %s in work-graph-store: %v\n", hookName, err)
	}

	os.Exit(runRepositoryHook(filepath.Join(gitCommonDirectory, "hooks", hookName), hookArguments, standardInput))
}

func record(hookName string, hookArguments []string, standardInput []byte, gitCommonDirectory string) error {
	var updates []workgraphstore.SpooledRefUpdate
	switch hookName {
	case workgraphstore.HookReferenceTransaction:
		// The hook runs for "prepared", "committed" and "aborted"; only a
		// committed transaction moved anything.
		if len(hookArguments) == 0 || hookArguments[0] != "committed" {
			return nil
		}
		for _, line := range strings.Split(string(standardInput), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			oldSHA, newSHA, ref := fields[0], fields[1], fields[2]
			if oldSHA == newSHA || !workgraphstore.IsRecordedRef(ref) {
				continue
			}
			updates = append(updates, workgraphstore.SpooledRefUpdate{Hook: hookName, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA})
		}
	case workgraphstore.HookPostCheckout:
		// Arguments: previous HEAD, new HEAD, and 1 for a branch checkout (0
		// for checking out files).
		if len(hookArguments) != 3 || hookArguments[2] != "1" {
			return nil
		}
		ref, err := gitOutput("symbolic-ref", "-q", "HEAD")
		if err != nil {
			ref = "HEAD"
		}
		updates = append(updates, workgraphstore.SpooledRefUpdate{Hook: hookName, Ref: ref, OldSHA: hookArguments[0], NewSHA: hookArguments[1]})
	}
	if len(updates) == 0 {
		return nil
	}

	worktreePath, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	gitSubcommand := workgraphstore.GitSubcommandOf(parentCommandLine())
	recordedAt := time.Now().UnixNano()

	var lines bytes.Buffer
	for _, update := range updates {
		update.RecordedAtUnixNano = recordedAt
		update.SessionID = os.Getenv("LLM_BRIDGE_SESSION_ID")
		update.GitSubcommand = gitSubcommand
		update.GitCommonDirectory = gitCommonDirectory
		update.WorktreePath = worktreePath
		encoded, err := json.Marshal(update)
		if err != nil {
			return err
		}
		lines.Write(encoded)
		lines.WriteByte('\n')
	}

	spoolPath := workgraphstore.DefaultSpoolPath()
	if err := os.MkdirAll(filepath.Dir(spoolPath), 0o700); err != nil {
		return err
	}
	spool, err := os.OpenFile(spoolPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// One write, so lines from two git commands never interleave.
	if _, err := spool.Write(lines.Bytes()); err != nil {
		spool.Close()
		return err
	}
	return spool.Close()
}

// parentCommandLine is the git command that ran this hook. The hook scripts
// exec this binary, so its parent is git itself.
func parentCommandLine() []string {
	content, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getppid()) + "/cmdline")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(content), "\x00"), "\x00")
}

// findGitCommonDirectory finds the common git directory the way git does for
// a hook, which runs at the top of the worktree: from GIT_DIR when git set it,
// else ./.git, following a worktree's .git file and commondir. Reading two
// small files is a tenth of the time of asking git, and every git command on
// the host runs several hooks. Anything unexpected asks git.
func findGitCommonDirectory() (string, error) {
	gitDirectory := os.Getenv("GIT_DIR")
	if gitDirectory == "" {
		gitDirectory = ".git"
		if content, err := os.ReadFile(".git"); err == nil {
			pointer, found := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir: ")
			if !found {
				return gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
			}
			gitDirectory = pointer
		}
	}
	gitDirectory, err := filepath.Abs(gitDirectory)
	if err != nil {
		return gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
	}
	if content, err := os.ReadFile(filepath.Join(gitDirectory, "commondir")); err == nil {
		common := strings.TrimSpace(string(content))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDirectory, common)
		}
		return filepath.Clean(common), nil
	}
	if information, err := os.Stat(filepath.Join(gitDirectory, "HEAD")); err == nil && !information.IsDir() {
		return filepath.Clean(gitDirectory), nil
	}
	return gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
}

func gitOutput(arguments ...string) (string, error) {
	output, err := exec.Command("git", arguments...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// runRepositoryHook runs the repository's own hook the way git would have, and
// returns its exit code; 0 when there is none.
func runRepositoryHook(path string, arguments []string, standardInput []byte) int {
	information, err := os.Stat(path)
	if err != nil || information.IsDir() || information.Mode()&0o111 == 0 {
		return 0
	}
	command := exec.Command(path, arguments...)
	command.Stdin = bytes.NewReader(standardInput)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err = command.Run()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "work-graph-hook: run %s: %v\n", path, err)
		return 1
	}
	return 0
}
