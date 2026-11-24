package workgraphstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two sessions share a repo the way agents do here: one commits on main, the
// other opens a worktree on a branch of its own and commits there, then the
// first rebases that branch and merges it. The real hook binary records every
// move, and the graph must credit each commit to the session that made it and
// list both sessions on the branch.
func TestTheHookRecordsWhichSessionMadeEachCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	scratch := t.TempDir()
	home := filepath.Join(scratch, "home")
	hookBinary := filepath.Join(scratch, "work-graph-hook")
	build := exec.Command("go", "build", "-o", hookBinary, "./cmd/work-graph-hook")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the hook: %v\n%s", err, output)
	}
	hooksDirectory := filepath.Join(scratch, "hooks")
	if err := os.MkdirAll(hooksDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{HookReferenceTransaction, HookPostCheckout, "pre-commit"} {
		script := "#!/bin/sh\nexec " + hookBinary + " " + name + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(hooksDirectory, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	repository := filepath.Join(scratch, "repo")
	worktree := filepath.Join(scratch, "repo-wt-feature")
	run := func(sessionID, directory string, arguments ...string) string {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = directory
		command.Env = append(os.Environ(),
			"HOME="+home, "LLM_BRIDGE_SESSION_ID="+sessionID,
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@example.com",
			"GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@example.com")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	run("", repository, "init", "-q", "-b", "main")
	run("", repository, "config", "core.hooksPath", hooksDirectory)
	// The repository's own hook must still run, with its arguments.
	write(filepath.Join(repository, ".git", "hooks", "pre-commit"), "#!/bin/sh\ntouch \"$GIT_DIR/../own-hook-ran\" 2>/dev/null || touch own-hook-ran\n")
	if err := os.Chmod(filepath.Join(repository, ".git", "hooks", "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}

	write(filepath.Join(repository, "a"), "a")
	run("br_first", repository, "add", "a")
	run("br_first", repository, "commit", "-qm", "first on main")
	rootCommit := run("", repository, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(repository, "own-hook-ran")); err != nil {
		t.Errorf("the repository's own pre-commit hook did not run: %v", err)
	}

	run("br_second", repository, "worktree", "add", "-q", worktree, "-b", "feature")
	write(filepath.Join(worktree, "b"), "b")
	run("br_second", worktree, "add", "b")
	run("br_second", worktree, "commit", "-qm", "feature work")

	write(filepath.Join(repository, "c"), "c")
	run("br_first", repository, "add", "c")
	run("br_first", repository, "commit", "-qm", "second on main")

	// The first session rebases the second's branch onto main and merges it.
	run("br_first", worktree, "rebase", "-q", "main")
	rebasedFeature := run("", repository, "rev-parse", "feature")
	run("br_first", repository, "merge", "-q", "--ff-only", "feature")

	store, err := Open(filepath.Join(scratch, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fakeRepoStore := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		repo := Repo{ID: 7, Name: "repo", Path: repository}
		if request.URL.Path == "/repos" {
			json.NewEncoder(writer).Encode([]Repo{repo})
			return
		}
		json.NewEncoder(writer).Encode(repo)
	}))
	defer fakeRepoStore.Close()
	repoStore := &RepoStoreClient{BaseURL: fakeRepoStore.URL, HTTP: fakeRepoStore.Client()}

	ingester := &Ingester{Store: store, SpoolPath: filepath.Join(home, ".local", "state", "work-graph-store", "ref-updates.jsonl"), RepoStore: repoStore, SettleDelay: time.Millisecond}
	if err := ingester.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ingester.SpoolPath); !os.IsNotExist(err) {
		t.Errorf("the spool is still there after a pass: %v", err)
	}

	graph, err := BuildGraph(context.Background(), store, Repo{ID: 7, Name: "repo", Path: repository}, 50)
	if err != nil {
		t.Fatal(err)
	}
	madeBy := map[string]string{}
	for _, commit := range graph.Commits {
		if commit.MadeBy == nil {
			t.Errorf("commit %q has no maker", commit.Subject)
			continue
		}
		madeBy[commit.Subject] = commit.MadeBy.SessionID
	}
	// The rebase rewrote "feature work", so the rebased commit is the first
	// session's; the second session's original is no longer on any branch.
	want := map[string]string{"first on main": "br_first", "second on main": "br_first", "feature work": "br_first"}
	for subject, session := range want {
		if madeBy[subject] != session {
			t.Errorf("%q made by %q, want %q", subject, madeBy[subject], session)
		}
	}
	if graph.Commits[len(graph.Commits)-1].SHA != rootCommit {
		t.Errorf("the oldest commit listed is not the root")
	}
	if graph.DefaultBranch != "main" {
		t.Errorf("default branch %q", graph.DefaultBranch)
	}

	var feature *Branch
	for index := range graph.Branches {
		if graph.Branches[index].Name == "feature" {
			feature = &graph.Branches[index]
		}
	}
	if feature == nil {
		t.Fatalf("no feature branch in %+v", graph.Branches)
	}
	if feature.HeadSHA != rebasedFeature || !feature.MergedIntoDefault || feature.WorktreePath != worktree {
		t.Errorf("feature = head %s merged %v worktree %q", feature.HeadSHA, feature.MergedIntoDefault, feature.WorktreePath)
	}
	var sessions []string
	for _, session := range feature.Sessions {
		sessions = append(sessions, session.SessionID+":"+strings.Join(session.GitSubcommands, ","))
	}
	if strings.Join(sessions, " ") != "br_second:branch,commit,worktree br_first:rebase" {
		t.Errorf("feature's sessions = %v", sessions)
	}

	// The second session's own commit, before the rebase, is still credited to
	// it when asked about by sha.
	updates, err := store.ListRefUpdates(RefUpdateFilter{SessionID: "br_second", Ref: "refs/heads/feature"})
	if err != nil {
		t.Fatal(err)
	}
	var originalFeatureCommit string
	for _, update := range updates {
		if update.GitSubcommand == "commit" {
			originalFeatureCommit = update.NewSHA
		}
	}
	parent := run("", repository, "rev-parse", originalFeatureCommit+"^")
	authorships, err := store.CommitAuthorships(7, map[string]string{originalFeatureCommit: parent})
	if err != nil {
		t.Fatal(err)
	}
	if authorships[originalFeatureCommit].SessionID != "br_second" {
		t.Errorf("the pre-rebase feature commit is credited to %q", authorships[originalFeatureCommit].SessionID)
	}
}

func TestGitSubcommandOfSkipsGlobalOptions(t *testing.T) {
	cases := map[string]string{
		"git commit -m x": "commit",
		"/usr/lib/git-core/git branch --quiet wt2 m": "branch",
		"git -C /tmp/x -c a=b rebase -q main":        "rebase",
		"git --no-pager --git-dir /x log":            "log",
		"git":                                        "",
	}
	for commandLine, want := range cases {
		if got := GitSubcommandOf(strings.Fields(commandLine)); got != want {
			t.Errorf("GitSubcommandOf(%q) = %q, want %q", commandLine, got, want)
		}
	}
}
