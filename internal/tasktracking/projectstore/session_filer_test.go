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

// fakeChat is one session in the fake llm-bridge-server.
type fakeChat struct {
	id, name, origin, manager string
	updatedAgo                time.Duration
	// goal is the brief's overall goal; "" means the chat has no brief.
	goal string
	// goalChangedIn is when the brief recorded a changed goal, as an offset
	// from now; zero means it never did.
	goalChangedIn time.Duration
}

// fakeBridge stands in for llm-bridge-server, work-graph-store and repo-store
// as the session filer calls them. verdicts maps a chat's name to the model's
// answer for it.
type fakeBridge struct {
	lock       sync.Mutex
	chats      []fakeChat
	verdicts   map[string]string
	modelCalls []string
	prompts    map[string]string
	now        time.Time
	// projectIDsTheModelMayPick is the schema's enum the fake insists on;
	// empty means the two live projects every test starts with.
	projectIDsTheModelMayPick string
}

func (bridge *fakeBridge) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bridge.lock.Lock()
		defer bridge.lock.Unlock()
		path := request.URL.Path
		if path == "/repos/15" {
			json.NewEncoder(writer).Encode(map[string]any{"id": 15, "name": "dash"})
			return
		}
		if path == "/session-repos" {
			json.NewEncoder(writer).Encode(map[string]any{"session_repos": []map[string]any{
				{"session_id": "br_sure", "repo_store_id": 15, "ref_updates": 3, "last_at_unix_nano": 1},
			}})
			return
		}
		switch {
		case path == "/sessions/summary":
			query := request.URL.Query()
			if query.Get("type") != "interactive" || query.Get("purpose") != "chat" {
				http.Error(writer, "the filer must ask for chats only: "+request.URL.RawQuery, http.StatusBadRequest)
				return
			}
			rows := []map[string]any{}
			for _, chat := range bridge.chats {
				rows = append(rows, map[string]any{"sessionId": chat.id, "displayName": chat.name,
					"updatedAt": bridge.now.Add(-chat.updatedAgo).Format(time.RFC3339), "createdAt": bridge.now.Add(-chat.updatedAgo - time.Hour).Format(time.RFC3339)})
			}
			json.NewEncoder(writer).Encode(map[string]any{"sessions": rows, "next": ""})
		case path == "/session-briefs":
			views := []map[string]any{}
			for _, chat := range bridge.chats {
				if chat.goal != "" {
					views = append(views, bridge.briefView(chat))
				}
			}
			json.NewEncoder(writer).Encode(views)
		case strings.HasPrefix(path, "/sessions/") && strings.HasSuffix(path, "/brief"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/sessions/"), "/brief")
			for _, chat := range bridge.chats {
				if chat.id == id {
					if chat.goal == "" {
						writer.Write([]byte(`{"brief":null,"revisions":[],"loose_ends":[]}`))
					} else {
						json.NewEncoder(writer).Encode(bridge.briefView(chat))
					}
					return
				}
			}
			http.NotFound(writer, request)
		case strings.HasPrefix(path, "/sessions/"):
			id := strings.TrimPrefix(path, "/sessions/")
			for _, chat := range bridge.chats {
				if chat.id == id {
					origin := chat.origin
					if origin == "" {
						origin = "frontend"
					}
					json.NewEncoder(writer).Encode(map[string]any{"session_id": chat.id, "display_name": chat.name, "type": "interactive", "purpose": "chat",
						"origin": origin, "manager_session_id": chat.manager, "working_dir": "/work/" + chat.id,
						"updated_at": bridge.now.Add(-chat.updatedAgo).Format(time.RFC3339Nano), "created_at": bridge.now.Add(-chat.updatedAgo - time.Hour).Format(time.RFC3339Nano)})
					return
				}
			}
			http.NotFound(writer, request)
		case path == "/oneshot":
			var oneShot struct {
				Prompt    string `json:"prompt"`
				ModelRole string `json:"model_role"`
				Caller    string `json:"caller"`
				Schema    struct {
					Properties struct {
						ProjectID struct {
							Enum []string `json:"enum"`
						} `json:"project_id"`
					} `json:"properties"`
				} `json:"schema"`
			}
			body, _ := io.ReadAll(request.Body)
			if err := json.Unmarshal(body, &oneShot); err != nil || oneShot.ModelRole != "balanced" || oneShot.Caller != SessionFilerCaller {
				http.Error(writer, "unexpected one-shot request: "+string(body), http.StatusBadRequest)
				return
			}
			wantEnum := bridge.projectIDsTheModelMayPick
			if wantEnum == "" {
				wantEnum = ",project_000001,project_000002"
			}
			if got := strings.Join(oneShot.Schema.Properties.ProjectID.Enum, ","); got != wantEnum {
				http.Error(writer, "the schema must allow exactly the live projects and none: "+got, http.StatusBadRequest)
				return
			}
			for name, verdict := range bridge.verdicts {
				if strings.Contains(oneShot.Prompt, "Name: "+name+"\n") {
					bridge.modelCalls = append(bridge.modelCalls, name)
					bridge.prompts[name] = oneShot.Prompt
					fmt.Fprintf(writer, `{"model":"model-test","parsed":%s}`, verdict)
					return
				}
			}
			http.Error(writer, "no verdict for this prompt: "+oneShot.Prompt, http.StatusBadRequest)
		default:
			t.Errorf("unexpected call: %s %s", request.Method, request.URL)
			http.NotFound(writer, request)
		}
	})
}

