package repostore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the pull-request list endpoint from a slice per
// repository, newest update first, paged as GitHub pages it. It records every
// path asked for.
type fakeGitHub struct {
	mutex          sync.Mutex
	pullRequests   map[string][]map[string]any // "owner/name" -> GitHub pull request objects
	failingRepos   map[string]error
	requestedPaths []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{pullRequests: map[string][]map[string]any{}, failingRepos: map[string]error{}}
}

func githubTime(unixSeconds int64) string {
	return time.Unix(unixSeconds, 0).UTC().Format(time.RFC3339)
}

// add puts a pull request on the fake. mergedAt and closedAt are 0 when not set.
func (github *fakeGitHub) add(repository string, number int64, state string, updatedAt, mergedAt, closedAt int64) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	object := map[string]any{
		"number": number, "title": fmt.Sprintf("pull request %d", number), "state": state, "draft": number%2 == 0,
		"html_url":   "https://github.com/" + repository + "/pull/" + strconv.FormatInt(number, 10),
		"created_at": githubTime(updatedAt - 100), "updated_at": githubTime(updatedAt),
		"merged_at": nil, "closed_at": nil,
		"user": map[string]any{"login": "example-owner"},
		"head": map[string]any{"ref": fmt.Sprintf("branch-%d", number), "sha": strings.Repeat("a", 40)},
		"base": map[string]any{"ref": "main"},
	}
	if mergedAt != 0 {
		object["merged_at"] = githubTime(mergedAt)
	}
	if closedAt != 0 {
		object["closed_at"] = githubTime(closedAt)
	}
	list := github.pullRequests[repository]
	// Keep the list newest update first, as GitHub sorts with sort=updated&direction=desc.
	index := 0
	for index < len(list) && list[index]["updated_at"].(string) >= object["updated_at"].(string) {
		index++
	}
	list = append(list, nil)
	copy(list[index+1:], list[index:])
	list[index] = object
	github.pullRequests[repository] = list
}

func (github *fakeGitHub) Get(_ context.Context, path string) ([]byte, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.requestedPaths = append(github.requestedPaths, path)
	parsed, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(parsed.Path, "/")
	if len(segments) != 4 || segments[0] != "repos" || segments[3] != "pulls" {
		return nil, fmt.Errorf("fake GitHub has no route %s", path)
	}
	repository := segments[1] + "/" + segments[2]
	if err := github.failingRepos[repository]; err != nil {
		return nil, err
	}
	query := parsed.Query()
	if query.Get("state") != "all" || query.Get("sort") != "updated" || query.Get("direction") != "desc" {
		return nil, fmt.Errorf("fake GitHub: %s does not ask for every state, most recently updated first", path)
	}
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	page, _ := strconv.Atoi(query.Get("page"))
	list := github.pullRequests[repository]
	start, end := (page-1)*perPage, page*perPage
	if start > len(list) {
		start = len(list)
	}
	if end > len(list) {
		end = len(list)
	}
	return json.Marshal(list[start:end])
}

func (github *fakeGitHub) pathsRequested() []string {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	return append([]string(nil), github.requestedPaths...)
}

func (github *fakeGitHub) forgetRequests() {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.requestedPaths = nil
}

func repoOnGitHub(t *testing.T, store *Store, name string) *Repo {
	t.Helper()
	repo := &Repo{Name: name, GitRemote: "https://github.com/example-owner/" + name + ".git"}
	if _, err := store.UpsertRepo(repo); err != nil {
		t.Fatal(err)
	}
	if repo.GitHubURL != "https://github.com/example-owner/"+name {
		t.Fatalf("github_url of %s = %q", name, repo.GitHubURL)
	}
	return repo
}

func numbersOf(pullRequests []PullRequest) []int64 {
	out := []int64{}
	for _, pullRequest := range pullRequests {
		out = append(out, pullRequest.Number)
	}
	return out
}

