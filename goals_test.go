package projectstore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func goalIDs(goals []any) string {
	var ids []string
	for _, raw := range goals {
		ids = append(ids, raw.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, " ")
}

func TestAGoalIsMadeUnderALiveProjectWithAStatement(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging"}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Old work"}`, http.StatusCreated)
	c.do("PATCH", "/projects/project_000002", `{"archived":true}`, http.StatusOK)

	goal := c.do("POST", "/projects/project_000001/goals", `{"statement":"  Read Signal in the inbox  ","done_when":"A Signal message shows in the list","priority":2,"target_at":1800000000,"created_by":"test"}`, http.StatusCreated)
	if goal["id"] != "goal_000001" || goal["project_id"] != "project_000001" || goal["state"] != "open" || goal["statement"] != "Read Signal in the inbox" ||
		goal["priority"] != 2.0 || goal["target_at"] != 1800000000.0 || goal["parent_goal_id"] != "" || goal["created_by"] != "test" || goal["closed_at"] != 0.0 {
		t.Fatalf("goal = %v", goal)
	}
	proposed := c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Slack too","state":"proposed"}`, http.StatusCreated)
	if proposed["id"] != "goal_000002" || proposed["state"] != "proposed" {
		t.Fatalf("proposed goal = %v", proposed)
	}

	for body, why := range map[string]string{
		`{"done_when":"x"}`:                           "no statement",
		`{"statement":"   "}`:                         "a blank statement",
		`{"statement":"x","state":"reached"}`:         "a new goal already reached",
		`{"statement":"x","state":"someday"}`:         "an unknown state",
		`{"statement":"x","colour":"red"}`:            "an unknown field",
		`{"statement":"x","closed_because":"y"}`:      "closed_because on create",
		`{"statement":"x","priority":"high"}`:         "a priority that is no number",
		`{"statement":"x","priority":1.5}`:            "a priority that is not whole",
		`{"statement":"x","target_at":-1}`:            "a negative target_at",
		`{"statement":"x","parent_goal_id":"goal_9"}`: "a parent that is no goal",
	} {
		recorder := httptest.NewRecorder()
		c.mux.ServeHTTP(recorder, httptest.NewRequest("POST", "/projects/project_000001/goals", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("a goal with %s = %d, want 400: %s", why, recorder.Code, recorder.Body)
		}
	}
	c.do("POST", "/projects/project_000404/goals", `{"statement":"x"}`, http.StatusNotFound)
	c.do("POST", "/projects/project_000002/goals", `{"statement":"x"}`, http.StatusBadRequest)

	if got := c.do("GET", "/goals/goal_000001", "", http.StatusOK); got["statement"] != "Read Signal in the inbox" {
		t.Errorf("GET goal = %v", got)
	}
	c.do("GET", "/goals/goal_000404", "", http.StatusNotFound)

	vocabulary := c.do("GET", "/vocabulary", "", http.StatusOK)
	if states := vocabulary["goal_states"].([]any); len(states) != 5 || vocabulary["default_goal_state"] != "open" {
		t.Errorf("vocabulary goal_states = %v, default %v", states, vocabulary["default_goal_state"])
	}
	if types := vocabulary["goal_link_entity_types"].([]any); len(types) != 2 {
		t.Errorf("vocabulary goal_link_entity_types = %v", types)
	}
}