func (bridge *fakeBridge) briefView(chat fakeChat) map[string]any {
	revisions := []map[string]any{{"id": "r1", "goal_changed": false, "created_at": bridge.now.Add(-chat.updatedAgo).Format(time.RFC3339)}}
	if chat.goalChangedIn != 0 {
		revisions = append(revisions, map[string]any{"id": "r2", "goal_changed": true, "created_at": bridge.now.Add(chat.goalChangedIn).Format(time.RFC3339)})
	}
	return map[string]any{"brief": map[string]any{"session_id": chat.id, "overall_goal": chat.goal, "current_step": "Working on it."},
		"revisions": revisions, "loose_ends": []any{}}
}

func (bridge *fakeBridge) setChat(id string, change func(*fakeChat)) {
	bridge.lock.Lock()
	defer bridge.lock.Unlock()
	for index := range bridge.chats {
		if bridge.chats[index].id == id {
			change(&bridge.chats[index])
		}
	}
}

// newSessionFilerClient starts a store with two live projects and an archived
// one, made four hours ago by the store's clock, which the returned pointer sets.
func newSessionFilerClient(t *testing.T, bridge *fakeBridge) (client, *time.Time) {
	t.Helper()
	bridge.now = time.Now()
	bridge.prompts = map[string]string{}
	server := httptest.NewServer(bridge.handler(t))
	t.Cleanup(server.Close)
	store := newTestStore(t)
	storeTime := bridge.now.Add(-4 * time.Hour)
	store.now = func() time.Time { return storeTime }
	fake := httpOwners{client: server.Client(), url: server.URL}
	owners := &Owners{HTTP: server.Client(), Repositories: fake, Bridge: fake}
	filer := &SessionFiler{Store: store, Owners: owners, ModelRole: "balanced", MinimumConfidence: 0.8, QuietBeforeFilingWithoutABrief: time.Hour, Window: 48 * time.Hour, SessionsPerPass: 20}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Handlers{Store: store, Owners: owners, SessionFiler: filer})
	c := client{t, mux}
	c.do("POST", "/projects", `{"name":"Chat UI","goal":"The chat sidebar and composer."}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Personal","kind":"stream","goal":"Errands: not software."}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Old thing"}`, http.StatusCreated)
	c.do("PATCH", "/projects/project_000003", `{"archived":true}`, http.StatusOK)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusCreated)
	return c, &storeTime
}

func filingsBySession(t *testing.T, c client, query string) map[string]map[string]any {
	t.Helper()
	bySession := map[string]map[string]any{}
	for _, raw := range c.do("GET", "/session-filings"+query, "", http.StatusOK)["session_filings"].([]any) {
		filing := raw.(map[string]any)
		if _, newer := bySession[filing["session_id"].(string)]; !newer {
			bySession[filing["session_id"].(string)] = filing
		}
	}
	return bySession
}

