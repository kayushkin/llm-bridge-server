package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/bus/messages"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
	"github.com/kayushkin/llm-bridge/msg"
)

const testBoardID = "board-tcl"

// fakeBridge answers the two instances and the agents llm-bridge-server listed
// on 2026-09-11, plus whatever sessions a test gives it by id.
// A session whose body is sessionUnreadable answers 500, the way a bridge that
// is up but failing does.
func fakeBridge(t *testing.T, sessions map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/instances/inst-cc-local":
			w.Write([]byte(`{"id":"inst-cc-local","harness_type":"claude_code","enabled":true}`))
		case r.URL.Path == "/instances/inst-codex-local":
			w.Write([]byte(`{"id":"inst-codex-local","harness_type":"codex","enabled":true}`))
		case r.URL.Path == "/agents":
			w.Write([]byte(`[{"id":12,"slug":"argraphments"},{"id":65,"slug":"northwind-builder"}]`))
		case strings.HasPrefix(r.URL.Path, "/sessions/"):
			body, ok := sessions[strings.TrimPrefix(r.URL.Path, "/sessions/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if body == sessionUnreadable {
				http.Error(w, `{"error":"injected"}`, http.StatusInternalServerError)
				return
			}
			w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
}

const sessionUnreadable = "<unreadable>"

// fakeKanban is the part of kanban-store a tick reads and writes.
type fakeKanban struct {
	t     *testing.T
	board issuesclient.Board
	view  issuesclient.BoardView
	// effective is each card's effective-defaults answer; a card absent here
	// answers 404, as kanban-store does for an item noteboard lacks.
	effective map[string]issuesclient.EffectiveDefaults
	children  map[string][]string // parent card id → child card ids
	scoped    map[string]bool
	links     map[string][]issuesclient.CardLink
	// status is a child's noteboard status as the parent_card listing reports
	// it; a child absent here is "open".
	status map[string]string
	// placements is what GET /cards/{id}/placements answers per card; a
	// card absent here has none.
	placements map[string][]issuesclient.CardPlacement
	// failures makes one kind of request answer with the given status, keyed
	// by the names fakeKanbanRoute returns.
	failures map[string]int
	// failAfter lets that many requests of a failing route succeed first, so a
	// test can fail the second of two reads of one route.
	failAfter map[string]int
	requests  map[string]int

	mu      sync.Mutex
	linked  []string // card ids given a session link
	moved   []string // card ids moved
	patched []string // card ids whose body was patched
	// sent is the decoded body of each write, keyed "link:<card>",
	// "move:<card>" or "patch:<card>"; a later write to the same key replaces
	// an earlier one.
	sent map[string]map[string]any
}

// record keeps the decoded body of one write under kind:cardID.
func (f *fakeKanban) record(kind, cardID string, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("%s %s: undecodable body: %v", kind, cardID, err)
	}
	if f.sent == nil {
		f.sent = map[string]map[string]any{}
	}
	f.sent[kind+":"+cardID] = body
}

// fakeKanbanRoute names the kind of request a failure can be injected into.
func fakeKanbanRoute(method, path string) string {
	boardPath := strings.TrimPrefix(path, "/boards/")
	switch {
	case method == "GET" && strings.HasPrefix(path, "/boards/") && !strings.Contains(boardPath, "/"):
		return "board"
	case method == "GET" && strings.HasPrefix(path, "/boards/") && strings.HasSuffix(path, "/cards") && strings.Count(boardPath, "/") == 1:
		return "view"
	case method == "GET" && strings.HasPrefix(path, "/entities/note/") && strings.HasSuffix(path, "/tags"):
		return "tags"
	case method == "GET" && strings.HasPrefix(path, "/entities/parent_card/"):
		return "children"
	case method == "GET" && strings.HasSuffix(path, "/links"):
		return "links"
	case method == "POST" && strings.HasSuffix(path, "/links"):
		return "link"
	case method == "POST" && strings.HasSuffix(path, "/move"):
		return "move"
	case method == "GET" && strings.HasSuffix(path, "/placements"):
		return "placements"
	case method == "PATCH" && strings.HasPrefix(path, "/cards/"):
		return "patch"
	}
	return ""
}

