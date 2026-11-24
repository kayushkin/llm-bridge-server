package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
)

// mkColumnWithWorkState creates a column mapped onto the shared vocabulary;
// workState "" leaves it unmapped.
func mkColumnWithWorkState(t *testing.T, h http.Handler, boardID, name, workState string) string {
	t.Helper()
	request := model.CreateColumnRequest{Name: name}
	if workState != "" {
		request.WorkState = &workState
	}
	var column model.Column
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/columns", request), &column)
	return column.ID
}

func stringPointer(value string) *string { return &value }

func workStateText(state *model.WorkState) string {
	if state == nil {
		return "<null>"
	}
	return string(*state)
}

func TestWorkStateVocabularyIsServedWithMeanings(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	var states []model.WorkStateMeaning
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/work-states", nil), &states)
	want := []model.WorkState{"not_started", "working", "waiting", "done", "dropped"}
	if len(states) != len(want) {
		t.Fatalf("got %d states, want %d: %+v", len(states), len(want), states)
	}
	for i, state := range states {
		if state.WorkState != want[i] || state.Meaning == "" {
			t.Errorf("state %d = %+v, want %q with a meaning", i, state, want[i])
		}
	}
	if !strings.Contains(states[2].Meaning, "blocked on a person or on another piece of work") {
		t.Errorf("waiting means %q", states[2].Meaning)
	}
}

// A column's work_state outside the vocabulary is refused on create and on
// update, naming the vocabulary, and nothing is written. The word is taken as
// written: "Working" is not "working".
func TestColumnWorkStateOutsideTheVocabularyIsRefused(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Northwind work")

	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "Blocked", WorkState: stringPointer("blocked")})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "not_started, working, waiting, done, dropped") {
		t.Fatalf("create with work_state blocked: want 400 naming the vocabulary, got %d: %s", w.Code, w.Body.String())
	}
	var columns []model.Column
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/columns", nil), &columns)
	if len(columns) != 0 {
		t.Fatalf("a refused create wrote a column: %+v", columns)
	}

	columnID := mkColumnWithWorkState(t, h, boardID, "In Progress", "working")
	for _, refused := range []string{"Working", " working", "in_progress"} {
		w := do(t, h, "PATCH", "/api/columns/"+columnID, model.UpdateColumnRequest{WorkState: stringPointer(refused)})
		if w.Code != 400 {
			t.Errorf("patch work_state %q: want 400, got %d: %s", refused, w.Code, w.Body.String())
		}
	}
	var column model.Column
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/columns/"+columnID, nil), &column)
	if workStateText(column.WorkState) != "working" {
		t.Fatalf("refused patches changed work_state to %s", workStateText(column.WorkState))
	}

	// An empty string clears it, and a cleared column says null rather than
	// leaving the field out.
	w = do(t, h, "PATCH", "/api/columns/"+columnID, model.UpdateColumnRequest{WorkState: stringPointer("")})
	var raw map[string]any
	decodeSuccessfulResponse(t, w, &raw)
	if value, present := raw["work_state"]; !present || value != nil {
		t.Fatalf("cleared column: want work_state null, got %v (present %v)", value, present)
	}
}

// work_state writes nothing: a card moved into a `done` column with no
// auto_status keeps noteboard's status as it was.
func TestWorkStateDoesNotImplyAutoStatus(t *testing.T) {
	h, notes, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Board")
	queued := mkColumnWithWorkState(t, h, boardID, "Queued", "not_started")
	done := mkColumnWithWorkState(t, h, boardID, "Done", "done")
	var card model.CardView
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "ship it", ColumnID: queued}), &card)
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/cards/"+card.Placement.CardID+"/move", model.MoveCardRequest{BoardID: boardID, ColumnID: done}), &map[string]any{})
	if status := notes.status(card.Placement.CardID); status != "open" {
		t.Fatalf("noteboard status after a move into a done work_state column = %q, want open", status)
	}
}