func TestAPassFilesWhatItIsSureOfProposesTheRestAndLeavesOthersAlone(t *testing.T) {
	bridge := &fakeBridge{
		chats: []fakeChat{
			{id: "br_sure", name: "sidebar counts", goal: "Make the chat sidebar count every filed session.", updatedAgo: time.Minute},
			{id: "br_none", name: "dinner", goal: "Pick a restaurant for Friday.", updatedAgo: 2 * time.Minute},
			{id: "br_unsure", name: "phone plan", goal: "Compare phone plans.", updatedAgo: 3 * time.Minute},
			{id: "br_by_hand", name: "filed by hand", goal: "Anything.", updatedAgo: 4 * time.Minute},
			{id: "br_first_turn", name: "still in its first turn", updatedAgo: 5 * time.Minute},
			{id: "br_old_no_brief", name: "composer fix", updatedAgo: 3 * time.Hour},
			{id: "br_empty", name: "  ", updatedAgo: 4 * time.Hour},
			{id: "br_discovered", name: "Reply with exactly: ok", origin: "discovery", updatedAgo: 5 * time.Hour},
			{id: "br_worker", name: "a worker", manager: "br_sure", goal: "Do a part of it.", updatedAgo: 6 * time.Hour},
			{id: "br_before_the_window", name: "long ago", goal: "Old work.", updatedAgo: 72 * time.Hour},
			{id: "br_test_message", name: "pong", goal: "Get the assistant to reply with the single word PONG.", updatedAgo: 7 * time.Hour},
		},
		verdicts: map[string]string{
			"pong":           `{"project_id":"project_000001","nothing_to_file":true,"confidence":0.9,"why":"A test message with no aim."}`,
			"sidebar counts": `{"project_id":"project_000001","confidence":0.95,"why":"The goal names the chat sidebar and it changed dash."}`,
			"dinner":         `{"project_id":"","confidence":0.4,"why":"No project covers choosing a restaurant."}`,
			"phone plan":     `{"project_id":"project_000002","confidence":0.6,"why":"An errand, but the stream may not take shopping."}`,
			"composer fix":   `{"project_id":"project_000001","confidence":0.3,"why":"Only the name hints at the composer."}`,
		},
	}
	c, storeTime := newSessionFilerClient(t, bridge)
	// The first passes happen two hours ago, so a goal can change after them.
	*storeTime = bridge.now.Add(-2 * time.Hour)
	c.do("POST", "/projects/project_000002/links", `{"entity_type":"session","entity_ref":"br_by_hand","created_by":"bridge-ui session header"}`, http.StatusCreated)

	report := c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	if report["chat_sessions"] != 10.0 || report["filed_by_others"] != 1.0 || report["waiting_for_a_brief"] != 1.0 || report["remaining"] != 0.0 {
		t.Errorf("report = %v", report)
	}
	skipped := report["skipped"].(map[string]any)
	if len(skipped) != 2 || skipped["ran outside the bridge: a one-shot call or a test run found on disk"] != 1.0 || skipped["started by another session"] != 1.0 {
		t.Errorf("skipped = %v", skipped)
	}
	if judged := report["judged"].([]any); len(judged) != 6 {
		t.Fatalf("judged %d sessions, want 6: %v", len(judged), judged)
	}
	if len(bridge.modelCalls) != 5 {
		t.Errorf("model calls = %v, want one for each of the five chats with something to read", bridge.modelCalls)
	}

	filings := filingsBySession(t, c, "")
	sure := filings["br_sure"]
	if sure["status"] != "filed" || sure["project_id"] != "project_000001" || sure["filed_project_id"] != "project_000001" || sure["evidence"] != "brief" ||
		sure["created_by"] != SessionFilerName || sure["model"] != "model-test" || sure["goal"] != "Make the chat sidebar count every filed session." ||
		fmt.Sprint(sure["repo_store_ids"]) != "[15]" {
		t.Errorf("the sure filing = %v", sure)
	}
	links := c.do("GET", "/links?entity_type=session&entity_ref=br_sure", "", http.StatusOK)["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links of the sure session = %v", links)
	}
	link := links[0].(map[string]any)
	if link["project_id"] != "project_000001" || link["created_by"] != SessionFilerName || link["label"] != "sidebar counts" || link["id"] != sure["link_id"] ||
		link["note"] != "The goal names the chat sidebar and it changed dash. (95% sure, "+sure["id"].(string)+")" {
		t.Errorf("the filer's link = %v", link)
	}
	if prompt := bridge.prompts["sidebar counts"]; !strings.Contains(prompt, `- dash: 3 ref moves; owned by project_000001 "Chat UI"`) ||
		!strings.Contains(prompt, "Goal: Make the chat sidebar count every filed session.") || !strings.Contains(prompt, "Folder it ran in: /work/br_sure") ||
		!strings.Contains(prompt, `- project_000002 "Personal" (stream, stage idea)`) || strings.Contains(prompt, "Old thing") {
		t.Errorf("the prompt for the sure session = %s", prompt)
	}
	if none := filings["br_none"]; none["status"] != "proposed" || none["project_id"] != "" || none["confidence"] != 0.0 || none["link_id"] != 0.0 {
		t.Errorf("the filing no project fits = %v", none)
	}
	if unsure := filings["br_unsure"]; unsure["status"] != "proposed" || unsure["project_id"] != "project_000002" || unsure["confidence"] != 0.6 {
		t.Errorf("the unsure filing = %v", unsure)
	}
	if named := filings["br_old_no_brief"]; named["status"] != "proposed" || named["evidence"] != "name" || named["goal"] != "" {
		t.Errorf("the filing judged from a name = %v", named)
	}
	if prompt := bridge.prompts["composer fix"]; !strings.Contains(prompt, "Goal: not known.") || !strings.Contains(prompt, "Repos it changed: none recorded.") {
		t.Errorf("the prompt for a chat with no brief = %s", prompt)
	}
	if empty := filings["br_empty"]; empty["status"] != "nothing_to_file" || empty["evidence"] != "none" || empty["model"] != "" {
		t.Errorf("the filing of an empty chat = %v", empty)
	}
	// A chat the model calls aimless is left unfiled whatever project it names beside that.
	if aimless := filings["br_test_message"]; aimless["status"] != "nothing_to_file" || aimless["project_id"] != "" || aimless["confidence"] != 0.0 || aimless["model"] != "model-test" {
		t.Errorf("the filing of a test message = %v", aimless)
	}
	for _, id := range []string{"br_test_message", "br_none", "br_unsure", "br_old_no_brief", "br_empty", "br_first_turn", "br_discovered", "br_worker", "br_before_the_window"} {
		if links := c.do("GET", "/links?entity_type=session&entity_ref="+id, "", http.StatusOK)["links"].([]any); len(links) != 0 {
			t.Errorf("%s was filed: %v", id, links)
		}
	}
	if proposed := c.do("GET", "/session-filings?status=proposed", "", http.StatusOK)["session_filings"].([]any); len(proposed) != 3 {
		t.Errorf("proposals = %v", proposed)
	}
	if forProject := c.do("GET", "/session-filings?project_id=project_000002&limit=5", "", http.StatusOK)["session_filings"].([]any); len(forProject) != 1 {
		t.Errorf("filings naming project_000002 = %v", forProject)
	}

	// A second pass judges nothing again: no goal changed.
	report = c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	already := report["already_judged"].(map[string]any)
	if len(report["judged"].([]any)) != 0 || len(bridge.modelCalls) != 5 || already["filed"] != 1.0 || already["proposed"] != 3.0 || already["nothing_to_file"] != 2.0 {
		t.Errorf("second pass = %v, model calls %v", report, bridge.modelCalls)
	}

	// The goal of one chat changes, and the chat judged by its name gets its
	// first brief: both are judged again, and the old proposals are superseded.
	bridge.setChat("br_none", func(chat *fakeChat) { chat.goalChangedIn = -time.Hour })
	*storeTime = bridge.now
	bridge.setChat("br_old_no_brief", func(chat *fakeChat) { chat.goal = "Fix the composer in the chat." })
	bridge.lock.Lock()
	bridge.verdicts["dinner"] = `{"project_id":"project_000002","confidence":0.9,"why":"The goal is now an errand the stream takes."}`
	bridge.verdicts["composer fix"] = `{"project_id":"project_000001","confidence":0.85,"why":"The goal names the composer."}`
	bridge.lock.Unlock()
	report = c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	if len(report["judged"].([]any)) != 2 {
		t.Fatalf("third pass judged %v", report["judged"])
	}
	filings = filingsBySession(t, c, "")
	if filings["br_none"]["status"] != "filed" || filings["br_none"]["filed_project_id"] != "project_000002" ||
		filings["br_old_no_brief"]["status"] != "filed" || filings["br_old_no_brief"]["evidence"] != "brief" {
		t.Errorf("after the goals changed: %v and %v", filings["br_none"], filings["br_old_no_brief"])
	}
	if superseded := c.do("GET", "/session-filings?status=superseded", "", http.StatusOK)["session_filings"].([]any); len(superseded) != 2 {
		t.Errorf("superseded = %v", superseded)
	}

	// A person accepts the one proposal left, and cannot decide it twice.
	unsure := filings["br_unsure"]["id"].(string)
	accepted := c.do("POST", "/session-filings/"+unsure+"/accept", `{"decided_by":"principal_000001"}`, http.StatusOK)
	if filing := accepted["session_filing"].(map[string]any); filing["status"] != "accepted" || filing["filed_project_id"] != "project_000002" || filing["decided_by"] != "principal_000001" {
		t.Errorf("accepted = %v", accepted)
	}
	if link := accepted["link"].(map[string]any); link["project_id"] != "project_000002" || link["created_by"] != "principal_000001" || link["note"] != "accepted "+unsure {
		t.Errorf("the link an accept made = %v", accepted["link"])
	}
	c.do("POST", "/session-filings/"+unsure+"/accept", `{}`, http.StatusConflict)
	c.do("POST", "/session-filings/"+unsure+"/dismiss", `{}`, http.StatusConflict)
	c.do("POST", "/session-filings/session_filing_000404/dismiss", `{}`, http.StatusNotFound)
	c.do("GET", "/session-filings?status=someday", "", http.StatusBadRequest)
	c.do("GET", "/session-filings?colour=red", "", http.StatusBadRequest)

	// The session a person filed is theirs now: the filer leaves it alone
	// even when its goal changes.
	bridge.setChat("br_unsure", func(chat *fakeChat) { chat.goalChangedIn = time.Hour })
	report = c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	if len(report["judged"].([]any)) != 0 || report["filed_by_others"] != 2.0 {
		t.Errorf("pass after a person's filing = %v", report)
	}
}