func (f *fakeKanban) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	boardPrefix := "/boards/" + f.board.ID
	writeJSON := func(v any) { json.NewEncoder(w).Encode(v) }
	route := fakeKanbanRoute(r.Method, path)
	if f.requests == nil {
		f.requests = map[string]int{}
	}
	f.requests[route]++
	if code := f.failures[route]; code != 0 && f.requests[route] > f.failAfter[route] {
		http.Error(w, `{"error":"injected"}`, code)
		return
	}
	switch {
	case r.Method == "GET" && path == boardPrefix:
		writeJSON(f.board)
	case r.Method == "GET" && path == boardPrefix+"/cards":
		writeJSON(f.view)
	case r.Method == "GET" && strings.HasPrefix(path, boardPrefix+"/cards/") && strings.HasSuffix(path, "/effective-defaults"):
		cardID := strings.TrimSuffix(strings.TrimPrefix(path, boardPrefix+"/cards/"), "/effective-defaults")
		resolved, ok := f.effective[cardID]
		if !ok {
			http.Error(w, `{"error":"noteboard item not found: `+cardID+`"}`, http.StatusNotFound)
			return
		}
		resolved.BoardID, resolved.CardID = f.board.ID, cardID
		writeJSON(resolved)
	case r.Method == "GET" && strings.HasPrefix(path, "/entities/note/") && strings.HasSuffix(path, "/tags"):
		cardID := strings.TrimSuffix(strings.TrimPrefix(path, "/entities/note/"), "/tags")
		tags := []issuesclient.EntityTag{}
		if f.scoped[cardID] {
			tags = append(tags, issuesclient.EntityTag{EntityType: "note", EntityRef: cardID, Tag: tagScoped})
		}
		writeJSON(tags)
	case r.Method == "GET" && strings.HasPrefix(path, "/entities/parent_card/") && strings.HasSuffix(path, "/cards"):
		parentID := strings.TrimSuffix(strings.TrimPrefix(path, "/entities/parent_card/"), "/cards")
		entries := []issuesclient.EntityCardEntry{}
		for _, id := range f.children[parentID] {
			status := f.status[id]
			if status == "" {
				status = "open"
			}
			entries = append(entries, issuesclient.EntityCardEntry{CardID: id, Item: map[string]any{"id": id, "status": status}})
		}
		writeJSON(entries)
	case r.Method == "GET" && strings.HasPrefix(path, "/cards/") && strings.HasSuffix(path, "/links"):
		links := f.links[strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/links")]
		if links == nil {
			links = []issuesclient.CardLink{}
		}
		writeJSON(links)
	case r.Method == "POST" && strings.HasPrefix(path, "/cards/") && strings.HasSuffix(path, "/links"):
		f.linked = append(f.linked, strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/links"))
		f.record("link", strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/links"), r)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	case r.Method == "POST" && strings.HasPrefix(path, "/cards/") && strings.HasSuffix(path, "/move"):
		f.moved = append(f.moved, strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/move"))
		f.record("move", strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/move"), r)
		w.Write([]byte(`{}`))
	case r.Method == "GET" && strings.HasPrefix(path, "/cards/") && strings.HasSuffix(path, "/placements"):
		placements := f.placements[strings.TrimSuffix(strings.TrimPrefix(path, "/cards/"), "/placements")]
		if placements == nil {
			placements = []issuesclient.CardPlacement{}
		}
		writeJSON(placements)
	case r.Method == "PATCH" && strings.HasPrefix(path, "/cards/"):
		f.patched = append(f.patched, strings.TrimPrefix(path, "/cards/"))
		f.record("patch", strings.TrimPrefix(path, "/cards/"), r)
		w.Write([]byte(`{}`))
	default:
		f.t.Errorf("unexpected kanban-store request %s %s", r.Method, path)
		http.NotFound(w, r)
	}
}

func testCard(id string, tags ...string) issuesclient.CardView {
	anyTags := make([]any, 0, len(tags))
	for _, tag := range tags {
		anyTags = append(anyTags, tag)
	}
	card := issuesclient.CardView{Item: map[string]any{"id": id, "title": "card " + id, "tags": anyTags}}
	card.Placement.CardID = id
	return card
}

func column(name string, cards ...issuesclient.CardView) issuesclient.ColumnView {
	return issuesclient.ColumnView{Column: issuesclient.Column{ID: "col-" + strings.ReplaceAll(strings.ToLower(name), " ", "-"), Name: name}, Cards: cards}
}

func fromBoard(value string) issuesclient.EffectiveDefault {
	return issuesclient.EffectiveDefault{Value: value, Source: issuesclient.DefaultSource{Kind: issuesclient.DefaultSourceKindBoard}}
}

func fromRule(value, ruleID string, position int, tags ...string) issuesclient.EffectiveDefault {
	return issuesclient.EffectiveDefault{Value: value, Source: issuesclient.DefaultSource{
		Kind: issuesclient.DefaultSourceKindTagRule, RuleID: ruleID, RuleTags: tags, RulePosition: &position,
	}}
}

// productCardDefaults is what a card tagged cat:product gets from the rule
// [cat:product] → inst-codex-local + bundle 6, on a board whose own agent is 65.
func productCardDefaults() issuesclient.EffectiveDefaults {
	return issuesclient.EffectiveDefaults{Tags: []string{"group:a", "cat:product"}, MatchedRuleIDs: []string{"rule-product"},
		Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{
			issuesclient.DefaultFieldInstance: fromRule("inst-codex-local", "rule-product", 0, "cat:product"),
			issuesclient.DefaultFieldBundle:   fromRule("6", "rule-product", 0, "cat:product"),
			issuesclient.DefaultFieldAgent:    fromBoard("65"),
		}}
}

func newDispatcher(kanban *fakeKanban, kanbanURL, bridgeURL string, published *[]messages.BridgeInbound) *backlogDispatcher {
	return &backlogDispatcher{
		kanban:             issuesclient.New(kanbanURL),
		bridgeURL:          bridgeURL,
		board:              &kanban.board,
		view:               &kanban.view,
		inProgressColumnID: "col-in-progress",
		publish: func(in messages.BridgeInbound) error {
			*published = append(*published, in)
			return nil
		},
		maxDispatch: 10,
		degraded:    degradations.New("dispatcher"),
		modelRole:   "default",
		modelRoles:  stubModelRoles{},
	}
}

func TestEachCardIsDispatchedWithItsOwnEffectiveDefaults(t *testing.T) {
	kanban := &fakeKanban{
		t:     t,
		board: issuesclient.Board{ID: testBoardID, Name: "Task completion loop"},
		view: issuesclient.BoardView{Board: issuesclient.Board{ID: testBoardID}, Columns: []issuesclient.ColumnView{
			column(columnBacklog, testCard("card-product", "group:a", "cat:product"), testCard("card-plain", "group:b"), testCard("card-nowhere", "group:c")),
		}},
		children: map[string][]string{"parent": {"card-product", "card-plain", "card-nowhere"}},
		effective: map[string]issuesclient.EffectiveDefaults{
			"card-product": productCardDefaults(),
			"card-plain": {Tags: []string{"group:b"}, MatchedRuleIDs: []string{}, Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{
				issuesclient.DefaultFieldInstance: fromBoard("inst-cc-local"),
				issuesclient.DefaultFieldAgent:    fromBoard("65"),
			}},
			// No rule matches and the board sets no instance.
			"card-nowhere": {Tags: []string{"group:c"}, MatchedRuleIDs: []string{}, Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{}},
		},
	}
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()

	var published []messages.BridgeInbound
	dispatcher := newDispatcher(kanban, kanbanServer.URL, bridgeServer.URL, &published)
	dispatched, undispatched := dispatcher.dispatchBacklog([]scopedParent{{cardID: "parent", item: map[string]any{"title": "Goal"}}})

	if dispatched != 2 || len(published) != 2 {
		t.Fatalf("dispatched=%d published=%d, want 2 and 2", dispatched, len(published))
	}
	// Groups dispatch in key order: a (the product card), then b.
	product := published[0].Create
	if product == nil || product.InstanceID != "inst-codex-local" || product.Harness != msg.HarnessCodex || product.BundleID != "6" || product.AgentID != "northwind-builder" {
		t.Errorf("the tagged card must run on its rule's instance and bundle with the board's agent, got %+v", product)
	}
	plain := published[1].Create
	if plain == nil || plain.InstanceID != "inst-cc-local" || plain.Harness != msg.HarnessClaudeCode || plain.BundleID != "" || plain.AgentID != "northwind-builder" {
		t.Errorf("the untagged card must run on the board's instance with no bundle, got %+v", plain)
	}

	// Each session names its own card, whose tags pick its context sections.
	if product != nil && product.CardID != "card-product" || plain != nil && plain.CardID != "card-plain" {
		t.Errorf("each session must name the card it is for, got %q and %q", product.CardID, plain.CardID)
	}

	if len(undispatched) != 1 || undispatched[0].CardID != "card-nowhere" {
		t.Fatalf("undispatched = %+v, want only card-nowhere", undispatched)
	}
	reason := undispatched[0].Reason
	if !errors.Is(reason, errCardHasNoInstance) || !strings.Contains(reason.Error(), "default_instance_id") || !strings.Contains(reason.Error(), testBoardID) {
		t.Errorf("the refusal must be errCardHasNoInstance naming the setting and the board, got %v", reason)
	}
	for _, id := range append(kanban.linked, kanban.moved...) {
		if id == "card-nowhere" {
			t.Errorf("a card with no instance was linked or moved: linked=%v moved=%v", kanban.linked, kanban.moved)
		}
	}
	if len(kanban.moved) != 2 {
		t.Errorf("moved = %v, want the two dispatched cards", kanban.moved)
	}
}

func TestACardWhoseInstanceOrAgentTheBridgeLacksIsNotDispatched(t *testing.T) {
	kanban := &fakeKanban{
		t:     t,
		board: issuesclient.Board{ID: testBoardID, Name: "Task completion loop"},
		effective: map[string]issuesclient.EffectiveDefaults{
			"card-gone-instance": {Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{
				issuesclient.DefaultFieldInstance: fromRule("inst-gone", "rule-x", 2, "cat:x"),
			}},
			"card-gone-agent": {Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{
				issuesclient.DefaultFieldInstance: fromBoard("inst-cc-local"),
				issuesclient.DefaultFieldAgent:    fromBoard("404"),
			}},
		},
	}
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()
	kc := issuesclient.New(kanbanServer.URL)

	_, err := resolveCardSessionSpec(kc, bridgeServer.URL, &kanban.board, "card-gone-instance")
	if err == nil || !strings.Contains(err.Error(), "inst-gone") || !strings.Contains(err.Error(), "cat:x") {
		t.Errorf("an instance the bridge lacks must be refused naming it and the rule that set it, got %v", err)
	}
	_, err = resolveCardSessionSpec(kc, bridgeServer.URL, &kanban.board, "card-gone-agent")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "board") {
		t.Errorf("an agent id the bridge lacks must be refused naming it, got %v", err)
	}
	_, err = resolveCardSessionSpec(kc, bridgeServer.URL, &kanban.board, "card-not-in-noteboard")
	if !errors.Is(err, issuesclient.ErrCardNotFound) {
		t.Errorf("a card kanban-store cannot find must say so, got %v", err)
	}
}

func TestASiblingSessionIsReusedOnlyWhenStartedWithWhatTheCardResolvesTo(t *testing.T) {
	session := func(instance, agent, bundle string) string {
		body, _ := json.Marshal(map[string]string{
			"session_id": "sess-sibling", "state": "idle", "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
			"instance_id": instance, "agent_id": agent, "bundle_id": bundle,
		})
		return string(body)
	}
	cases := []struct {
		name      string
		session   string
		wantReuse bool
	}{
		{"same instance, agent and bundle", session("inst-codex-local", "northwind-builder", "6"), true},
		{"another instance", session("inst-cc-local", "northwind-builder", "6"), false},
		{"another agent", session("inst-codex-local", "argraphments", "6"), false},
		{"no bundle where the card has one", session("inst-codex-local", "northwind-builder", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kanban := &fakeKanban{
				t:     t,
				board: issuesclient.Board{ID: testBoardID, Name: "Task completion loop"},
				view: issuesclient.BoardView{Board: issuesclient.Board{ID: testBoardID}, Columns: []issuesclient.ColumnView{
					column(columnBacklog, testCard("card-next", "group:a", "cat:product")),
					column(columnDone, testCard("card-finished", "group:a", "cat:product")),
				}},
				children:  map[string][]string{"parent": {"card-next", "card-finished"}},
				effective: map[string]issuesclient.EffectiveDefaults{"card-next": productCardDefaults()},
				links: map[string][]issuesclient.CardLink{
					"card-finished": {{CardID: "card-finished", EntityType: linkTypeSession, EntityRef: "sess-sibling"}},
				},
			}
			kanbanServer := httptest.NewServer(kanban)
			defer kanbanServer.Close()
			bridgeServer := fakeBridge(t, map[string]string{"sess-sibling": tc.session})
			defer bridgeServer.Close()

			var published []messages.BridgeInbound
			dispatcher := newDispatcher(kanban, kanbanServer.URL, bridgeServer.URL, &published)
			dispatched, undispatched := dispatcher.dispatchBacklog([]scopedParent{{cardID: "parent", item: map[string]any{"title": "Goal"}}})
			if dispatched != 1 || len(undispatched) != 0 || len(published) != 1 {
				t.Fatalf("dispatched=%d undispatched=%v published=%d, want one dispatch", dispatched, undispatched, len(published))
			}
			in := published[0]
			reused := in.BusSessionID == "sess-sibling" && in.Create == nil
			if reused != tc.wantReuse {
				t.Errorf("reused=%v (session %s, create %+v), want reuse=%v", reused, in.BusSessionID, in.Create, tc.wantReuse)
			}
			if !tc.wantReuse && (in.Create == nil || in.Create.InstanceID != "inst-codex-local" || in.BusSessionID == "sess-sibling") {
				t.Errorf("a fresh session must be minted on the card's own instance, got session %s create %+v", in.BusSessionID, in.Create)
			}
		})
	}
}

// The tick carries on past a card with no instance and then fails, so the
// scheduler records `error` rather than a green run that dispatched nothing.
func TestARunThatLeftACardUndispatchedFails(t *testing.T) {
	build := func(nowhereHasInstance bool) *fakeKanban {
		nowhere := issuesclient.EffectiveDefaults{Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{}}
		if nowhereHasInstance {
			nowhere.Defaults[issuesclient.DefaultFieldInstance] = fromBoard("inst-cc-local")
		}
		board := issuesclient.Board{ID: testBoardID, Name: "Task completion loop"}
		return &fakeKanban{
			t:     t,
			board: board,
			view: issuesclient.BoardView{Board: board, Columns: []issuesclient.ColumnView{
				column(columnGoals, testCard("parent")),
				column(columnBacklog, testCard("card-plain", "group:a"), testCard("card-nowhere", "group:b")),
				column(columnInProgress), column(columnDone), column(columnFailed),
			}},
			scoped:   map[string]bool{"parent": true},
			children: map[string][]string{"parent": {"card-plain", "card-nowhere"}},
			effective: map[string]issuesclient.EffectiveDefaults{
				"card-plain":   {Defaults: map[issuesclient.DefaultField]issuesclient.EffectiveDefault{issuesclient.DefaultFieldInstance: fromBoard("inst-cc-local")}},
				"card-nowhere": nowhere,
			},
		}
	}
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()

	kanban := build(false)
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	err := run(runOptions{ModelRole: "default", ModelRoles: stubModelRoles{}, BoardID: testBoardID, BridgeURL: bridgeWithIssues(t, kanbanServer, bridgeServer), DryRun: true, MaxDispatch: 10})
	if err == nil || !strings.Contains(err.Error(), "card-nowhere") || strings.Contains(err.Error(), "card-plain") {
		t.Errorf("a run with a card nothing gives an instance must fail naming only that card, got %v", err)
	}

	resolvedKanban := build(true)
	resolvedServer := httptest.NewServer(resolvedKanban)
	defer resolvedServer.Close()
	if err := run(runOptions{ModelRole: "default", ModelRoles: stubModelRoles{}, BoardID: testBoardID, BridgeURL: bridgeWithIssues(t, resolvedServer, bridgeServer), DryRun: true, MaxDispatch: 10}); err != nil {
		t.Errorf("a run where every card resolves an instance must succeed — the board itself needs none: %v", err)
	}
}