// One card on two boards: its shared state is the column it last moved into,
// whichever board that is on, and a latest column with no work_state answers
// null even though the other board's column has one.
func TestCardWorkStateIsTheLatestMoveAcrossBoards(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	support := mkBoard(t, h, "Support Desk")
	actionNeeded := mkColumnWithWorkState(t, h, support, "Action needed", "not_started")
	waitingOnReply := mkColumnWithWorkState(t, h, support, "Waiting on reply", "waiting")
	northwind := mkBoard(t, h, "Northwind work")
	inProgress := mkColumnWithWorkState(t, h, northwind, "In Progress", "working")
	unmapped := mkColumnWithWorkState(t, h, northwind, "Someday", "")

	var created model.CardView
	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+support+"/cards", model.CreateCardRequest{Title: "refund", ColumnID: actionNeeded}), &created)
	cardID := created.Placement.CardID
	if workStateText(created.WorkState) != "not_started" || created.WorkStateSource == nil || created.WorkStateSource.BoardID != support {
		t.Fatalf("new card: work_state %s from %+v, want not_started from Support", workStateText(created.WorkState), created.WorkStateSource)
	}

	expect := func(step, wantState, wantBoard, wantColumn string) {
		t.Helper()
		var detail model.CardDetail
		decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID, nil), &detail)
		if workStateText(detail.WorkState) != wantState || detail.WorkStateSource == nil ||
			detail.WorkStateSource.BoardID != wantBoard || detail.WorkStateSource.ColumnID != wantColumn {
			t.Fatalf("%s: card says %s from %+v, want %s from board %s column %s", step, workStateText(detail.WorkState), detail.WorkStateSource, wantState, wantBoard, wantColumn)
		}
		// Every board the card is on answers the same shared state.
		for _, boardID := range []string{support, northwind} {
			var view model.BoardView
			decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil), &view)
			found := false
			for _, column := range view.Columns {
				for _, card := range column.Cards {
					if card.Placement.CardID != cardID {
						continue
					}
					found = true
					if workStateText(card.WorkState) != wantState || card.WorkStateSource == nil || card.WorkStateSource.BoardID != wantBoard {
						t.Fatalf("%s: board %s shows %s from %+v, want %s from %s", step, boardID, workStateText(card.WorkState), card.WorkStateSource, wantState, wantBoard)
					}
				}
			}
			if !found && boardID == support {
				t.Fatalf("%s: card missing from Support's board view", step)
			}
		}
	}
	expect("created on Support", "not_started", support, actionNeeded)

	mustStatus(t, do(t, h, "PUT", "/api/boards/"+northwind+"/cards/"+cardID, model.AttachCardRequest{ColumnID: inProgress}), 201, "attach to Northwind")
	expect("placed on Northwind after", "working", northwind, inProgress)

	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{BoardID: support, ColumnID: waitingOnReply}), &map[string]any{})
	expect("moved on Support last", "waiting", support, waitingOnReply)

	decodeSuccessfulResponse(t, do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{BoardID: northwind, ColumnID: unmapped}), &map[string]any{})
	expect("moved into an unmapped column last", "<null>", northwind, unmapped)
}

// Two placements moved at the same instant: the lower board id wins, so the
// answer is the same on every read.
func TestSharedWorkStateBreaksATieOnBoardID(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	working, waiting := model.WorkStateWorking, model.WorkStateWaiting
	placements := []model.PlacementWorkState{
		{BoardID: "board-b", ColumnID: "col-b", WorkState: &working, MovedAt: at},
		{BoardID: "board-a", ColumnID: "col-a", WorkState: &waiting, MovedAt: at},
		{BoardID: "board-c", ColumnID: "col-c", WorkState: &working, MovedAt: at.Add(-time.Hour)},
	}
	for _, order := range [][]int{{0, 1, 2}, {1, 0, 2}, {2, 1, 0}} {
		shuffled := []model.PlacementWorkState{placements[order[0]], placements[order[1]], placements[order[2]]}
		state, source := model.SharedWorkState(shuffled)
		if workStateText(state) != "waiting" || source.BoardID != "board-a" {
			t.Fatalf("order %v: got %s from %+v, want waiting from board-a", order, workStateText(state), source)
		}
	}
	if state, source := model.SharedWorkState(nil); state != nil || source != nil {
		t.Fatalf("no placements: got %v %v, want nil nil", state, source)
	}
}