func TestAProposalIsDecidedByAPersonOrClosedByADirectFiling(t *testing.T) {
	bridge := &fakeBridge{
		chats: []fakeChat{
			{id: "br_a", name: "chat a", goal: "Something unclear.", updatedAgo: time.Minute},
			{id: "br_b", name: "chat b", goal: "Something else unclear.", updatedAgo: time.Minute},
			{id: "br_c", name: "chat c", goal: "A third unclear thing.", updatedAgo: time.Minute},
		},
		verdicts: map[string]string{
			"chat a": `{"project_id":"","confidence":0,"why":"Too little to tell."}`,
			"chat b": `{"project_id":"project_000001","confidence":0.5,"why":"A weak fit."}`,
			"chat c": `{"project_id":"project_000001","confidence":0.5,"why":"A weak fit."}`,
		},
	}
	c, _ := newSessionFilerClient(t, bridge)
	c.do("POST", "/session-filings/run", `{"since_days":7,"limit":10}`, http.StatusOK)
	filings := filingsBySession(t, c, "?status=proposed")
	if len(filings) != 3 {
		t.Fatalf("proposals = %v", filings)
	}

	// A proposal with no project needs the person to name one.
	a := filings["br_a"]["id"].(string)
	c.do("POST", "/session-filings/"+a+"/accept", `{"decided_by":"principal_000001"}`, http.StatusBadRequest)
	c.do("POST", "/session-filings/"+a+"/accept", `{"decided_by":"principal_000001","project_id":"project_000404"}`, http.StatusBadRequest)
	accepted := c.do("POST", "/session-filings/"+a+"/accept", `{"decided_by":"principal_000001","project_id":"project_000002"}`, http.StatusOK)
	if accepted["session_filing"].(map[string]any)["filed_project_id"] != "project_000002" {
		t.Errorf("accepted = %v", accepted)
	}

	// Dismissing leaves the session unfiled, and the filer does not ask again.
	b := filings["br_b"]["id"].(string)
	if dismissed := c.do("POST", "/session-filings/"+b+"/dismiss", `{"decided_by":"principal_000001"}`, http.StatusOK); dismissed["status"] != "dismissed" || dismissed["decided_by"] != "principal_000001" {
		t.Errorf("dismissed = %v", dismissed)
	}

	// Filing the session by hand closes the proposal for it.
	c.do("POST", "/projects/project_000002/links", `{"entity_type":"session","entity_ref":"br_c","created_by":"bridge-ui session header"}`, http.StatusCreated)
	if filing := filingsBySession(t, c, "?session_id=br_c")["br_c"]; filing["status"] != "superseded" || filing["decided_by"] != "bridge-ui session header" {
		t.Errorf("the proposal for a session filed by hand = %v", filing)
	}

	calls := len(bridge.modelCalls)
	report := c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	if len(report["judged"].([]any)) != 0 || len(bridge.modelCalls) != calls || report["already_judged"].(map[string]any)["dismissed"] != 1.0 || report["filed_by_others"] != 2.0 {
		t.Errorf("pass after the decisions = %v", report)
	}
	if links := c.do("GET", "/links?entity_type=session&entity_ref=br_b", "", http.StatusOK)["links"].([]any); len(links) != 0 {
		t.Errorf("a dismissed session was filed: %v", links)
	}
}

