package projectstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOwners answers like the real stores: repo 15 is "dash", board b1 is
// "Support Desk" (and only with the service token), anything else is 404.
// repoFifteenName is repo 15's name in the fake repo-store; a test renames it.
var repoFifteenName = func() string { return "dash" }

func fakeOwners(t *testing.T) *Owners {
	t.Helper()
	// Commits a day apart on main, newest first; the third is older than
	// RecentCommitWindowDays days, so it is neither recent nor activity.
	now := time.Now().Unix()
	day := int64(24 * 60 * 60)
	// noteboard's items, by id: one open todo, one done, one note.
	var noteboardLock sync.Mutex
	noteboardItems := map[string]map[string]any{
		"n-open": {"id": "n-open", "type": "todo", "title": "Ship the inbox", "status": "open", "priority": 2, "tags": []string{}, "due_at": nil,
			"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z", "completed_at": nil},
		"n-done": {"id": "n-done", "type": "todo", "title": "Write the schema", "status": "done", "priority": 1, "tags": []string{}, "due_at": nil,
			"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z", "completed_at": time.Unix(now-day, 0).UTC().Format(time.RFC3339Nano)},
		"n-design": {"id": "n-design", "type": "note", "title": "Design: one inbox", "status": "open", "priority": 0, "tags": []string{}, "due_at": nil,
			"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-03T00:00:00Z", "completed_at": nil},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/items/") && request.Method == http.MethodGet {
			noteboardLock.Lock()
			item, found := noteboardItems[strings.TrimPrefix(request.URL.Path, "/api/items/")]
			noteboardLock.Unlock()
			if !found {
				http.NotFound(writer, request)
				return
			}
			json.NewEncoder(writer).Encode(item)
			return
		}
		switch request.URL.Path {
		case "/api/items/query":
			var query struct {
				IDs          []string `json:"ids"`
				IncludeItems bool     `json:"include_items"`
			}
			json.NewDecoder(request.Body).Decode(&query)
			answer := map[string]any{"items": []any{}, "missing_ids": []string{}}
			noteboardLock.Lock()
			for _, id := range query.IDs {
				if item, found := noteboardItems[id]; found {
					answer["items"] = append(answer["items"].([]any), item)
				} else {
					answer["missing_ids"] = append(answer["missing_ids"].([]string), id)
				}
			}
			noteboardLock.Unlock()
			json.NewEncoder(writer).Encode(answer)
			return
		case "/api/items":
			var todo map[string]any
			json.NewDecoder(request.Body).Decode(&todo)
			noteboardLock.Lock()
			id := fmt.Sprintf("n-made-%d", len(noteboardItems))
			todo["id"], todo["status"], todo["created_at"], todo["updated_at"] = id, "open", "2026-10-20T00:00:00Z", "2026-10-20T00:00:00Z"
			noteboardItems[id] = todo
			noteboardLock.Unlock()
			writer.WriteHeader(http.StatusCreated)
			json.NewEncoder(writer).Encode(todo)
			return
		case "/oneshot":
			if request.Header.Get(BridgeServiceTokenHeader) != "bridge-token" {
				http.Error(writer, "no token", http.StatusUnauthorized)
				return
			}
			var oneShot map[string]any
			body, _ := io.ReadAll(request.Body)
			json.Unmarshal(body, &oneShot)
			if oneShot["model_role"] != "balanced" || oneShot["caller"] != SuggestionCaller || !strings.Contains(oneShot["prompt"].(string), "Ship the inbox") {
				http.Error(writer, "unexpected one-shot request: "+string(body), http.StatusBadRequest)
				return
			}
			writer.Write([]byte(`{"model":"claude-test","parsed":{"suggestions":[{"title":"Merge the feature branch","why":"It has waited a day."},{"title":"  ","why":"blank"},{"title":"Deploy dash","why":"Two commits are not live."}]}}`))
			return
		case "/pull-requests":
			if request.URL.Query().Get("repo_id") != "15" {
				writer.Write([]byte(`{"pull_requests":[]}`))
				return
			}
			open := map[string]any{"repo_id": 15, "number": 7, "title": "Add the inbox", "state": "open", "author": "kayushkin", "head_sha": "b", "url": "https://github.com/kayushkin/dash/pull/7", "created_at": now - day, "updated_at": now - day}
			merged := map[string]any{"repo_id": 15, "number": 6, "title": "Old work", "state": "merged", "author": "kayushkin", "url": "https://github.com/kayushkin/dash/pull/6", "created_at": now - 30*day, "merged_at": now - 2*day, "closed_at": now - 2*day}
			pullRequests := []any{open}
			if request.URL.Query().Get("state") == "all" {
				pullRequests = append(pullRequests, merged)
			}
			json.NewEncoder(writer).Encode(map[string]any{"pull_requests": pullRequests})
			return
		}
		switch request.URL.Path {
		case "/repos/15":
			json.NewEncoder(writer).Encode(map[string]any{"id": 15, "name": repoFifteenName()})
		case "/api/boards/b1":
			if request.Header.Get(KanbanServiceTokenHeader) != "service-token" {
				http.Error(writer, "no token", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"id": "b1", "name": "Support Desk"})
		case "/api/status":
			json.NewEncoder(writer).Encode(map[string]any{"services": []map[string]any{{"name": "dash-server", "unit": "dash-server"}}})
		case "/api/entities/project/project_000001/cards":
			writer.Write([]byte(`[{"card_id":"c1","item":{"title":"Waiting on a reply","status":"open"},"work_state":"waiting"},
				{"card_id":"c2","item":{"title":"Shipped","status":"done"},"work_state":"done"},
				{"card_id":"c3","item":{"title":"Column maps to nothing","status":"open","work_state":null}},
				{"card_id":"c4","item":null}]`))
		case "/repos/15/graph":
			fmt.Fprintf(writer, `{"default_branch":"main","commits":[
				{"sha":"b","parents":["a"],"committed_at":%d,"subject":"side work"},
				{"sha":"a","parents":["a0"],"committed_at":%d,"author_name":"Slava","subject":"Land the inbox","made_by":{"session_id":"br_1790000000000000002"},"on_origin_branch":true},
				{"sha":"a0","parents":["old"],"committed_at":%d,"subject":"Start"},
				{"sha":"old","parents":[],"committed_at":%d,"subject":"Ancient"}],"branches":[
				{"name":"main","head_sha":"a","merged_into_default":true},
				{"name":"feature","head_sha":"b","committed_at":100,"merged_into_default":false,"worktree_path":"/w","sessions":[{"session_id":"br_1790000000000000001"}]},
				{"name":"origin/feature","is_remote":true,"merged_into_default":false},
				{"name":"old","head_sha":"c","merged_into_default":true}]}`, now-day, now-2*day, now-3*day, now-30*day)
		case "/deployments":
			fmt.Fprintf(writer, `[{"commit_sha":"a","deployed_by":"br_1790000000000000002","created_at":%d},{"commit_sha":"abc","deployed_by":"someone","created_at":50}]`, now-day/2)
		case "/sessions/br_1790000000000000001":
			if request.Header.Get(BridgeServiceTokenHeader) != "bridge-token" {
				http.Error(writer, "no token", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"session_id": "br_1790000000000000001", "display_name": "Discord signup UI"})
		case "/api/jobs/500":
			http.Error(writer, "boom", http.StatusInternalServerError)
		default:
			if strings.HasPrefix(request.URL.Path, "/api/entities/project/") {
				writer.Write([]byte(`[]`))
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return &Owners{HTTP: server.Client(), RepoStoreURL: server.URL, KanbanStoreURL: server.URL, KanbanStoreServiceToken: "service-token",
		SchedulerURL: server.URL, HealthcheckURL: server.URL, PrincipalStoreURL: server.URL, NoteboardURL: server.URL, WorkGraphStoreURL: server.URL, BridgeURL: server.URL, BridgeServiceToken: "bridge-token",
		SuggestionModelRole: "balanced", ModelHTTP: server.Client()}
}

type client struct {
	t   *testing.T
	mux *http.ServeMux
}

func newClient(t *testing.T) client {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Handlers{Store: store, Owners: fakeOwners(t)})
	return client{t, mux}
}

func (c client) do(method, path, body string, wantStatus int) map[string]any {
	c.t.Helper()
	recorder := httptest.NewRecorder()
	c.mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	if recorder.Code != wantStatus {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, recorder.Code, wantStatus, recorder.Body)
	}
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return decoded
}

