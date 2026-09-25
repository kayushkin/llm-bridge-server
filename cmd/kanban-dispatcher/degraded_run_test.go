package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/bus/messages"
	"github.com/slava/scheduler/internal/kanbanclient"
)

// These tests drive whole ticks through run. Each failure the tick cannot do
// its job through must fail the run naming what fell short, and each case where
// the tick simply had nothing to do must not.

// scopedBoard is a board with one scoped goal, "parent", and the given cards
// placed in the given columns. Every card is a child of the goal.
func scopedBoard(t *testing.T, columns map[string][]kanbanclient.CardView) *fakeKanban {
	board := kanbanclient.Board{ID: testBoardID, Name: "Task completion loop"}
	view := kanbanclient.BoardView{Board: board}
	var children []string
	placements := map[string][]kanbanclient.CardPlacement{}
	for _, name := range []string{columnGoals, columnBacklog, columnInProgress, columnDone, columnFailed} {
		cards := columns[name]
		if name == columnGoals {
			cards = append([]kanbanclient.CardView{testCard("parent")}, cards...)
		}
		col := column(name, cards...)
		view.Columns = append(view.Columns, col)
		for _, card := range cards {
			if card.Placement.CardID == "parent" {
				continue
			}
			children = append(children, card.Placement.CardID)
			placements[card.Placement.CardID] = []kanbanclient.CardPlacement{{CardID: card.Placement.CardID, BoardID: testBoardID, ColumnID: col.Column.ID}}
		}
	}
	return &fakeKanban{
		t:          t,
		board:      board,
		view:       view,
		scoped:     map[string]bool{"parent": true},
		children:   map[string][]string{"parent": children},
		effective:  map[string]kanbanclient.EffectiveDefaults{},
		placements: placements,
		status:     map[string]string{},
		failures:   map[string]int{},
	}
}

// tick runs one tick against kanban and a bridge holding sessions, publishing
// into a slice instead of the bus.
func tick(t *testing.T, kanban *fakeKanban, sessions map[string]string) (error, []messages.BridgeInbound) {
	t.Helper()
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, sessions)
	defer bridgeServer.Close()
	var published []messages.BridgeInbound
	err := run(runOptions{
		BoardID: testBoardID, KanbanURL: kanbanServer.URL, BridgeURL: bridgeServer.URL, MaxDispatch: 10,
		Publish: func(in messages.BridgeInbound) error {
			published = append(published, in)
			return nil
		},
	})
	return err, published
}

func wantShortfall(t *testing.T, err error, phrases ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the run must fail, naming %q; it succeeded", phrases)
	}
	for _, phrase := range phrases {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("the run's error must say %q, got: %v", phrase, err)
		}
	}
}

func TestAQuietBoardIsNotAShortfall(t *testing.T) {
	kanban := scopedBoard(t, nil)
	kanban.scoped = map[string]bool{}
	if err, _ := tick(t, kanban, nil); err != nil {
		t.Errorf("a board with nothing scoped has nothing to do, got %v", err)
	}
	if err, _ := tick(t, scopedBoard(t, nil), nil); err != nil {
		t.Errorf("a scoped goal with no children has nothing to do, got %v", err)
	}
}

func TestAGoalWhoseScopedTagCannotBeReadFailsTheRun(t *testing.T) {
	kanban := scopedBoard(t, nil)
	kanban.failures["tags"] = http.StatusServiceUnavailable
	err, _ := tick(t, kanban, nil)
	wantShortfall(t, err, "goal parent", "scoped tag could not be read")
}

func TestAGoalWhoseChildrenCannotBeListedFailsTheRun(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-next", "group:a")}})
	kanban.failures["children"] = http.StatusServiceUnavailable
	err, published := tick(t, kanban, nil)
	wantShortfall(t, err, "whole Backlog was invisible", "closure pass", "not rolled up")
	if len(published) != 0 {
		t.Errorf("nothing can be dispatched under a goal whose children are unknown, published %d", len(published))
	}
}