func TestAWaitingProposalIsJudgedAgainOnceAProjectIsMadeThatMayFitIt(t *testing.T) {
	bridge := &fakeBridge{
		chats: []fakeChat{
			{id: "br_show", name: "game show site", goal: "Build a site for the game show.", updatedAgo: 100 * time.Hour},
			{id: "br_left", name: "left unfiled", goal: "Something a person said to leave.", updatedAgo: 100 * time.Hour},
		},
		verdicts: map[string]string{
			"game show site": `{"project_id":"","confidence":0,"why":"No project covers a game show site."}`,
			"left unfiled":   `{"project_id":"","confidence":0,"why":"No project covers it."}`,
		},
	}
	c, storeTime := newSessionFilerClient(t, bridge)
	*storeTime = bridge.now.Add(-3 * time.Hour)
	c.do("POST", "/session-filings/run", `{"since_days":7}`, http.StatusOK)
	proposals := filingsBySession(t, c, "?status=proposed")
	if len(proposals) != 2 {
		t.Fatalf("proposals = %v", proposals)
	}
	c.do("POST", "/session-filings/"+proposals["br_left"]["id"].(string)+"/dismiss", `{"decided_by":"principal_000001"}`, http.StatusOK)

	// With no new project, a later pass leaves the proposal waiting; both
	// chats are older than the pass's window.
	*storeTime = bridge.now.Add(-2 * time.Hour)
	calls := len(bridge.modelCalls)
	if report := c.do("POST", "/session-filings/run", `{}`, http.StatusOK); len(report["judged"].([]any)) != 0 || len(bridge.modelCalls) != calls {
		t.Errorf("pass with no new project = %v", report)
	}

	// The person makes the project that was missing. The next pass judges
	// the waiting proposal again, though its chat is older than the window,
	// and leaves the dismissed one alone.
	c.do("POST", "/projects", `{"name":"Game show","goal":"A site that runs a game show night."}`, http.StatusCreated)
	bridge.lock.Lock()
	bridge.projectIDsTheModelMayPick = ",project_000001,project_000004,project_000002"
	bridge.verdicts["game show site"] = `{"project_id":"project_000004","confidence":0.95,"why":"The goal names the game show site."}`
	bridge.lock.Unlock()
	*storeTime = bridge.now.Add(-time.Hour)
	report := c.do("POST", "/session-filings/run", `{}`, http.StatusOK)
	if judged := report["judged"].([]any); len(judged) != 1 || judged[0].(map[string]any)["status"] != "filed" || judged[0].(map[string]any)["filed_project_id"] != "project_000004" {
		t.Fatalf("pass after the project was made = %v", report)
	}
	if old := c.do("GET", "/session-filings?status=superseded&session_id=br_show", "", http.StatusOK)["session_filings"].([]any); len(old) != 1 {
		t.Errorf("the proposal that was judged again = %v", old)
	}
	if links := c.do("GET", "/links?entity_type=session&entity_ref=br_left", "", http.StatusOK)["links"].([]any); len(links) != 0 {
		t.Errorf("a dismissed session was filed: %v", links)
	}

	// Nothing brings either back a second time.
	*storeTime = bridge.now
	calls = len(bridge.modelCalls)
	if report := c.do("POST", "/session-filings/run", `{}`, http.StatusOK); len(report["judged"].([]any)) != 0 || len(bridge.modelCalls) != calls {
		t.Errorf("pass after the proposal was settled = %v", report)
	}
}

