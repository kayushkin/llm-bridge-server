package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schedulerconfig "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// The stubs below stand in for the board under llm-bridge-server's /issues and
// its oneshot route, which is where the decomposition call goes. Each fault field is a status code: zero means "answer
// normally", non-zero means answer with that status, which is how every failure
// this suite is about arrives in production — the service answering, just not
// with the status the client wanted.

const (
	stubBoardID    = "board-1"
	stubColGoals   = "col-goals"
	stubColBacklog = "col-backlog"
	stubBoardName  = "Task completion loop"
	// stubModelRole is the role the stub scheduler holds and the stub model
	// expects to be asked for.
	stubModelRole = "balanced"
)

// stubGoal is one card in the Goals column as the scoper meets it: a placement,
// the noteboard item behind it, and whether it already carries `scoped`.
type stubGoal struct {
	cardID   string
	title    string
	body     string
	tags     []string // noteboard tags; must contain tagTrigger to be picked
	isScoped bool     // already carries the `scoped` entity_tag
}

type stubKanban struct {
	t     *testing.T
	goals []stubGoal
	// blankBacklogID is the one way the Backlog column can resolve to an empty
	// id in production. run calls EnsureBoard first, and EnsureBoard CREATES any
	// column the board is missing — so an absent Backlog column is not
	// reachable. A column row that is present under the right name and carries a
	// blank id is: EnsureBoard sees the name, skips creation, and caches "".
	blankBacklogID bool
	failTags       int // status to answer GET /entities/note/{id}/tags with
	failCreate     int // status to answer POST /boards/{id}/cards with
	failLink       int // status to answer POST /cards/{id}/links with
	failStamp      int // status to answer POST /entities/note/{id}/tags with

	created []map[string]any  // every card body the scoper posted
	linked  map[string]string // child card id -> parent it was linked to
	stamped []string          // parent card ids stamped `scoped`
	server  *httptest.Server
}

func newStubKanban(t *testing.T, goals ...stubGoal) *stubKanban {
	k := &stubKanban{t: t, goals: goals, linked: map[string]string{}}
	k.server = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.server.Close)
	return k
}

func (k *stubKanban) URL() string { return k.server.URL }

func (k *stubKanban) columns() []map[string]any {
	cols := []map[string]any{
		{"id": stubColGoals, "board_id": stubBoardID, "name": columnGoals, "position": 1.0},
	}
	backlogID := stubColBacklog
	if k.blankBacklogID {
		backlogID = ""
	}
	cols = append(cols, map[string]any{"id": backlogID, "board_id": stubBoardID, "name": columnBacklog, "position": 2.0})
	for i, n := range []string{columnInProgress, columnDone, columnFailed} {
		cols = append(cols, map[string]any{"id": "col-" + n, "board_id": stubBoardID, "name": n, "position": float64(i + 3)})
	}
	return cols
}

func (k *stubKanban) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == "GET" && p == "/boards/"+stubBoardID:
		writeJSON(w, 200, map[string]any{"id": stubBoardID, "name": stubBoardName})

	case r.Method == "GET" && p == "/boards/"+stubBoardID+"/columns":
		writeJSON(w, 200, k.columns())

	// The board view. Only the Goals column carries cards; every other column
	// is present and empty, which is what the live board looks like on the
	// ticks this job actually has.
	case r.Method == "GET" && p == "/boards/"+stubBoardID+"/cards":
		var colViews []map[string]any
		for _, col := range k.columns() {
			var cards []map[string]any
			if col["name"] == columnGoals {
				for _, g := range k.goals {
					cards = append(cards, map[string]any{
						"placement": map[string]any{"card_id": g.cardID, "board_id": stubBoardID, "column_id": stubColGoals, "position": 1.0},
						"item":      map[string]any{"id": g.cardID, "title": g.title, "body": g.body, "tags": toAny(g.tags)},
					})
				}
			}
			colViews = append(colViews, map[string]any{"column": col, "cards": cards})
		}
		writeJSON(w, 200, map[string]any{
			"board":   map[string]any{"id": stubBoardID, "name": stubBoardName},
			"columns": colViews,
		})

	// createSubcard posts here directly rather than through issuesclient.
	case r.Method == "POST" && p == "/boards/"+stubBoardID+"/cards":
		if k.failCreate != 0 {
			w.WriteHeader(k.failCreate)
			fmt.Fprint(w, "board refused the card")
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		k.created = append(k.created, payload)
		writeJSON(w, 201, map[string]any{"item": map[string]any{"id": fmt.Sprintf("child-%d", len(k.created))}})

	case r.Method == "POST" && strings.HasSuffix(p, "/links"):
		if k.failLink != 0 {
			w.WriteHeader(k.failLink)
			fmt.Fprint(w, "board refused the link")
			return
		}
		cardID := strings.TrimSuffix(strings.TrimPrefix(p, "/cards/"), "/links")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		ref, _ := payload["entity_ref"].(string)
		k.linked[cardID] = ref
		writeJSON(w, 201, map[string]any{"id": "link-" + cardID})

	case strings.HasPrefix(p, "/entities/note/") && strings.HasSuffix(p, "/tags"):
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/entities/note/"), "/tags")
		if r.Method == "GET" {
			if k.failTags != 0 {
				w.WriteHeader(k.failTags)
				fmt.Fprint(w, "tag read refused")
				return
			}
			var tags []map[string]any
			for _, g := range k.goals {
				if g.cardID == ref && g.isScoped {
					tags = append(tags, map[string]any{"entity_type": "note", "entity_ref": ref, "tag": tagScoped})
				}
			}
			writeJSON(w, 200, tags)
			return
		}
		if k.failStamp != 0 {
			w.WriteHeader(k.failStamp)
			fmt.Fprint(w, "stamp refused")
			return
		}
		k.stamped = append(k.stamped, ref)
		writeJSON(w, 201, map[string]any{"entity_type": "note", "entity_ref": ref, "tag": tagScoped})

	default:
		k.t.Errorf("stub kanban: unexpected %s %s", r.Method, p)
		w.WriteHeader(500)
	}
}

