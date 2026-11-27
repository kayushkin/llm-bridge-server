package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// Ported from 18ecd596 and f119a711 (test/the-mint-site-truncates-a-long-parent-
// id): the mechanisms of a whole tick that main's degradation tests did not
// already pin. Column ids, entity types and ids are written as literals, so a
// drifted wire value in main.go goes red here instead of moving the fixture
// with it.

func instanceOnly() kanbanclient.EffectiveDefaults {
	return kanbanclient.EffectiveDefaults{Defaults: map[kanbanclient.DefaultField]kanbanclient.EffectiveDefault{
		kanbanclient.DefaultFieldInstance: fromBoard("inst-cc-local"),
	}}
}

// ============================ a tick that cannot start ============================

func TestATickThatCannotReadItsBoardFailsAndPublishesNothing(t *testing.T) {
	cases := map[string]struct {
		route  string
		status int
		want   string
	}{
		"board unreadable": {"board", http.StatusServiceUnavailable, "get board " + testBoardID},
		"board gone":       {"board", http.StatusNotFound, "does not exist"},
		"view unreadable":  {"view", http.StatusServiceUnavailable, "board view"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-a", "group:a")}})
			kanban.effective["card-a"] = instanceOnly()
			kanban.failures[tc.route] = tc.status
			err, published := tick(t, kanban, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the tick must fail saying %q, got %v", tc.want, err)
			}
			if len(published) != 0 {
				t.Errorf("a tick that could not read its board published %d", len(published))
			}
		})
	}
}

func TestABoardMissingALifecycleColumnFailsTheTick(t *testing.T) {
	kanban := scopedBoard(t, nil)
	kanban.view.Columns = kanban.view.Columns[:len(kanban.view.Columns)-1] // drop Failed
	err, _ := tick(t, kanban, nil)
	if err == nil || !strings.Contains(err.Error(), `missing required column "Failed"`) {
		t.Errorf("a board with no Failed column must fail the tick naming it, got %v", err)
	}
}

// ============================ which cards are dispatched ============================

// A goal nobody has decomposed yet has no scoped tag, and its cards must wait.
func TestAGoalWithoutTheScopedTagHasNothingDispatched(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-a", "group:a")}})
	kanban.effective["card-a"] = instanceOnly()
	kanban.scoped = map[string]bool{}
	err, published := tick(t, kanban, nil)
	if err != nil {
		t.Errorf("an unscoped goal is not a shortfall, got %v", err)
	}
	if len(published) != 0 || len(kanban.moved) != 0 {
		t.Errorf("an unscoped goal's card was dispatched: published=%d moved=%v", len(published), kanban.moved)
	}
}

func TestADispatchedCardIsLinkedToItsSessionAndMovedToInProgress(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-a", "group:a")}})
	kanban.effective["card-a"] = instanceOnly()
	err, published := tick(t, kanban, nil)
	if err != nil || len(published) != 1 {
		t.Fatalf("err=%v published=%d, want one dispatch", err, len(published))
	}
	sessionID := published[0].BusSessionID
	// scopedBoard's goal is "parent": six bytes, so it is minted whole.
	if !regexp.MustCompile(`^tcl-parent-a-\d+$`).MatchString(sessionID) {
		t.Errorf("session id %q must name the parent goal and the group", sessionID)
	}
	link := kanban.sent["link:card-a"]
	if link["entity_type"] != "session" || link["entity_ref"] != sessionID {
		t.Errorf("session link = %v, want entity_type session and entity_ref %q", link, sessionID)
	}
	move := kanban.sent["move:card-a"]
	if move["column_id"] != "col-in-progress" || move["board_id"] != testBoardID {
		t.Errorf("move = %v, want In Progress (col-in-progress) on %s", move, testBoardID)
	}
	// Position 0 is the top of the column, where a person looking at the board
	// sees the card arrive.
	if move["position"] != float64(0) {
		t.Errorf("the dispatched card moved to position %v, want 0 (the top of In Progress)", move["position"])
	}
}

func TestACardWhoseSessionLinkFailedIsNotMoved(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-a", "group:a")}})
	kanban.effective["card-a"] = instanceOnly()
	kanban.failures["link"] = http.StatusServiceUnavailable
	err, _ := tick(t, kanban, nil)
	wantShortfall(t, err, "card-a", "linking it to the card failed")
	if len(kanban.moved) != 0 {
		t.Errorf("a card the board points at no session for must stay in Backlog, moved %v", kanban.moved)
	}
}

// ============================ reviving a sibling ============================