// A sibling that cannot be read still leaves the card dispatched, on a fresh
// session — the run fails because that session may not have been needed.
func TestASiblingThatCannotBeReadForReuseFailsTheRun(t *testing.T) {
	board := func() *fakeKanban {
		kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
			columnBacklog: {testCard("card-next", "group:a", "cat:product")},
			columnDone:    {testCard("card-finished", "group:a", "cat:product")},
		})
		kanban.effective["card-next"] = productCardDefaults()
		kanban.status["card-finished"] = "done"
		kanban.links = map[string][]kanbanclient.CardLink{
			"card-finished": {{CardID: "card-finished", EntityType: linkTypeSession, EntityRef: "sess-sibling"}},
		}
		return kanban
	}

	t.Run("links unreadable", func(t *testing.T) {
		kanban := board()
		kanban.failures["links"] = http.StatusServiceUnavailable
		err, published := tick(t, kanban, nil)
		wantShortfall(t, err, "sibling card card-finished could not be read")
		if len(published) != 1 || published[0].Create == nil {
			t.Errorf("the card must still be dispatched on a fresh session, published %+v", published)
		}
	})
	t.Run("session state unreadable", func(t *testing.T) {
		err, published := tick(t, board(), map[string]string{"sess-sibling": sessionUnreadable})
		wantShortfall(t, err, "state of sibling session sess-sibling could not be read")
		if len(published) != 1 || published[0].Create == nil {
			t.Errorf("the card must still be dispatched on a fresh session, published %+v", published)
		}
	})
	t.Run("siblings unlistable", func(t *testing.T) {
		// The first children read finds the Backlog card; the second, for its
		// siblings, fails.
		kanban := board()
		kanban.failures["children"] = http.StatusServiceUnavailable
		kanban.failAfter = map[string]int{"children": 1}
		err, published := tick(t, kanban, nil)
		wantShortfall(t, err, `group "a": its sibling cards could not be listed`)
		if len(published) != 1 || published[0].Create == nil {
			t.Errorf("the card must still be dispatched on a fresh session, published %+v", published)
		}
	})
	t.Run("session gone", func(t *testing.T) {
		err, published := tick(t, board(), nil)
		if err != nil {
			t.Errorf("a sibling session the bridge no longer has leaves nothing to revive, so minting fresh is right, got %v", err)
		}
		if len(published) != 1 || published[0].Create == nil {
			t.Errorf("the card must be dispatched on a fresh session, published %+v", published)
		}
	})
}

func TestARefusedPublishFailsTheRun(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnBacklog: {testCard("card-next", "group:a", "cat:product")}})
	kanban.effective["card-next"] = productCardDefaults()
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()
	err := run(runOptions{
		BoardID: testBoardID, KanbanURL: kanbanServer.URL, BridgeURL: bridgeServer.URL, MaxDispatch: 10,
		Publish: func(messages.BridgeInbound) error { return errors.New("bus down") },
	})
	wantShortfall(t, err, "card-next", "nats publish: bus down")
	if len(kanban.linked) != 0 || len(kanban.moved) != 0 {
		t.Errorf("a card whose publish was refused must not be linked or moved: linked=%v moved=%v", kanban.linked, kanban.moved)
	}
}

func TestTheClosurePass(t *testing.T) {
	// card-finished is done but still sits in In Progress; card-busy is open.
	board := func() *fakeKanban {
		kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
			columnInProgress: {testCard("card-finished", "group:a"), testCard("card-busy", "group:b")},
		})
		kanban.status["card-finished"] = "done"
		return kanban
	}

	t.Run("a settled child is moved", func(t *testing.T) {
		kanban := board()
		if err, _ := tick(t, kanban, nil); err != nil {
			t.Fatalf("the closure pass must succeed, got %v", err)
		}
		if len(kanban.moved) != 1 || kanban.moved[0] != "card-finished" {
			t.Errorf("moved = %v, want only card-finished", kanban.moved)
		}
	})
	t.Run("placement unreadable", func(t *testing.T) {
		kanban := board()
		kanban.failures["placements"] = http.StatusUnauthorized
		err, _ := tick(t, kanban, nil)
		wantShortfall(t, err, "child card-finished is done but its placement could not be read")
		if len(kanban.moved) != 0 {
			t.Errorf("a child whose placement is unknown must not be moved, moved %v", kanban.moved)
		}
	})
	t.Run("not on this board", func(t *testing.T) {
		kanban := board()
		kanban.placements["card-finished"] = []kanbanclient.CardPlacement{{CardID: "card-finished", BoardID: "board-elsewhere", ColumnID: "col-x"}}
		if err, _ := tick(t, kanban, nil); err != nil {
			t.Errorf("a child placed on another board has nothing here to correct, got %v", err)
		}
		if len(kanban.moved) != 0 {
			t.Errorf("a child not on this board must not be moved, moved %v", kanban.moved)
		}
	})
	t.Run("move refused", func(t *testing.T) {
		kanban := board()
		kanban.failures["move"] = http.StatusConflict
		err, _ := tick(t, kanban, nil)
		wantShortfall(t, err, "child card-finished is done but the board refused to move it to Done")
	})
}