func TestAGoalMovesOnlyAlongItsStatesAndSaysWhyItClosed(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Signal","state":"proposed"}`, http.StatusCreated)
	goal := "/goals/goal_000001"

	// A proposal is accepted or dismissed, nothing else.
	c.do("PATCH", goal, `{"state":"reached","closed_because":"done","changed_by":"test"}`, http.StatusConflict)
	c.do("PATCH", goal, `{"state":"dropped","closed_because":"no","changed_by":"test"}`, http.StatusConflict)
	c.do("PATCH", goal, `{"state":"someday"}`, http.StatusBadRequest)
	c.do("PATCH", goal, `{"colour":"red"}`, http.StatusBadRequest)
	if accepted := c.do("PATCH", goal, `{"state":"open","changed_by":"slava"}`, http.StatusOK); accepted["state"] != "open" {
		t.Fatalf("accepted = %v", accepted)
	}
	c.do("PATCH", goal, `{"state":"dismissed","closed_because":"no","changed_by":"test"}`, http.StatusConflict)
	c.do("PATCH", goal, `{"state":"proposed"}`, http.StatusConflict)

	// Closing needs a reason and a name.
	c.do("PATCH", goal, `{"state":"reached","changed_by":"slava"}`, http.StatusBadRequest)
	c.do("PATCH", goal, `{"state":"reached","closed_because":"  ","changed_by":"slava"}`, http.StatusBadRequest)
	c.do("PATCH", goal, `{"state":"reached","closed_because":"Signal reads in the list"}`, http.StatusBadRequest)
	// A reason with no closing move is refused, so it is never dropped in silence.
	c.do("PATCH", goal, `{"closed_because":"just a note"}`, http.StatusBadRequest)
	c.do("PATCH", goal, `{"state":"open","closed_because":"just a note"}`, http.StatusBadRequest)

	reached := c.do("PATCH", goal, `{"state":"reached","closed_because":"Signal reads in the list","changed_by":"slava"}`, http.StatusOK)
	if reached["state"] != "reached" || reached["closed_by"] != "slava" || reached["closed_because"] != "Signal reads in the list" || reached["closed_at"] == 0.0 {
		t.Fatalf("reached = %v", reached)
	}
	c.do("PATCH", goal, `{"state":"dropped","closed_because":"changed my mind","changed_by":"slava"}`, http.StatusConflict)
	reopened := c.do("PATCH", goal, `{"state":"open","changed_by":"slava"}`, http.StatusOK)
	if reopened["state"] != "open" || reopened["closed_by"] != "" || reopened["closed_because"] != "" || reopened["closed_at"] != 0.0 {
		t.Fatalf("a reopened goal keeps closed fields: %v", reopened)
	}
	c.do("PATCH", goal, `{"state":"dropped","closed_because":"Signal is gone","changed_by":"slava"}`, http.StatusOK)
	c.do("PATCH", goal, `{"state":"reached","closed_because":"x","changed_by":"slava"}`, http.StatusConflict)
	c.do("PATCH", goal, `{"state":"open","changed_by":"slava"}`, http.StatusOK)

	// The other fields change in any state, and every change is recorded.
	c.do("PATCH", goal, `{"statement":"  ","changed_by":"slava"}`, http.StatusBadRequest)
	edited := c.do("PATCH", goal, `{"statement":"Read Signal and Slack","done_when":"Both show","priority":3,"target_at":1800000000,"changed_by":"slava"}`, http.StatusOK)
	if edited["statement"] != "Read Signal and Slack" || edited["priority"] != 3.0 || edited["target_at"] != 1800000000.0 {
		t.Fatalf("edited = %v", edited)
	}
	var summary []string
	for _, raw := range c.do("GET", goal+"/changes", "", http.StatusOK)["changes"].([]any) {
		change := raw.(map[string]any)
		summary = append(summary, change["field"].(string)+":"+change["old_value"].(string)+">"+change["new_value"].(string))
	}
	want := "state:proposed>open state:open>reached closed_because:>Signal reads in the list state:reached>open closed_because:Signal reads in the list> " +
		"state:open>dropped closed_because:>Signal is gone state:dropped>open closed_because:Signal is gone> " +
		"statement:Read Signal>Read Signal and Slack done_when:>Both show priority:0>3 target_at:0>1800000000"
	if got := strings.Join(summary, " "); got != want {
		t.Errorf("changes =\n%s\nwant\n%s", got, want)
	}
	c.do("GET", "/goals/goal_000404/changes", "", http.StatusNotFound)
	c.do("PATCH", "/goals/goal_000404", `{"priority":1}`, http.StatusNotFound)

	// A dismissed proposal stays dismissed.
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read fax","state":"proposed"}`, http.StatusCreated)
	c.do("PATCH", "/goals/goal_000002", `{"state":"dismissed","closed_because":"nobody faxes","changed_by":"slava"}`, http.StatusOK)
	c.do("PATCH", "/goals/goal_000002", `{"state":"open","changed_by":"slava"}`, http.StatusConflict)
}