func TestProjectsNestAndRecordEveryChange(t *testing.T) {
	c := newClient(t)
	parent := c.do("POST", "/projects", `{"name":"LLM bridge","kind":"project","stage":"building","created_by":"test"}`, http.StatusCreated)
	if parent["id"] != "project_000001" || parent["spend_target_share"] != nil {
		t.Fatalf("parent = %v", parent)
	}
	child := c.do("POST", "/projects", `{"name":"Access and gating","parent_id":"project_000001","spend_target_share":0.1}`, http.StatusCreated)
	if child["id"] != "project_000002" || child["stage"] != "idea" || child["kind"] != "project" || child["spend_target_share"] != 0.1 {
		t.Fatalf("child = %v", child)
	}

	c.do("POST", "/projects", `{"name":"x","stage":"someday"}`, http.StatusBadRequest)
	c.do("POST", "/projects", `{"name":"x","parent_id":"project_000099"}`, http.StatusBadRequest)
	c.do("POST", "/projects", `{"name":"x","colour":"red"}`, http.StatusBadRequest)
	c.do("POST", "/projects", `{"name":"   "}`, http.StatusBadRequest)
	c.do("POST", "/projects", `{"name":"x","spend_target_share":1.5}`, http.StatusBadRequest)
	// A project cannot become its own ancestor.
	c.do("PATCH", "/projects/project_000001", `{"parent_id":"project_000002"}`, http.StatusBadRequest)

	c.do("PATCH", "/projects/project_000002", `{"stage":"shipping","spend_target_share":null,"changed_by":"claxon"}`, http.StatusOK)
	c.do("PATCH", "/projects/project_000002", `{"archived":true}`, http.StatusOK)
	changes := c.do("GET", "/projects/project_000002/changes", "", http.StatusOK)["changes"].([]any)
	var summary []string
	for _, raw := range changes {
		change := raw.(map[string]any)
		summary = append(summary, change["field"].(string)+":"+change["old_value"].(string)+">"+change["new_value"].(string)+"@"+change["changed_by"].(string))
	}
	if got := strings.Join(summary, " "); got != "stage:idea>shipping@claxon spend_target_share:0.1>@claxon archived:false>true@" {
		t.Errorf("changes = %s", got)
	}

	listed := c.do("GET", "/projects", "", http.StatusOK)["projects"].([]any)
	if len(listed) != 1 {
		t.Errorf("an archived project is listed by default: %v", listed)
	}
	listed = c.do("GET", "/projects?parent_id=project_000001&include_archived=true", "", http.StatusOK)["projects"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["name"] != "Access and gating" {
		t.Errorf("children = %v", listed)
	}
	c.do("GET", "/projects?colour=red", "", http.StatusBadRequest)
	c.do("GET", "/projects/project_000404", "", http.StatusNotFound)
}

func TestALinkIsCheckedWithItsOwnerAndCarriesItsName(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Ticketing"}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Chat UI"}`, http.StatusCreated)

	repo := c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15","note":"serves the desk"}`, http.StatusCreated)
	if repo["label"] != "dash" {
		t.Errorf("repo link label = %v", repo["label"])
	}
	board := c.do("POST", "/projects/project_000001/links", `{"entity_type":"board","entity_ref":"b1"}`, http.StatusCreated)
	if board["label"] != "Support Desk" {
		t.Errorf("board link label = %v", board["label"])
	}
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"service","entity_ref":"dash-server"}`, http.StatusCreated)
	// One repo, two projects.
	c.do("POST", "/projects/project_000002/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusCreated)

	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusConflict)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"999"}`, http.StatusBadRequest)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"service","entity_ref":"nope"}`, http.StatusBadRequest)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"scheduler_job","entity_ref":"500"}`, http.StatusBadGateway)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"spaceship","entity_ref":"1"}`, http.StatusBadRequest)
	c.do("POST", "/projects/project_000404/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusNotFound)

	session := c.do("POST", "/projects/project_000002/links", `{"entity_type":"session","entity_ref":"br_1790000000000000001"}`, http.StatusCreated)
	if session["label"] != "Discord signup UI" {
		t.Errorf("session link label = %v", session["label"])
	}
	c.do("POST", "/projects/project_000002/links", `{"entity_type":"session","entity_ref":"br_1"}`, http.StatusBadRequest)
	if filed := c.do("GET", "/links?entity_type=session", "", http.StatusOK)["links"].([]any); len(filed) != 1 {
		t.Errorf("every filed session = %v", filed)
	}
	c.do("GET", "/links?entity_ref=15", "", http.StatusBadRequest)

	owners := c.do("GET", "/links?entity_type=repo&entity_ref=15", "", http.StatusOK)["links"].([]any)
	if len(owners) != 2 {
		t.Errorf("repo 15 is owned by %d projects, want 2", len(owners))
	}
	c.do("DELETE", "/projects/project_000001/links/1", "", http.StatusNoContent)
	c.do("DELETE", "/projects/project_000001/links/1", "", http.StatusNotFound)
	links := c.do("GET", "/projects/project_000001/links", "", http.StatusOK)["links"].([]any)
	if len(links) != 2 {
		t.Errorf("links after delete = %v", links)
	}
}

func TestTheRollupIsWorkedOutFromWhatTheProjectOwns(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Ticketing","goal":"a help desk","spend_target_share":0.2}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Email to ticket","parent_id":"project_000001","stage":"building"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"board","entity_ref":"b1"}`, http.StatusCreated)

	rollup := c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)
	counts, _ := json.Marshal(rollup["cards_by_work_state"])
	if string(counts) != `{"unmapped":1,"waiting":1}` {
		t.Errorf("cards_by_work_state = %s", counts)
	}
	if cards := rollup["cards"].([]any); len(cards) != 3 {
		t.Errorf("cards = %v, want the orphan dropped", cards)
	}
	repos := rollup["repos"].([]any)
	branches := repos[0].(map[string]any)["unmerged_branches"].([]any)
	if len(branches) != 1 || branches[0].(map[string]any)["name"] != "feature" {
		t.Errorf("unmerged branches = %v", branches)
	}
	if children := rollup["children"].([]any); len(children) != 1 {
		t.Errorf("children = %v", children)
	}

	recorder := httptest.NewRecorder()
	c.mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/digest", nil))
	text := recorder.Body.String()
	for _, want := range []string{"Ticketing  project_000001  [project, idea]  spend target 20%", "goal: a help desk", "cards: unmapped 1, waiting 1",
		"boards: Support Desk (b1)", "repo dash (15), deployed", `open pull request #7 "Add the inbox"`, "feature, last commit", "worktree /w, sessions br_1790000000000000001",
		"\n    Email to ticket  project_000002  [project, building]"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
}