func TestTheRollup(t *testing.T) {
	board := func() *fakeKanban {
		kanban := scopedBoard(t, map[string][]kanbanclient.CardView{columnDone: {testCard("card-a")}, columnFailed: {testCard("card-b")}})
		kanban.status["card-a"] = "done"
		kanban.status["card-b"] = "archived"
		return kanban
	}

	t.Run("a settled goal is noted and moved to Done", func(t *testing.T) {
		kanban := board()
		if err, _ := tick(t, kanban, nil); err != nil {
			t.Fatalf("the rollup must succeed, got %v", err)
		}
		if len(kanban.patched) != 1 || kanban.patched[0] != "parent" || len(kanban.moved) != 1 || kanban.moved[0] != "parent" {
			t.Errorf("patched=%v moved=%v, want the goal noted and moved", kanban.patched, kanban.moved)
		}
	})
	t.Run("an unsettled goal is left alone", func(t *testing.T) {
		kanban := board()
		kanban.status["card-b"] = "open"
		if err, _ := tick(t, kanban, nil); err != nil {
			t.Errorf("a goal with open children has nothing to roll up, got %v", err)
		}
		if len(kanban.moved) != 0 {
			t.Errorf("an unsettled goal must not be moved, moved %v", kanban.moved)
		}
	})
	t.Run("rollup note refused", func(t *testing.T) {
		kanban := board()
		kanban.failures["patch"] = http.StatusServiceUnavailable
		err, _ := tick(t, kanban, nil)
		wantShortfall(t, err, "refused its rollup note")
		if len(kanban.moved) != 1 || kanban.moved[0] != "parent" {
			t.Errorf("the goal is still moved to Done without its note, moved %v", kanban.moved)
		}
	})
	t.Run("move to Done refused", func(t *testing.T) {
		kanban := board()
		kanban.failures["move"] = http.StatusConflict
		err, _ := tick(t, kanban, nil)
		wantShortfall(t, err, "refused to move it to Done", "stays in Goals")
	})
}

// kanban-store gates every route, so the closure pass's placement read must
// carry the service token like every other kanban call.
func TestThePlacementReadCarriesTheServiceToken(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get(kanbanclient.ServiceTokenHeader)
		w.Write([]byte(`[{"card_id":"c1","board_id":"` + testBoardID + `","column_id":"col-done"}]`))
	}))
	defer server.Close()
	column, err := currentPlacement(kanbanclient.New(server.URL, "secret-token"), "c1", testBoardID)
	if err != nil || column != "col-done" {
		t.Fatalf("currentPlacement = %q, %v; want col-done", column, err)
	}
	if authorization != "secret-token" {
		t.Errorf("the placement read sent service token %q, want secret-token", authorization)
	}
}

// A sibling session that is busy, or last updated outside the revive window,
// is the reuse rule working: it is not reused and not a shortfall.
func TestABusyOrStaleSiblingSessionIsNotAShortfall(t *testing.T) {
	session := func(state string, updated time.Time) string {
		return `{"state":"` + state + `","updated_at":"` + updated.UTC().Format(time.RFC3339) + `","instance_id":"inst-codex-local","agent_id":"northwind-builder","bundle_id":"6"}`
	}
	cases := map[string]string{
		"busy":  session("running", time.Now()),
		"stale": session("idle", time.Now().Add(-2*reviveWindow)),
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
				"card-finished": {{CardID: "card-finished", EntityType: linkTypeSession, EntityRef: "sess-sibling"}},
			}
			err, published := tick(t, kanban, map[string]string{"sess-sibling": body})
			if err != nil {
				t.Errorf("a %s sibling session is not a shortfall, got %v", name, err)
			}
			if len(published) != 1 || published[0].Create == nil {
				t.Errorf("the card must be dispatched on a fresh session, published %+v", published)
			}
		})
	}
}

// Cards past the per-tick cap keep their Backlog placement for the next tick.
func TestThePerTickCapIsNotAShortfall(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnBacklog: {testCard("card-a", "group:a"), testCard("card-b", "group:b")},
	})
	instance := kanbanclient.EffectiveDefaults{Defaults: map[kanbanclient.DefaultField]kanbanclient.EffectiveDefault{kanbanclient.DefaultFieldInstance: fromBoard("inst-cc-local")}}
	kanban.effective["card-a"], kanban.effective["card-b"] = instance, instance
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()
	var published []messages.BridgeInbound
	err := run(runOptions{
		BoardID: testBoardID, KanbanURL: kanbanServer.URL, BridgeURL: bridgeServer.URL, MaxDispatch: 1,
		Publish: func(in messages.BridgeInbound) error {
			published = append(published, in)
			return nil
		},
	})
	if err != nil {
		t.Errorf("a card deferred by the per-tick cap is not a shortfall, got %v", err)
	}
	if len(published) != 1 {
		t.Errorf("published %d, want 1 under a cap of 1", len(published))
	}
}

func TestADryRunWritesNothing(t *testing.T) {
	kanban := scopedBoard(t, map[string][]kanbanclient.CardView{
		columnInProgress: {testCard("card-a")},
		columnFailed:     {testCard("card-b")},
	})
	kanban.status["card-a"] = "done"
	kanban.status["card-b"] = "archived"
	kanbanServer := httptest.NewServer(kanban)
	defer kanbanServer.Close()
	bridgeServer := fakeBridge(t, nil)
	defer bridgeServer.Close()
	if err := run(runOptions{BoardID: testBoardID, KanbanURL: kanbanServer.URL, BridgeURL: bridgeServer.URL, MaxDispatch: 10, DryRun: true}); err != nil {
		t.Fatalf("a dry run over a settled goal must succeed, got %v", err)
	}
	if len(kanban.moved) != 0 || len(kanban.patched) != 0 || len(kanban.linked) != 0 {
		t.Errorf("a dry run wrote to the board: moved=%v patched=%v linked=%v", kanban.moved, kanban.patched, kanban.linked)
	}
}