func TestGoalsNestWithinOneProjectAndListParentsFirst(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging"}`, http.StatusCreated)
	c.do("POST", "/projects", `{"name":"Chat UI"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"One inbox","priority":1}`, http.StatusCreated)                     // goal_000001
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Signal","parent_goal_id":"goal_000001"}`, http.StatusCreated) // goal_000002
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Send from the phone","priority":5}`, http.StatusCreated)           // goal_000003
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Slack","parent_goal_id":"goal_000001","priority":4}`, http.StatusCreated)
	c.do("POST", "/projects/project_000002/goals", `{"statement":"A phone layout"}`, http.StatusCreated) // goal_000005
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read fax","state":"proposed"}`, http.StatusCreated)
	c.do("PATCH", "/goals/goal_000006", `{"state":"dismissed","closed_because":"nobody faxes","changed_by":"test"}`, http.StatusOK)

	// A parent is a goal of the same project and never the goal or one below it.
	c.do("POST", "/projects/project_000002/goals", `{"statement":"x","parent_goal_id":"goal_000001"}`, http.StatusBadRequest)
	c.do("PATCH", "/goals/goal_000005", `{"parent_goal_id":"goal_000001"}`, http.StatusBadRequest)
	c.do("PATCH", "/goals/goal_000001", `{"parent_goal_id":"goal_000001"}`, http.StatusBadRequest)
	c.do("PATCH", "/goals/goal_000001", `{"parent_goal_id":"goal_000002"}`, http.StatusBadRequest)
	c.do("PATCH", "/goals/goal_000002", `{"parent_goal_id":"goal_000404"}`, http.StatusBadRequest)

	// The more urgent sibling first, each parent directly before its children,
	// and no dismissed proposal unless asked for.
	if got := goalIDs(c.do("GET", "/projects/project_000001/goals", "", http.StatusOK)["goals"].([]any)); got != "goal_000003 goal_000001 goal_000004 goal_000002" {
		t.Errorf("project goals in order = %s", got)
	}
	if got := goalIDs(c.do("GET", "/goals", "", http.StatusOK)["goals"].([]any)); got != "goal_000003 goal_000001 goal_000004 goal_000002 goal_000005" {
		t.Errorf("every project's goals = %s", got)
	}
	if got := goalIDs(c.do("GET", "/goals?project_id=project_000002", "", http.StatusOK)["goals"].([]any)); got != "goal_000005" {
		t.Errorf("goals of project_000002 = %s", got)
	}
	if got := goalIDs(c.do("GET", "/projects/project_000001/goals?state=dismissed", "", http.StatusOK)["goals"].([]any)); got != "goal_000006" {
		t.Errorf("dismissed goals = %s", got)
	}

	// A goal moved to the top, and a child whose parent a filter leaves out.
	c.do("PATCH", "/goals/goal_000002", `{"parent_goal_id":"","changed_by":"test"}`, http.StatusOK)
	c.do("PATCH", "/goals/goal_000001", `{"state":"reached","closed_because":"the inbox is one","changed_by":"test"}`, http.StatusOK)
	if got := goalIDs(c.do("GET", "/goals?state=open&project_id=project_000001", "", http.StatusOK)["goals"].([]any)); got != "goal_000003 goal_000004 goal_000002" {
		t.Errorf("open goals with the parent reached = %s", got)
	}

	c.do("GET", "/projects/project_000001/goals?state=someday", "", http.StatusBadRequest)
	c.do("GET", "/projects/project_000001/goals?colour=red", "", http.StatusBadRequest)
	c.do("GET", "/projects/project_000404/goals", "", http.StatusNotFound)
	c.do("GET", "/goals?state=someday", "", http.StatusBadRequest)
	c.do("GET", "/goals?colour=red", "", http.StatusBadRequest)
	c.do("GET", "/goals?project_id=project_000404", "", http.StatusNotFound)
}