func TestNextStepsAreLinkedTodosAndSuggestionsWaitForAPerson(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging","goal":"one inbox","done_when":"every chat reads in one list"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusCreated)
	for _, note := range []string{"n-open", "n-done", "n-design", "n-gone"} {
		c.mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/projects/project_000001/links", strings.NewReader(`{"entity_type":"note","entity_ref":"`+note+`"}`)))
	}

	step := c.do("POST", "/projects/project_000001/next-steps", `{"title":"Read Signal too","priority":3,"created_by":"test"}`, http.StatusCreated)
	if step["type"] != "todo" || step["status"] != "open" || step["title"] != "Read Signal too" {
		t.Fatalf("next step = %v", step)
	}
	c.do("POST", "/projects/project_000001/next-steps", `{"title":"  "}`, http.StatusBadRequest)
	c.do("POST", "/projects/project_000001/next-steps", `{"title":"x","colour":"red"}`, http.StatusBadRequest)
	c.do("POST", "/projects/project_000404/next-steps", `{"title":"x"}`, http.StatusNotFound)

	rollup := c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)
	var order []string
	for _, raw := range rollup["notes"].([]any) {
		order = append(order, raw.(map[string]any)["title"].(string))
	}
	// Open todos first, most urgent first; the rest newest first.
	if got := strings.Join(order, " | "); got != "Read Signal too | Ship the inbox | Design: one inbox | Write the schema" {
		t.Errorf("notes in order = %s", got)
	}
	repo := rollup["repos"].([]any)[0].(map[string]any)
	var commits []string
	for _, raw := range repo["recent_commits"].([]any) {
		commit := raw.(map[string]any)
		commits = append(commits, commit["sha"].(string)+":"+commit["session_id"].(string))
	}
	if got := strings.Join(commits, " "); got != "a:br_1790000000000000002 a0:" {
		t.Errorf("recent commits on main = %s, want first parents from the head, none older than the window", got)
	}
	if pullRequests := repo["open_pull_requests"].([]any); len(pullRequests) != 1 || pullRequests[0].(map[string]any)["number"] != 7.0 {
		t.Errorf("open pull requests = %v", pullRequests)
	}

	suggested := c.do("POST", "/projects/project_000001/suggestions", `{"requested_by":"test"}`, http.StatusCreated)["suggestions"].([]any)
	if len(suggested) != 2 || suggested[0].(map[string]any)["model"] != "claude-test" || suggested[0].(map[string]any)["status"] != "proposed" {
		t.Fatalf("suggestions = %v, want the blank one dropped", suggested)
	}
	again := c.do("POST", "/projects/project_000001/suggestions", `{}`, http.StatusCreated)["suggestions"].([]any)
	if superseded := c.do("GET", "/projects/project_000001/suggestions?status=superseded", "", http.StatusOK)["suggestions"].([]any); len(superseded) != 2 {
		t.Errorf("a second round leaves %d superseded, want 2", len(superseded))
	}
	c.do("GET", "/projects/project_000001/suggestions?status=maybe", "", http.StatusBadRequest)
	if pending := c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)["suggestions"].([]any); len(pending) != 2 {
		t.Errorf("rollup suggestions = %v", pending)
	}

	acceptID := again[0].(map[string]any)["id"].(string)
	accepted := c.do("POST", "/suggestions/"+acceptID+"/accept", `{"decided_by":"slava"}`, http.StatusOK)
	note := accepted["note"].(map[string]any)
	if accepted["suggestion"].(map[string]any)["status"] != "accepted" || accepted["suggestion"].(map[string]any)["note_id"] != note["note_id"] || note["title"] != "Merge the feature branch" {
		t.Errorf("accepted = %v", accepted)
	}
	c.do("POST", "/suggestions/"+acceptID+"/accept", `{}`, http.StatusConflict)
	c.do("POST", "/suggestions/"+again[1].(map[string]any)["id"].(string)+"/dismiss", `{"decided_by":"slava"}`, http.StatusOK)
	c.do("POST", "/suggestions/suggestion_000999/dismiss", `{}`, http.StatusNotFound)

	activity := c.do("GET", "/projects/project_000001/activity", "", http.StatusOK)["events"].([]any)
	kinds := map[string]int{}
	for _, raw := range activity {
		kinds[raw.(map[string]any)["kind"].(string)]++
	}
	want := map[string]int{ActivityLinkAdded: 6, ActivityCommitLanded: 2, ActivityDeployed: 1, ActivityPullRequestOpened: 1, ActivityPullRequestMerged: 1,
		ActivityNoteCompleted: 1, ActivitySuggestionAccepted: 1, ActivitySuggestionDismissed: 1}
	for kind, count := range want {
		if kinds[kind] != count {
			t.Errorf("%d %s events, want %d (all: %v)", kinds[kind], kind, count, kinds)
		}
	}
	for index := 1; index < len(activity); index++ {
		if activity[index-1].(map[string]any)["at"].(float64) < activity[index].(map[string]any)["at"].(float64) {
			t.Fatalf("activity is not newest first at %d", index)
		}
	}
	c.do("GET", "/projects/project_000001/activity?days=0", "", http.StatusBadRequest)
	c.do("GET", "/projects/project_000001/activity?since=1", "", http.StatusBadRequest)
	c.do("GET", "/projects/project_000404/activity", "", http.StatusNotFound)
}

func TestARenamedRepoIsNamedAsRepoStoreNamesItNow(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Orchestrator"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusCreated)
	repoFifteenName = func() string { return "dash-renamed" }
	t.Cleanup(func() { repoFifteenName = func() string { return "dash" } })

	repo := c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)["repos"].([]any)[0].(map[string]any)
	if repo["name"] != "dash-renamed" {
		t.Errorf("rollup names the repo %v, want its name now", repo["name"])
	}
	for _, raw := range c.do("GET", "/projects/project_000001/activity", "", http.StatusOK)["events"].([]any) {
		event := raw.(map[string]any)
		if event["kind"] == ActivityCommitLanded && event["repo_name"] != "dash-renamed" {
			t.Fatalf("a commit event names the repo %v", event["repo_name"])
		}
		if event["kind"] == ActivityLinkAdded && event["entity_type"] == "repo" && event["label"] != "dash-renamed" {
			t.Fatalf("the link event names the repo %v", event["label"])
		}
	}
}
