package workgraphstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A mention names a commit by repo-store name or by GitHub owner/name, with a
// full or short sha, and the answer carries the full sha and the repo as
// repo-store has it.
func TestCommitsByMentionFindsTheCommitAndItsRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	scratch := t.TempDir()
	repository := filepath.Join(scratch, "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = repository
		command.Env = append(os.Environ(),
			"HOME="+scratch, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@example.com",
			"GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@example.com")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "first")
	first := run("rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "second")
	second := run("rev-parse", "HEAD")

	repo := Repo{ID: 7, Name: "dash", Path: repository, GitHubURL: "https://github.com/kayushkin/dash"}
	fakeRepoStore := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos":
			json.NewEncoder(writer).Encode([]Repo{repo, {ID: 8, Name: "other", Path: scratch}})
		case "/repos/by-name/dash":
			json.NewEncoder(writer).Encode(repo)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer fakeRepoStore.Close()
	store, err := Open(filepath.Join(scratch, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Handlers{Store: store, RepoStore: &RepoStoreClient{BaseURL: fakeRepoStore.URL, HTTP: fakeRepoStore.Client()}})

	get := func(mention string) (int, CommitAnswer) {
		t.Helper()
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/commits/"+mention, nil))
		var answer CommitAnswer
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
				t.Fatal(err)
			}
		}
		return recorder.Code, answer
	}

	for _, mention := range []string{"dash@" + second[:7], "dash@" + second, "kayushkin/dash@" + second[:10], "Kayushkin/Dash@" + strings.ToUpper(second[:7])} {
		status, answer := get(mention)
		if status != http.StatusOK {
			t.Errorf("%s: status %d", mention, status)
			continue
		}
		if answer.Commit.SHA != second || answer.Commit.Subject != "second" || strings.Join(answer.Commit.Parents, ",") != first {
			t.Errorf("%s: commit %+v", mention, answer.Commit)
		}
		if answer.Repo.ID != 7 || answer.Repo.GitHubURL != repo.GitHubURL {
			t.Errorf("%s: repo %+v", mention, answer.Repo)
		}
	}

	// origin/main holds the first commit only: the second was never pushed to
	// origin, only to another remote, whose branches do not count.
	run("update-ref", "refs/remotes/origin/main", first)
	run("update-ref", "refs/remotes/upstream/main", second)
	if _, answer := get("dash@" + first[:7]); !answer.Commit.OnOriginBranch {
		t.Errorf("the pushed commit reads as not on an origin branch")
	}
	if _, answer := get("dash@" + second[:7]); answer.Commit.OnOriginBranch {
		t.Errorf("the unpushed commit reads as on an origin branch")
	}
	graph, err := BuildGraph(t.Context(), store, repo, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, commit := range graph.Commits {
		if commit.OnOriginBranch != (commit.SHA == first) {
			t.Errorf("graph: %q on_origin_branch = %v", commit.Subject, commit.OnOriginBranch)
		}
	}

	for mention, want := range map[string]int{
		"nope@" + second[:7]:         http.StatusNotFound,   // no such repo
		"someone/dash@" + second[:7]: http.StatusNotFound,   // no repo at that GitHub page
		"dash@0000000":               http.StatusNotFound,   // no such commit
		"dash@" + second[:6]:         http.StatusBadRequest, // too short to be a mention
		"dash@--help1":               http.StatusBadRequest, // not hex, so never handed to git
		"dash":                       http.StatusBadRequest,
	} {
		if status, _ := get(mention); status != want {
			t.Errorf("%s: status %d, want %d", mention, status, want)
		}
	}
}