func TestTheStoreListsPullRequestsNewestUpdateFirstAndFiltersByRepoAndState(t *testing.T) {
	store := openTest(t)
	dash, bus := repoOnGitHub(t, store, "dash"), repoOnGitHub(t, store, "bus")
	write := func(repoID int64, pullRequests ...PullRequest) {
		t.Helper()
		if err := store.RecordPullRequestSync(repoID, PullRequestSyncCursor{GitHubURL: "u", StartedAt: 1}, pullRequests); err != nil {
			t.Fatal(err)
		}
	}
	write(dash.ID,
		PullRequest{Number: 1, State: PullRequestStateMerged, UpdatedAt: 100, MergedAt: 90, ClosedAt: 90},
		PullRequest{Number: 2, State: PullRequestStateOpen, UpdatedAt: 300, IsDraft: true},
		PullRequest{Number: 3, State: PullRequestStateClosed, UpdatedAt: 200, ClosedAt: 200})
	write(bus.ID, PullRequest{Number: 1, State: PullRequestStateOpen, UpdatedAt: 250})

	everything, err := store.ListPullRequests(PullRequestListFilter{State: PullRequestStateQueryAll, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(numbersOf(everything)) != "[2 1 3 1]" || everything[1].RepoID != bus.ID {
		t.Fatalf("every pull request = %+v, want dash#2, bus#1, dash#3, dash#1", everything)
	}
	if !everything[0].IsDraft || everything[0].SyncedAt == 0 {
		t.Errorf("dash#2 read back as %+v, want a draft with synced_at set", everything[0])
	}

	for state, want := range map[string]string{"open": "[2 1]", "closed": "[3]", "merged": "[1]"} {
		got, err := store.ListPullRequests(PullRequestListFilter{State: state, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(numbersOf(got)) != want {
			t.Errorf("state %s = %v, want %s", state, numbersOf(got), want)
		}
	}
	ofDash, err := store.ListPullRequests(PullRequestListFilter{RepoID: dash.ID, State: PullRequestStateQueryAll, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(numbersOf(ofDash)) != "[2 3]" {
		t.Errorf("dash with limit 2 = %v, want [2 3]", numbersOf(ofDash))
	}
	if _, err := store.ListPullRequests(PullRequestListFilter{RepoID: 999, State: PullRequestStateQueryAll, Limit: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown repo id = %v, want ErrNotFound", err)
	}

	// A second write of the same number replaces the row rather than adding one.
	write(dash.ID, PullRequest{Number: 2, State: PullRequestStateMerged, UpdatedAt: 400, MergedAt: 400, ClosedAt: 400, Title: "renamed"})
	ofDash, _ = store.ListPullRequests(PullRequestListFilter{RepoID: dash.ID, State: PullRequestStateQueryAll, Limit: 100})
	if len(ofDash) != 3 || ofDash[0].Number != 2 || ofDash[0].State != PullRequestStateMerged || ofDash[0].Title != "renamed" || ofDash[0].IsDraft {
		t.Errorf("after rewriting dash#2: %+v", ofDash)
	}

	// Deleting a repo takes its pull requests and its cursor with it.
	if err := store.DeleteRepo(dash.ID); err != nil {
		t.Fatal(err)
	}
	everything, _ = store.ListPullRequests(PullRequestListFilter{State: PullRequestStateQueryAll, Limit: 100})
	if len(everything) != 1 || everything[0].RepoID != bus.ID {
		t.Errorf("after deleting dash: %+v", everything)
	}
	if _, synced, _ := store.PullRequestSyncCursorOf(dash.ID); synced {
		t.Error("the deleted repo's sync cursor survived it")
	}
}

func TestTheStoreRefusesAPullRequestItCouldNotListAndWritesNoneOfTheBatch(t *testing.T) {
	store := openTest(t)
	dash := repoOnGitHub(t, store, "dash")
	for _, bad := range []PullRequest{
		{Number: 0, State: PullRequestStateOpen, UpdatedAt: 1},
		{Number: 1, State: "draft", UpdatedAt: 1},
		{Number: 1, State: PullRequestStateOpen},
	} {
		err := store.RecordPullRequestSync(dash.ID, PullRequestSyncCursor{GitHubURL: "u", StartedAt: 1},
			[]PullRequest{{Number: 9, State: PullRequestStateOpen, UpdatedAt: 1}, bad})
		if err == nil {
			t.Errorf("%+v was stored", bad)
		}
	}
	stored, _ := store.ListPullRequests(PullRequestListFilter{State: PullRequestStateQueryAll, Limit: 100})
	if len(stored) != 0 {
		t.Errorf("a refused batch wrote %+v", stored)
	}
	if _, synced, _ := store.PullRequestSyncCursorOf(dash.ID); synced {
		t.Error("a refused batch moved the cursor")
	}
}

func TestFetchingReadsEveryPageThenOnlyWhatChangedSince(t *testing.T) {
	github := newFakeGitHub()
	// 101 pull requests: one more than a page. The newest is #101.
	for number := int64(1); number <= 101; number++ {
		github.add("example-owner/northwind-api", number, "open", 1000+number, 0, 0)
	}
	every, err := FetchPullRequestsUpdatedSince(context.Background(), github, "example-owner", "northwind-api", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(every) != 101 || every[0].Number != 101 || every[100].Number != 1 {
		t.Fatalf("read %d pull requests, first %d", len(every), every[0].Number)
	}
	if paths := github.pathsRequested(); len(paths) != 2 || !strings.HasSuffix(paths[1], "page=2") {
		t.Errorf("asked for %v, want pages 1 and 2", paths)
	}

	github.forgetRequests()
	recent, err := FetchPullRequestsUpdatedSince(context.Background(), github, "example-owner", "northwind-api", 1099)
	if err != nil {
		t.Fatal(err)
	}
	// Updated at 1099, 1100 and 1101: the one in the same second as since is read again.
	if fmt.Sprint(numbersOf(recent)) != "[101 100 99]" {
		t.Errorf("since 1099 read %v", numbersOf(recent))
	}
	if paths := github.pathsRequested(); len(paths) != 1 {
		t.Errorf("since 1099 asked for %v, want the first page only", paths)
	}
}

func TestGitHubStatesBecomeOpenClosedOrMerged(t *testing.T) {
	github := newFakeGitHub()
	github.add("o/n", 1, "open", 10, 0, 0)
	github.add("o/n", 2, "closed", 20, 0, 20)
	github.add("o/n", 3, "closed", 30, 30, 30)
	github.add("o/n", 4, "locked", 40, 0, 0)
	_, err := FetchPullRequestsUpdatedSince(context.Background(), github, "o", "n", 0)
	if err == nil || !strings.Contains(err.Error(), `state "locked"`) {
		t.Fatalf("a state GitHub does not use = %v, want it named", err)
	}
	github.pullRequests["o/n"] = github.pullRequests["o/n"][1:]
	read, err := FetchPullRequestsUpdatedSince(context.Background(), github, "o", "n", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]PullRequest{}
	for _, pullRequest := range read {
		got[pullRequest.Number] = pullRequest
	}
	if got[1].State != PullRequestStateOpen || got[1].ClosedAt != 0 || got[1].MergedAt != 0 {
		t.Errorf("#1 = %+v, want open with no close or merge time", got[1])
	}
	if got[2].State != PullRequestStateClosed || got[2].ClosedAt != 20 || got[2].MergedAt != 0 {
		t.Errorf("#2 = %+v, want closed at 20", got[2])
	}
	if got[3].State != PullRequestStateMerged || got[3].MergedAt != 30 {
		t.Errorf("#3 = %+v, want merged at 30", got[3])
	}
	if got[2].Author != "example-owner" || got[2].HeadRef != "branch-2" || got[2].BaseRef != "main" || got[2].URL != "https://github.com/o/n/pull/2" || !got[2].IsDraft || got[2].CreatedAt != -80 {
		t.Errorf("#2 fields = %+v", got[2])
	}
}

func TestTheSyncerStartsFromTheLastSyncAndOverWhenTheGitHubRepositoryChanges(t *testing.T) {
	store := openTest(t)
	github := newFakeGitHub()
	syncer := NewPullRequestSyncer(store, github)
	dash := repoOnGitHub(t, store, "dash")
	github.add("example-owner/dash", 1, "closed", 100, 100, 100)

	written, err := syncer.SyncRepo(context.Background(), dash)
	if err != nil || written != 1 {
		t.Fatalf("first sync wrote %d: %v", written, err)
	}
	cursor, synced, err := store.PullRequestSyncCursorOf(dash.ID)
	if err != nil || !synced || cursor.GitHubURL != dash.GitHubURL || cursor.StartedAt == 0 {
		t.Fatalf("cursor after the first sync = %+v %v %v", cursor, synced, err)
	}

	// Nothing changed on GitHub since: the next sync reads the first page,
	// finds only what is older than its cursor, and writes nothing.
	written, err = syncer.SyncRepo(context.Background(), dash)
	if err != nil || written != 0 {
		t.Fatalf("second sync wrote %d: %v", written, err)
	}
	github.add("example-owner/dash", 2, "open", now()+5, 0, 0)
	written, err = syncer.SyncRepo(context.Background(), dash)
	if err != nil || written != 1 {
		t.Fatalf("sync after a new pull request wrote %d: %v", written, err)
	}

	// The remote moves to another GitHub repository: the cursor belongs to the
	// old one, so the sync reads the new one from the start.
	renamed, err := store.SetIdentity(dash.ID, RepoIdentity{Name: "dash", Path: "/repos/dash", GitRemote: "https://github.com/example-owner/dashboard.git"})
	if err != nil {
		t.Fatal(err)
	}
	github.add("example-owner/dashboard", 7, "open", 50, 0, 0)
	written, err = syncer.SyncRepo(context.Background(), renamed)
	if err != nil || written != 1 {
		t.Fatalf("sync after the remote moved wrote %d: %v", written, err)
	}
}

func TestAFailedSyncWritesNothingAndKeepsTheCursor(t *testing.T) {
	store := openTest(t)
	github := newFakeGitHub()
	syncer := NewPullRequestSyncer(store, github)
	dash := repoOnGitHub(t, store, "dash")
	github.failingRepos["example-owner/dash"] = errors.New("HTTP 403: API rate limit exceeded")
	if _, err := syncer.SyncRepo(context.Background(), dash); !errors.Is(err, ErrGitHubRequestFailed) || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("SyncRepo = %v, want a GitHub failure carrying GitHub's words", err)
	}
	if _, synced, _ := store.PullRequestSyncCursorOf(dash.ID); synced {
		t.Error("a failed sync moved the cursor")
	}
	noRemote := &Repo{Name: "local-only"}
	if _, err := store.UpsertRepo(noRemote); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.SyncRepo(context.Background(), noRemote); !errors.Is(err, ErrRepoHasNoGitHubURL) {
		t.Errorf("syncing a repo with no github_url = %v", err)
	}
}

func TestSyncingEveryRepoCarriesOnPastAFailingOne(t *testing.T) {
	store := openTest(t)
	github := newFakeGitHub()
	syncer := NewPullRequestSyncer(store, github)
	broken, working := repoOnGitHub(t, store, "aaa-broken"), repoOnGitHub(t, store, "bbb-working")
	if _, err := store.UpsertRepo(&Repo{Name: "local-only"}); err != nil {
		t.Fatal(err)
	}
	github.failingRepos["example-owner/aaa-broken"] = errors.New("HTTP 404: Not Found")
	github.add("example-owner/bbb-working", 1, "open", 10, 0, 0)

	syncer.SyncEveryRepo(context.Background())
	stored, _ := store.ListPullRequests(PullRequestListFilter{State: PullRequestStateQueryAll, Limit: 100})
	if len(stored) != 1 || stored[0].RepoID != working.ID {
		t.Fatalf("after one pass: %+v, want the working repo's pull request", stored)
	}
	if _, synced, _ := store.PullRequestSyncCursorOf(broken.ID); synced {
		t.Error("the failing repo got a cursor")
	}
	for _, path := range github.pathsRequested() {
		if strings.Contains(path, "local-only") {
			t.Errorf("the repo with no github_url was asked for: %s", path)
		}
	}
}

func TestOwnerAndNameComeOnlyFromAGitHubRepositoryPage(t *testing.T) {
	owner, name, err := ownerAndNameOfGitHubURL("https://github.com/example-owner/dash")
	if err != nil || owner != "example-owner" || name != "dash" {
		t.Errorf("= %q %q %v", owner, name, err)
	}
	for _, bad := range []string{"", "https://gitlab.com/a/b", "https://github.com/a", "https://github.com/a/b/c", "https://github.com//b"} {
		if _, _, err := ownerAndNameOfGitHubURL(bad); err == nil {
			t.Errorf("%q was split", bad)
		}
	}
}

// pullRequestServer is the API over a store and a fake GitHub.
func pullRequestServer(t *testing.T) (*Store, *fakeGitHub, http.Handler) {
	t.Helper()
	store := openTest(t)
	github := newFakeGitHub()
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	RegisterPullRequestHandlers(mux, store, NewPullRequestSyncer(store, github))
	return store, github, mux
}

func call(t *testing.T, handler http.Handler, method, target string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder.Code, recorder.Body.String()
}

func decodePullRequestList(t *testing.T, body string) []PullRequest {
	t.Helper()
	var list struct {
		PullRequests []PullRequest `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if list.PullRequests == nil {
		t.Fatalf("pull_requests is missing or null: %s", body)
	}
	return list.PullRequests
}

func TestSyncRouteReadsGitHubAndAnswersEveryStateOfTheRepo(t *testing.T) {
	store, github, server := pullRequestServer(t)
	dash := repoOnGitHub(t, store, "dash")
	github.add("example-owner/dash", 1, "closed", 100, 100, 100)
	github.add("example-owner/dash", 2, "open", 200, 0, 0)
	github.add("example-owner/dash", 3, "closed", 300, 0, 300)

	status, body := call(t, server, "POST", fmt.Sprintf("/repos/%d/pull-requests/sync", dash.ID))
	if status != 200 {
		t.Fatalf("sync = %d %s", status, body)
	}
	synced := decodePullRequestList(t, body)
	if fmt.Sprint(numbersOf(synced)) != "[3 2 1]" || synced[2].State != PullRequestStateMerged || synced[0].RepoID != dash.ID {
		t.Fatalf("sync answered %+v", synced)
	}
	// The wire names the caller depends on.
	var raw struct {
		PullRequests []map[string]any `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"repo_id", "number", "title", "state", "is_draft", "author", "head_ref", "base_ref", "head_sha", "url", "created_at", "updated_at", "merged_at", "closed_at", "synced_at"} {
		if _, present := raw.PullRequests[0][field]; !present {
			t.Errorf("a pull request on the wire has no %s: %v", field, raw.PullRequests[0])
		}
	}

	status, body = call(t, server, "GET", "/pull-requests")
	if status != 200 || fmt.Sprint(numbersOf(decodePullRequestList(t, body))) != "[2]" {
		t.Errorf("GET /pull-requests with no parameters = %d %s, want the open one only", status, body)
	}
	status, body = call(t, server, "GET", fmt.Sprintf("/pull-requests?repo_id=%d&state=all&limit=2", dash.ID))
	if status != 200 || fmt.Sprint(numbersOf(decodePullRequestList(t, body))) != "[3 2]" {
		t.Errorf("state=all limit=2 = %d %s", status, body)
	}
	status, body = call(t, server, "GET", "/pull-requests?state=merged")
	if status != 200 || fmt.Sprint(numbersOf(decodePullRequestList(t, body))) != "[1]" {
		t.Errorf("state=merged = %d %s", status, body)
	}
}

func TestSyncRouteRefusals(t *testing.T) {
	store, github, server := pullRequestServer(t)
	dash := repoOnGitHub(t, store, "dash")
	noRemote := &Repo{Name: "local-only", GitRemote: "git@gitlab.example:a/b.git"}
	sshHostResolver = func(host string) (string, error) { return host, nil }
	t.Cleanup(func() { sshHostResolver = resolveSSHHostWithSSHConfig })
	if _, err := store.UpsertRepo(noRemote); err != nil {
		t.Fatal(err)
	}
	github.failingRepos["example-owner/dash"] = errors.New("gh: HTTP 401: Bad credentials")

	for _, c := range []struct {
		target   string
		status   int
		contains string
	}{
		{fmt.Sprintf("/repos/%d/pull-requests/sync", noRemote.ID), 400, "github_url"},
		{fmt.Sprintf("/repos/%d/pull-requests/sync", dash.ID), 502, "Bad credentials"},
		{"/repos/999/pull-requests/sync", 404, "repo not found"},
		{"/repos/x/pull-requests/sync", 400, "invalid id"},
		{fmt.Sprintf("/repos/%d/pull-requests/sync?state=all", dash.ID), 400, `unknown query parameter \"state\"; accepted: none`},
	} {
		status, body := call(t, server, "POST", c.target)
		if status != c.status || !strings.Contains(body, c.contains) {
			t.Errorf("POST %s = %d %s, want %d containing %q", c.target, status, body, c.status, c.contains)
		}
	}
}

func TestListRouteParameterValidation(t *testing.T) {
	store, _, server := pullRequestServer(t)
	dash := repoOnGitHub(t, store, "dash")
	for _, c := range []struct {
		query    string
		status   int
		contains string
	}{
		{"", 200, `"pull_requests":[]`},
		{fmt.Sprintf("?repo_id=%d", dash.ID), 200, `"pull_requests":[]`},
		{"?state=all&limit=1", 200, `"pull_requests":[]`},
		{"?repo_id=999", 404, "repo not found"},
		{"?repo_id=", 400, "repo_id"},
		{"?repo_id=dash", 400, "repo_id"},
		{"?repo_id=-1", 400, "repo_id"},
		{"?state=", 400, "unknown state"},
		{"?state=draft", 400, "accepted: all, closed, merged, open"},
		{"?limit=0", 400, "limit"},
		{"?limit=ten", 400, "limit"},
		{"?limit=", 400, "limit"},
		{"?repo=dash", 400, `unknown query parameter \"repo\"; accepted: repo_id, state, limit`},
		{"?State=open&q=x", 400, `unknown query parameter \"State\", \"q\"`},
	} {
		status, body := call(t, server, "GET", "/pull-requests"+c.query)
		if status != c.status || !strings.Contains(body, c.contains) {
			t.Errorf("GET /pull-requests%s = %d %s, want %d containing %q", c.query, status, body, c.status, c.contains)
		}
	}
}