// stubModel answers llm-bridge-server's oneshot route. It exists so that no
// test in this package can send a real model call — the hazard measured on the
// sibling spawn path in card `f8fbd728`.
type stubModel struct {
	t        *testing.T
	subtasks []Subtask
	status   int  // non-200 makes the decompose call fail
	noParsed bool // a 200 whose reply carries no parsed JSON
	calls    int
	server   *httptest.Server
}

func newStubModel(t *testing.T, subtasks ...Subtask) *stubModel {
	m := &stubModel{t: t, subtasks: subtasks}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/oneshot" {
			t.Errorf("stub model: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var request struct {
			ModelRole string `json:"model_role"`
			Caller    string `json:"caller"`
			Model     string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("stub model: body: %v", err)
		}
		if request.ModelRole != stubModelRole || request.Caller != "kanban-scoper" || request.Model != "" {
			t.Errorf("stub model: request = %+v, want role %s from kanban-scoper and no model", request, stubModelRole)
		}
		m.calls++
		if m.status != 0 {
			w.WriteHeader(m.status)
			fmt.Fprint(w, `{"error":{"message":"credit balance is too low"}}`)
			return
		}
		if m.noParsed {
			writeJSON(w, 200, map[string]any{"stop_reason": "end_turn", "text": "no"})
			return
		}
		items := make([]map[string]any, 0, len(m.subtasks))
		for _, st := range m.subtasks {
			items = append(items, map[string]any{
				"title": st.Title, "body": st.Body, "group_key": st.GroupKey,
				"capability": st.Capability, "role": st.Role,
			})
		}
		writeJSON(w, 200, map[string]any{"stop_reason": "end_turn", "parsed": map[string]any{"subtasks": items}})
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *stubModel) URL() string { return m.server.URL }

// newStubScheduler answers GET /settings with the scoper's model role, as the
// scheduler service does.
func newStubScheduler(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/settings" {
			t.Errorf("stub scheduler: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		writeJSON(w, 200, msg.ServiceSettings{Service: "scheduler", Settings: []msg.ServiceSetting{{
			Key: schedulerconfig.SettingKanbanScoperModelRole, ValueType: msg.ServiceSettingValueTypeModelRole, Value: stubModelRole,
		}}})
	}))
	t.Cleanup(server.Close)
	return server
}

// newStubBridge is llm-bridge-server as the scoper reaches it: the board under
// /issues, the scheduler's settings under /scheduler, and /oneshot at the
// root, each answered by its stub with the prefix taken off.
func newStubBridge(t *testing.T, issues, scheduler, oneshot http.Handler) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("/issues/", http.StripPrefix("/issues", issues))
	mux.Handle("/scheduler/", http.StripPrefix("/scheduler", scheduler))
	mux.Handle("/", oneshot)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// testConfig wires a run at the three stubs, behind one bridge address, and
// nowhere else.
func testConfig(k *stubKanban, decomposer *stubModel) config {
	bridge := newStubBridge(decomposer.t, k.server.Config.Handler, newStubScheduler(decomposer.t).Config.Handler, decomposer.server.Config.Handler)
	return config{
		BridgeURL: bridge.URL,
		BoardID:   stubBoardID,
		MaxScope:  maxScopePerTick,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func plainSubtask(title string) Subtask {
	return Subtask{Title: title, Body: "do the thing", GroupKey: "grp"}
}