func TestWhatServesAGoalIsCheckedWithItsOwner(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Signal"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Slack"}`, http.StatusCreated)

	todo := c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"n-open","note":"the first step","created_by":"test"}`, http.StatusCreated)
	if todo["label"] != "Ship the inbox" || todo["goal_id"] != "goal_000001" || todo["note"] != "the first step" || todo["created_by"] != "test" {
		t.Errorf("note link = %v", todo)
	}
	session := c.do("POST", "/goals/goal_000001/links", `{"entity_type":"session","entity_ref":"br_1790000000000000001"}`, http.StatusCreated)
	if session["label"] != "Discord signup UI" {
		t.Errorf("session link label = %v", session["label"])
	}
	// One todo may serve two goals.
	c.do("POST", "/goals/goal_000002/links", `{"entity_type":"note","entity_ref":"n-open"}`, http.StatusCreated)

	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"n-open"}`, http.StatusConflict)
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"n-never"}`, http.StatusBadRequest)
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"session","entity_ref":"br_1"}`, http.StatusBadRequest)
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"n-broken"}`, http.StatusBadGateway)
	// A repo belongs to a project, not to a goal.
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"repo","entity_ref":"15"}`, http.StatusBadRequest)
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note"}`, http.StatusBadRequest)
	c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"n-done","colour":"red"}`, http.StatusBadRequest)
	c.do("POST", "/goals/goal_000404/links", `{"entity_type":"note","entity_ref":"n-open"}`, http.StatusNotFound)

	if links := c.do("GET", "/goals/goal_000001/links", "", http.StatusOK)["links"].([]any); len(links) != 2 {
		t.Errorf("links of goal_000001 = %v", links)
	}
	if links := c.do("GET", "/goals/goal_000001/links?entity_type=session", "", http.StatusOK)["links"].([]any); len(links) != 1 {
		t.Errorf("session links of goal_000001 = %v", links)
	}
	c.do("GET", "/goals/goal_000001/links?colour=red", "", http.StatusBadRequest)
	c.do("GET", "/goals/goal_000404/links", "", http.StatusNotFound)

	if served := c.do("GET", "/goal-links?entity_type=note&entity_ref=n-open", "", http.StatusOK)["links"].([]any); len(served) != 2 {
		t.Errorf("n-open serves %d goals, want 2", len(served))
	}
	if every := c.do("GET", "/goal-links?entity_type=session", "", http.StatusOK)["links"].([]any); len(every) != 1 {
		t.Errorf("every session goal link = %v", every)
	}
	c.do("GET", "/goal-links?entity_ref=n-open", "", http.StatusBadRequest)
	c.do("GET", "/goal-links?entity_type=note&colour=red", "", http.StatusBadRequest)

	c.do("DELETE", "/goals/goal_000002/links/1", "", http.StatusNotFound)
	c.do("DELETE", "/goals/goal_000001/links/one", "", http.StatusBadRequest)
	c.do("DELETE", "/goals/goal_000001/links/1", "", http.StatusNoContent)
	c.do("DELETE", "/goals/goal_000001/links/1", "", http.StatusNotFound)
	if links := c.do("GET", "/goals/goal_000001/links", "", http.StatusOK)["links"].([]any); len(links) != 1 {
		t.Errorf("links after delete = %v", links)
	}
	// A goal link files nothing under the project.
	if links := c.do("GET", "/projects/project_000001/links", "", http.StatusOK)["links"].([]any); len(links) != 0 {
		t.Errorf("project links = %v, want none", links)
	}
}

func TestTheRollupDigestAndActivityShowAProjectsGoals(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/projects", `{"name":"Messaging"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/links", `{"entity_type":"note","entity_ref":"n-design"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"One inbox","done_when":"Every chat reads in one list","target_at":1800000000,"created_by":"test"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Signal","parent_goal_id":"goal_000001"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read Slack","state":"proposed"}`, http.StatusCreated)
	c.do("POST", "/projects/project_000001/goals", `{"statement":"Read fax"}`, http.StatusCreated)
	c.do("PATCH", "/goals/goal_000004", `{"state":"dropped","closed_because":"nobody faxes","changed_by":"slava"}`, http.StatusOK)
	for _, note := range []string{"n-open", "n-done", "n-design"} {
		c.do("POST", "/goals/goal_000001/links", `{"entity_type":"note","entity_ref":"`+note+`"}`, http.StatusCreated)
	}
	c.do("POST", "/goals/goal_000002/links", `{"entity_type":"session","entity_ref":"br_1790000000000000001"}`, http.StatusCreated)
	c.do("POST", "/goals/goal_000002/links", `{"entity_type":"note","entity_ref":"n-done"}`, http.StatusCreated)

	rollup := c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)
	goals := rollup["goals"].([]any)
	if len(goals) != 3 {
		t.Fatalf("rollup goals = %v, want the open and proposed ones", goals)
	}
	first := goals[0].(map[string]any)
	if first["goal"].(map[string]any)["id"] != "goal_000001" || len(first["links"].([]any)) != 3 || first["open_todos"] != 1.0 || first["done_todos"] != 1.0 || len(first["missing_note_ids"].([]any)) != 0 {
		t.Errorf("first goal = %v, want three links, one todo open and one done, the note counted as neither", first)
	}
	second := goals[1].(map[string]any)
	if second["goal"].(map[string]any)["id"] != "goal_000002" || len(second["links"].([]any)) != 2 || second["open_todos"] != 0.0 || second["done_todos"] != 1.0 {
		t.Errorf("second goal = %v", second)
	}
	if third := goals[2].(map[string]any); third["goal"].(map[string]any)["state"] != "proposed" || len(third["links"].([]any)) != 0 {
		t.Errorf("third goal = %v", third)
	}
	// The project's own notes are still read, in the same noteboard call.
	if notes := rollup["notes"].([]any); len(notes) != 1 || notes[0].(map[string]any)["note_id"] != "n-design" {
		t.Errorf("project notes = %v", notes)
	}

	recorder := httptest.NewRecorder()
	c.mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/digest", nil))
	text := recorder.Body.String()
	for _, want := range []string{"  open goals:\n",
		"    goal_000001  One inbox; done when: Every chat reads in one list; target 2027-01-15; todos: 1 open, 1 done\n",
		"      goal_000002  Read Signal; todos: 0 open, 1 done\n",
		"  proposed goals awaiting a decision: 1\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Read fax") || strings.Contains(text, "Read Slack") {
		t.Errorf("digest names a dropped or proposed goal:\n%s", text)
	}

	// A todo noteboard has since lost is named, not skipped.
	noteboardItemIsGone = func(id string) bool { return id == "n-open" }
	t.Cleanup(func() { noteboardItemIsGone = func(string) bool { return false } })
	first = c.do("GET", "/projects/project_000001/rollup", "", http.StatusOK)["goals"].([]any)[0].(map[string]any)
	if missing := first["missing_note_ids"].([]any); len(missing) != 1 || missing[0] != "n-open" || first["open_todos"] != 0.0 {
		t.Errorf("goal with a lost todo = %v", first)
	}

	kinds := map[string]int{}
	var stateChange map[string]any
	for _, raw := range c.do("GET", "/projects/project_000001/activity", "", http.StatusOK)["events"].([]any) {
		event := raw.(map[string]any)
		kinds[event["kind"].(string)]++
		if event["kind"] == ActivityGoalStateChanged {
			stateChange = event
		}
	}
	for kind, count := range map[string]int{ActivityGoalCreated: 4, ActivityGoalStateChanged: 1, ActivityGoalLinkAdded: 5, ActivityLinkAdded: 1} {
		if kinds[kind] != count {
			t.Errorf("%d %s events, want %d (all: %v)", kinds[kind], kind, count, kinds)
		}
	}
	if stateChange == nil || stateChange["goal_id"] != "goal_000004" || stateChange["old_value"] != "open" || stateChange["new_value"] != "dropped" ||
		stateChange["actor"] != "slava" || stateChange["label"] != "Read fax" {
		t.Errorf("state change event = %v", stateChange)
	}
	vocabulary := c.do("GET", "/vocabulary", "", http.StatusOK)["activity_kinds"].([]any)
	listed := map[string]bool{}
	for _, kind := range vocabulary {
		listed[kind.(string)] = true
	}
	for _, kind := range []string{ActivityGoalCreated, ActivityGoalStateChanged, ActivityGoalLinkAdded} {
		if !listed[kind] {
			t.Errorf("vocabulary lacks activity kind %s", kind)
		}
	}
}