// The reverse lookup answers a project rollup in one call: each row carries
// the card id, the item (title, noteboard status) and the card's shared
// work_state, and the items come from one noteboard query, not a read per card.
func TestEntityCardsCarryTitleStatusAndWorkStateFromOneNoteboardCall(t *testing.T) {
	h, notes, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Northwind work")
	inProgress := mkColumnWithWorkState(t, h, boardID, "In Progress", "working")
	done := mkColumnWithWorkState(t, h, boardID, "Done", "done")
	unmapped := mkColumnWithWorkState(t, h, boardID, "Inbox", "")
	if w := do(t, h, "PATCH", "/api/columns/"+done, model.UpdateColumnRequest{AutoStatus: stringPointer("done")}); w.Code != 200 {
		t.Fatalf("give Done an auto_status: %d %s", w.Code, w.Body.String())
	}

	const project = "project_000001"
	link := func(cardID string) {
		t.Helper()
		mustStatus(t, do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{EntityType: "project", EntityRef: project}), 201, "link card to project")
	}
	newCard := func(title, columnID string) string {
		t.Helper()
		var card model.CardView
		decodeSuccessfulResponse(t, do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: title, ColumnID: columnID}), &card)
		link(card.Placement.CardID)
		return card.Placement.CardID
	}
	working := newCard("build the rollup", inProgress)
	finished := newCard("register the type", done)
	inbox := newCard("someday", unmapped)
	// A link to a card id that has no noteboard item and no placement: the
	// orphan a caller must be able to spot.
	link("gone-card")

	queriesBefore, readsBefore := notes.queries, notes.itemReads
	w := do(t, h, "GET", "/api/entities/project/"+project+"/cards", nil)
	var rows []map[string]any
	decodeSuccessfulResponse(t, w, &rows)
	if notes.queries-queriesBefore != 1 || notes.itemReads != readsBefore {
		t.Fatalf("reverse lookup asked noteboard %d queries and %d single reads, want 1 and 0", notes.queries-queriesBefore, notes.itemReads-readsBefore)
	}
	type want struct {
		cardID, title, status string
		workState             any
	}
	wants := []want{
		{working, "build the rollup", "open", "working"},
		{finished, "register the type", "done", "done"},
		{inbox, "someday", "open", nil},
	}
	if len(rows) != len(wants)+1 {
		t.Fatalf("got %d rows, want %d: %s", len(rows), len(wants)+1, w.Body.String())
	}
	for i, expected := range wants {
		row := rows[i]
		item, _ := row["item"].(map[string]any)
		workState, present := row["work_state"]
		if row["card_id"] != expected.cardID || item == nil || item["title"] != expected.title || item["status"] != expected.status ||
			!present || workState != expected.workState {
			encoded, _ := json.Marshal(row)
			t.Errorf("row %d = %s, want card %s %q status %s work_state %v", i, encoded, expected.cardID, expected.title, expected.status, expected.workState)
		}
		if source, _ := row["work_state_source"].(map[string]any); source == nil || source["board_id"] != boardID {
			t.Errorf("row %d: work_state_source %v, want board %s", i, row["work_state_source"], boardID)
		}
	}
	orphan := rows[3]
	if orphan["card_id"] != "gone-card" || orphan["item"] != nil || orphan["work_state"] != nil || orphan["work_state_source"] != nil {
		t.Fatalf("orphan row = %v, want item null, work_state null and no source", orphan)
	}
}

// The state is the card's and every reader gets it; the board it came from is
// not named to a principal who cannot view that board.
func TestWorkStateSourceIsHiddenFromAPrincipalWhoCannotSeeItsBoard(t *testing.T) {
	h, grants, _ := setupWithPrincipalEnforcement(t)
	f := buildTwoBoards(t, h, grants)
	mustStatus(t, requestAs(t, h, asService, "PATCH", "/api/columns/"+f.supportColumnID,
		model.UpdateColumnRequest{WorkState: stringPointer("waiting")}), 200, "map Support's column")
	mustStatus(t, requestAs(t, h, asService, "POST", "/api/cards/"+f.sharedCardID+"/move",
		model.MoveCardRequest{BoardID: f.supportBoardID, ColumnID: f.supportColumnID}), 200, "move the shared card on Support last")

	var asBob model.CardDetail
	decodeSuccessfulResponse(t, requestAs(t, h, asPrincipal(bob), "GET", "/api/cards/"+f.sharedCardID, nil), &asBob)
	if workStateText(asBob.WorkState) != "waiting" || asBob.WorkStateSource == nil ||
		asBob.WorkStateSource.BoardID != "" || asBob.WorkStateSource.ColumnID != "" || asBob.WorkStateSource.ColumnName != "" || asBob.WorkStateSource.MovedAt.IsZero() {
		t.Fatalf("bob (Finance only) sees %s from %+v, want waiting with the Support board unnamed", workStateText(asBob.WorkState), asBob.WorkStateSource)
	}
	var asAlice model.CardDetail
	decodeSuccessfulResponse(t, requestAs(t, h, asPrincipal(alice), "GET", "/api/cards/"+f.sharedCardID, nil), &asAlice)
	if asAlice.WorkStateSource == nil || asAlice.WorkStateSource.BoardID != f.supportBoardID {
		t.Fatalf("alice (Support) sees source %+v, want Support named", asAlice.WorkStateSource)
	}
}