func TestThePassNeverFilesUnderAProjectTheModelMadeUpAndStopsAtItsLimit(t *testing.T) {
	bridge := &fakeBridge{
		chats: []fakeChat{
			{id: "br_invented", name: "invented", goal: "Build a new thing.", updatedAgo: time.Minute},
			{id: "br_archived", name: "archived", goal: "Work on the old thing.", updatedAgo: 2 * time.Minute},
			{id: "br_odds", name: "odds", goal: "Anything.", updatedAgo: 3 * time.Minute},
			{id: "br_later", name: "later", goal: "Fix the sidebar.", updatedAgo: 4 * time.Minute},
		},
		verdicts: map[string]string{
			"invented": `{"project_id":"project_000099","confidence":0.99,"why":"It deserves a project of its own."}`,
			"archived": `{"project_id":"project_000003","confidence":0.99,"why":"The archived project."}`,
			"odds":     `{"project_id":"project_000001","confidence":7,"why":"Very sure."}`,
			"later":    `{"project_id":"project_000001","confidence":0.9,"why":"The goal names the sidebar."}`,
		},
	}
	c, _ := newSessionFilerClient(t, bridge)
	report := c.do("POST", "/session-filings/run", `{"limit":3}`, http.StatusBadGateway)
	if len(report["failed"].([]any)) != 3 || len(report["judged"].([]any)) != 0 || report["remaining"] != 1.0 {
		t.Errorf("report = %v", report)
	}
	if filings := c.do("GET", "/session-filings", "", http.StatusOK)["session_filings"].([]any); len(filings) != 0 {
		t.Errorf("a refused verdict was recorded: %v", filings)
	}
	if links := c.do("GET", "/links?entity_type=session", "", http.StatusOK)["links"].([]any); len(links) != 0 {
		t.Errorf("a refused verdict was filed: %v", links)
	}
	if projects := c.do("GET", "/projects?include_archived=true", "", http.StatusOK)["projects"].([]any); len(projects) != 3 {
		t.Errorf("the filer made a project: %v", projects)
	}

	c.do("POST", "/session-filings/run", `{"limit":0}`, http.StatusBadRequest)
	c.do("POST", "/session-filings/run", `{"since_days":9999}`, http.StatusBadRequest)
	c.do("POST", "/session-filings/run", `{"colour":"red"}`, http.StatusBadRequest)
}