// An age that cannot be read is not a fresh one: reviving on it could wake a
// session of any age. It is the rule working, so it is not a shortfall.
func TestASiblingSessionOfUnknownAgeIsNotRevived(t *testing.T) {
	cases := map[string]string{
		"no updated_at":          `{"state":"idle","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`,
		"unparseable updated_at": `{"state":"idle","updated_at":"yesterday","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
				columnBacklog: {testCard("card-next", "group:a", "cat:product")},
				columnDone:    {testCard("card-finished", "group:a", "cat:product")},
			})
			kanban.effective["card-next"] = productCardDefaults()
			kanban.status["card-finished"] = "done"
			kanban.links = map[string][]kanbanclient.CardLink{
				"card-finished": {{CardID: "card-finished", EntityType: "session", EntityRef: "sess-sibling"}},
			}
			err, published := tick(t, kanban, map[string]string{"sess-sibling": body})
			if err != nil {
				t.Errorf("a sibling of unknown age is not a shortfall, got %v", err)
			}
			if len(published) != 1 || published[0].Create == nil || published[0].BusSessionID == "sess-sibling" {
				t.Errorf("the card must get a fresh session, published %+v", published)
			}
		})
	}
}

func TestGroupSiblingsAreTheParentsCardsInTheSameGroupOnly(t *testing.T) {
	kanban := &fakeKanban{
		t:     t,
		board: kanbanclient.Board{ID: testBoardID},
		view: kanbanclient.BoardView{Columns: []kanbanclient.ColumnView{
			column(columnBacklog, testCard("card-next", "group:a")),
			column(columnDone, testCard("sibling", "group:a"), testCard("other-group", "group:b"), testCard("other-goal", "group:a")),
		}},
		children: map[string][]string{"parent": {"card-next", "sibling", "other-group"}, "goal-2": {"other-goal"}},
	}
	server := httptest.NewServer(kanban)
	defer server.Close()
	got := groupSiblings(kanbanclient.New(server.URL, ""), &kanban.view, groupOf("parent", testCard("card-next", "group:a")), degradations.New("dispatcher"))
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Placement.CardID)
	}
	if strings.Join(ids, ",") != "card-next,sibling" {
		t.Errorf("siblings = %v, want card-next and sibling — not another group's card, not another goal's", ids)
	}
}

// ============================ closure and rollup ============================

func TestTheClosurePassMovesEachSettledChildToItsOwnColumn(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnInProgress: {testCard("card-done", "group:a"), testCard("card-archived", "group:b"), testCard("card-open", "group:c")},
	})
	kanban.status["card-done"] = "done"
	kanban.status["card-archived"] = "archived"
	if err, _ := tick(t, kanban, nil); err != nil {
		t.Fatalf("the closure pass must succeed, got %v", err)
	}
	if got := kanban.sent["move:card-done"]["column_id"]; got != "col-done" {
		t.Errorf("a done child moved to %v, want col-done", got)
	}
	if got := kanban.sent["move:card-archived"]["column_id"]; got != "col-failed" {
		t.Errorf("an archived child moved to %v, want col-failed", got)
	}
	for _, card := range []string{"card-done", "card-archived"} {
		if got := kanban.sent["move:"+card]["position"]; got != float64(0) {
			t.Errorf("%s moved to position %v, want 0 (the top of its column)", card, got)
		}
	}
	if _, moved := kanban.sent["move:card-open"]; moved {
		t.Error("a child still being worked on must not be moved")
	}
}

func TestTheRollupAppendsItsNoteAndCountsFailedChildrenApart(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnDone: {testCard("card-a")}, columnFailed: {testCard("card-b")}})
	kanban.view.Columns[0].Cards[0].Item["body"] = "The goal as written."
	kanban.status["card-a"] = "done"
	kanban.status["card-b"] = "archived"
	if err, _ := tick(t, kanban, nil); err != nil {
		t.Fatalf("the rollup must succeed, got %v", err)
	}
	body, _ := kanban.sent["patch:parent"]["body"].(string)
	if !strings.HasPrefix(body, "The goal as written.\n\n---\n_Rolled up by kanban-dispatcher at ") {
		t.Errorf("the rollup note must be appended to the goal's body, got %q", body)
	}
	if !strings.Contains(body, "children: done=1 failed=1 open=0") {
		t.Errorf("the rollup note must count the done and the failed child apart, got %q", body)
	}
	if got := kanban.sent["move:parent"]["column_id"]; got != "col-done" {
		t.Errorf("the goal moved to %v, want col-done", got)
	}
	if got := kanban.sent["move:parent"]["position"]; got != float64(0) {
		t.Errorf("the goal moved to position %v, want 0 (the top of Done)", got)
	}
}

// A card can sit on several boards; the closure pass must read the column on
// the board it is correcting.
func TestCurrentPlacementAnswersForTheBoardAskedAbout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"card_id":"c1","board_id":"board-elsewhere","column_id":"col-x"},{"card_id":"c1","board_id":"` + testBoardID + `","column_id":"col-done"}]`))
	}))
	defer server.Close()
	column, err := currentPlacement(kanbanclient.New(server.URL, ""), "c1", testBoardID)
	if err != nil || column != "col-done" {
		t.Errorf("currentPlacement = %q, %v; want col-done, the column on %s", column, err, testBoardID)
	}
}

// The session read's updated_at is stamped by the bridge in RFC 3339; a sibling
// updated a minute ago must still be revived after the stricter age check.
func TestAFreshIdleSiblingIsStillRevived(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnBacklog: {testCard("card-next", "group:a", "cat:product")},
		columnDone:    {testCard("card-finished", "group:a", "cat:product")},
	})
	kanban.effective["card-next"] = productCardDefaults()
	kanban.status["card-finished"] = "done"
	kanban.links = map[string][]kanbanclient.CardLink{
		"card-finished": {{CardID: "card-finished", EntityType: "session", EntityRef: "sess-sibling"}},
	}
	body := `{"state":"idle","updated_at":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`
	err, published := tick(t, kanban, map[string]string{"sess-sibling": body})
	if err != nil || len(published) != 1 || published[0].BusSessionID != "sess-sibling" || published[0].Create != nil {
		t.Errorf("an idle sibling updated a minute ago must be revived: err=%v published=%+v", err, published)
	}
}

// A goal with no children has not been decomposed yet, not finished: rolling
// it up would close work nobody started.
func TestAGoalWithNoChildrenIsNotRolledUp(t *testing.T) {
	kanban := scopedBoard(t, nil)
	if err, _ := tick(t, kanban, nil); err != nil {
		t.Fatalf("a goal with no children is not a shortfall, got %v", err)
	}
	if len(kanban.moved) != 0 || len(kanban.patched) != 0 {
		t.Errorf("a goal with no children was rolled up: moved=%v patched=%v", kanban.moved, kanban.patched)
	}
}