func TestAPassIsRefusedWithoutAModelRoleAndWhileAnotherRuns(t *testing.T) {
	store := newTestStore(t)
	filer := &SessionFiler{Store: store, Owners: &Owners{}, MinimumConfidence: 0.8, Window: time.Hour, SessionsPerPass: 5}
	if _, err := filer.Run(t.Context(), SessionFilerRunRequest{}); err == nil || !strings.Contains(err.Error(), "project_store.session_filer.model_role") {
		t.Errorf("a pass with no model role: %v", err)
	}
	filer.ModelRole = "balanced"
	filer.running.Lock()
	if _, err := filer.Run(t.Context(), SessionFilerRunRequest{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second pass while one runs: %v", err)
	}
	filer.running.Unlock()

	for _, bad := range []*SessionFiler{
		{MinimumConfidence: 0, Window: time.Hour, SessionsPerPass: 5},
		{MinimumConfidence: 1.5, Window: time.Hour, SessionsPerPass: 5},
		{MinimumConfidence: 0.8, Window: 0, SessionsPerPass: 5},
		{MinimumConfidence: 0.8, Window: time.Hour, SessionsPerPass: 0},
		{MinimumConfidence: 0.8, Window: time.Hour, SessionsPerPass: 5, QuietBeforeFilingWithoutABrief: -time.Minute},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("settings %+v were accepted", bad)
		}
	}
	if err := filer.Validate(); err != nil {
		t.Errorf("good settings were refused: %v", err)
	}
}

func TestTheFilerFilesOnlyChatsAPersonHasThroughTheBridge(t *testing.T) {
	chat := BridgeSession{Type: "interactive", Purpose: "chat", Origin: "frontend"}
	if reason := sessionFilerSkipReason(chat); reason != "" {
		t.Errorf("a chat is skipped: %s", reason)
	}
	for name, change := range map[string]func(*BridgeSession){
		"an autoworker":        func(session *BridgeSession) { session.Type, session.Purpose = "autonomous", "autoworker" },
		"a subagent":           func(session *BridgeSession) { session.Type, session.Purpose = "system", "subagent" },
		"a voice command":      func(session *BridgeSession) { session.Purpose = "voice" },
		"a discovered session": func(session *BridgeSession) { session.Origin = "discovery" },
		"a managed session":    func(session *BridgeSession) { session.ManagerSessionID = "br_1" },
	} {
		session := chat
		change(&session)
		if sessionFilerSkipReason(session) == "" {
			t.Errorf("%s is filed", name)
		}
	}
}
