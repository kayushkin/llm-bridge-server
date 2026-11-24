package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/model"
)

// clockState is a pointer helper: the column and ladder requests take pointers so
// that "leave it alone" and "clear it" stay distinguishable.
func clockState(s model.ClockState) *model.ClockState { return &s }

func seconds(n int) *int { return &n }

// mkColumnWithClock creates a column classified for the budget clock.
func mkColumnWithClock(t *testing.T, h http.Handler, boardID, name string, state model.ClockState) string {
	t.Helper()
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{
		Name: name, BudgetClockState: clockState(state),
	})
	if w.Code != 201 {
		t.Fatalf("create column %s: %d %s", name, w.Code, w.Body.String())
	}
	var c model.Column
	decodeSuccessfulResponse(t, w, &c)
	if c.BudgetClockState == nil || *c.BudgetClockState != state {
		t.Fatalf("column %s came back with clock %v, want %s", name, c.BudgetClockState, state)
	}
	return c.ID
}

// TestTimelineReplaysTheWorkedExample drives the case the feature was designed
// around, over real HTTP: mail arrives on a P0 card, half an hour of work is
// logged, a reply is sent and the card waits a day on someone else, the answer
// comes back, and an hour later it is done.
//
// Twenty-five and a half hours pass. Two were allowed. Only ninety minutes of it
// was time the work could actually be done, so the card came in under its limit —
// which is the entire point of counting two clocks instead of one.
func TestTimelineReplaysTheWorkedExample(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Email")
	actionNeeded := mkColumnWithClock(t, h, boardID, "Action needed", model.ClockRunning)
	waiting := mkColumnWithClock(t, h, boardID, "Waiting on reply", model.ClockPaused)
	completed := mkColumnWithClock(t, h, boardID, "Action completed", model.ClockStopped)

	// P0 is the TOP rung, and the top rung is the highest stored value — not zero,
	// which is where noteboard leaves everything nobody has ranked.
	w := do(t, h, "PUT", "/api/boards/"+boardID+"/priority-levels", model.SetPriorityLadderRequest{
		Levels: []model.BoardPriorityLevel{
			{PriorityValue: 5, Label: "P0", BudgetSeconds: seconds(2 * 3600)},
			{PriorityValue: 4, Label: "P1", BudgetSeconds: seconds(8 * 3600)},
			{PriorityValue: 3, Label: "P2", BudgetSeconds: seconds(24 * 3600)},
			{PriorityValue: 2, Label: "P3", BudgetSeconds: seconds(7 * 24 * 3600)},
			{PriorityValue: 1, Label: "P4", BudgetSeconds: seconds(30 * 24 * 3600)},
		},
	})
	if w.Code != 200 {
		t.Fatalf("set ladder: %d %s", w.Code, w.Body.String())
	}

	priority := 5
	w = do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "Contract question from the client", ColumnID: actionNeeded, Priority: &priority,
	})
	if w.Code != 201 {
		t.Fatalf("create card: %d %s", w.Code, w.Body.String())
	}
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	// Creating the card wrote its first event, and that event is what the clock
	// starts from — so the whole scenario is expressed by backdating from it.
	start := time.Now().UTC().Add(-25*time.Hour - 30*time.Minute)
	at := func(d time.Duration) *time.Time { v := start.Add(d); return &v }

	post := func(path string, body any) {
		t.Helper()
		if w := do(t, h, "POST", path, body); w.Code != 201 {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	post("/api/cards/"+cardID+"/events", model.CreateCardEventRequest{
		Kind: model.EventEmailReceived, BoardID: boardID, Actor: "email-classifier",
		Summary: "Client asks which contract clause applies", OccurredAt: at(0),
	})
	// Half an hour of work, recorded as a note — a summary of what was done, which
	// is exactly the kind of thing D4 moved out of the noteboard body and onto the
	// card as activity.
	post("/api/cards/"+cardID+"/notes", model.CreateCardNoteRequest{
		BoardID: boardID, Kind: "summary", Actor: "me",
		Body: "Read the contract, drafted an answer, sent it for legal sign-off.", OccurredAt: at(30 * time.Minute),
	})
	// Ball handed over: the clock pauses even though the card stays open.
	post("/api/cards/"+cardID+"/events", model.CreateCardEventRequest{
		Kind: model.EventWaitingStarted, BoardID: boardID, Actor: "me",
		Summary: "Waiting on legal", OccurredAt: at(30 * time.Minute),
	})
	// A day later the answer arrives and the clock runs again.
	post("/api/cards/"+cardID+"/events", model.CreateCardEventRequest{
		Kind: model.EventEmailReceived, BoardID: boardID, Actor: "email-classifier",
		Summary: "Legal replied", OccurredAt: at(24*time.Hour + 30*time.Minute),
	})
	// An hour of work, then done.
	post("/api/cards/"+cardID+"/events", model.CreateCardEventRequest{
		Kind: model.EventCardCompleted, BoardID: boardID, Actor: "me",
		Summary: "Answered the client", OccurredAt: at(25*time.Hour + 30*time.Minute),
	})

	w = do(t, h, "GET", "/api/cards/"+cardID+"/timeline?board_id="+boardID, nil)
	if w.Code != 200 {
		t.Fatalf("timeline: %d %s", w.Code, w.Body.String())
	}
	var timeline struct {
		Summary *model.CardTimeSummary    `json:"summary"`
		Entries []model.TimelineEntry     `json:"entries"`
		Level   *model.BoardPriorityLevel `json:"priority_level"`
	}
	decodeSuccessfulResponse(t, w, &timeline)

	// The card-created event sits a hair before the backdated mail, so the figures
	// are checked to the minute rather than to the second.
	roundedHours := func(sec float64) float64 { return float64(int(sec/60+0.5)) / 60 }
	if got := roundedHours(timeline.Summary.ElapsedSeconds); got != 25.5 {
		t.Errorf("elapsed = %vh, want 25.5h", got)
	}
	if got := roundedHours(timeline.Summary.BudgetClockSeconds); got != 1.5 {
		t.Errorf("budget clock = %vh, want 1.5h", got)
	}
	if got := roundedHours(timeline.Summary.WaitingSeconds); got != 24 {
		t.Errorf("waiting = %vh, want 24h", got)
	}
	if timeline.Summary.BudgetSeconds == nil || *timeline.Summary.BudgetSeconds != 2*3600 {
		t.Errorf("budget = %v, want 7200s from the P0 rung", timeline.Summary.BudgetSeconds)
	}
	if timeline.Summary.OverBudget == nil || *timeline.Summary.OverBudget {
		t.Error("card should be inside its limit: 1.5h of workable time against 2h")
	}
	if timeline.Level == nil || timeline.Level.Label != "P0" {
		t.Errorf("priority level = %v, want P0", timeline.Level)
	}
	if timeline.Summary.ClockState != model.ClockStopped {
		t.Errorf("clock state = %q, want stopped", timeline.Summary.ClockState)
	}

	// The timeline reads as a story: six actions, each with the gap before it.
	if len(timeline.Entries) != 6 {
		t.Fatalf("timeline has %d entries, want 6", len(timeline.Entries))
	}
	var noteBodies int
	for _, e := range timeline.Entries {
		if e.Note != nil {
			noteBodies++
			if e.Note.Kind != "summary" {
				t.Errorf("note kind = %q, want summary", e.Note.Kind)
			}
		}
	}
	if noteBodies != 1 {
		t.Errorf("timeline carried %d notes, want the one that was written", noteBodies)
	}
	// The gap the timeline has to make obvious is the day nobody could do anything.
	var replyGap float64
	for _, e := range timeline.Entries {
		if e.Event.Summary == "Legal replied" {
			replyGap = e.SecondsSincePreviousEvent
		}
	}
	if replyGap != 24*3600 {
		t.Errorf("gap before the reply = %vs, want 86400s", replyGap)
	}

	// The board view answers the same question without a per-card round trip,
	// because that badge is drawn for every card on screen.
	w = do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil)
	var view model.BoardView
	decodeSuccessfulResponse(t, w, &view)
	if view.PriorityLadder == nil || len(view.PriorityLadder.Levels) != 5 {
		t.Fatalf("board view ladder = %v, want 5 rungs", view.PriorityLadder)
	}
	if view.PriorityLadder.Levels[0].Label != "P0" {
		t.Errorf("top rung is %q, want P0 — the ladder must come back top-first", view.PriorityLadder.Levels[0].Label)
	}
	var found *model.CardTimeSummary
	for _, col := range view.Columns {
		for _, card := range col.Cards {
			if card.Placement.CardID == cardID {
				found = card.Time
			}
		}
	}
	if found == nil {
		t.Fatal("board view carried no time summary for the card")
	}
	if got := roundedHours(found.BudgetClockSeconds); got != 1.5 {
		t.Errorf("board view budget clock = %vh, want 1.5h", got)
	}
	if found.PriorityLabel != "P0" {
		t.Errorf("board view label = %q, want P0", found.PriorityLabel)
	}
	if found.Segments != nil {
		t.Error("board view should not carry per-card segments; that detail belongs to the timeline")
	}
	_ = waiting
	_ = completed
}

// TestMovingIntoAColumnTakesItsClock pins the column half of the design: a move
// is an action like any other, and what it means for the clock is whatever the
// destination column declares.
func TestMovingIntoAColumnTakesItsClock(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Agent runs")
	inProgress := mkColumnWithClock(t, h, boardID, "In Progress", model.ClockRunning)
	blocked := mkColumnWithClock(t, h, boardID, "Blocked", model.ClockPaused)

	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "Fix the deploy", ColumnID: inProgress,
	})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	w = do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{
		BoardID: boardID, ColumnID: blocked,
	})
	if w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	var events []model.CardEvent
	w = do(t, h, "GET", "/api/cards/"+cardID+"/events?board_id="+boardID, nil)
	decodeSuccessfulResponse(t, w, &events)
	if len(events) != 2 {
		t.Fatalf("events = %d, want a creation and a move", len(events))
	}
	move := events[1]
	if move.Kind != model.EventCardMoved || move.ClockState != model.ClockPaused {
		t.Errorf("move recorded as %s/%s, want card_moved/paused", move.Kind, move.ClockState)
	}
	if move.FromColumnID != inProgress || move.ToColumnID != blocked {
		t.Errorf("move went %s → %s, want %s → %s", move.FromColumnID, move.ToColumnID, inProgress, blocked)
	}
}

// TestUnclassifiedColumnLeavesTheClockAlone pins the other half: a column nobody
// has classified must not invent a clock change. The card keeps the state its
// last action left it in.
func TestUnclassifiedColumnLeavesTheClockAlone(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Projects")
	blocked := mkColumnWithClock(t, h, boardID, "Blocked", model.ClockPaused)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/columns", model.CreateColumnRequest{Name: "Someday"})
	var someday model.Column
	decodeSuccessfulResponse(t, w, &someday)

	w = do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Rewrite it", ColumnID: blocked})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{BoardID: boardID, ColumnID: someday.ID}); w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	var events []model.CardEvent
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events?board_id="+boardID, nil), &events)
	last := events[len(events)-1]
	if last.ClockState != model.ClockPaused {
		t.Errorf("clock after moving into an unclassified column = %q, want the paused state it already had", last.ClockState)
	}
}

// TestHoldPausesTheClockEverywhere pins that the stop button is a clock event and
// that it is deliberately not board-scoped: parking work parks it on every board
// the card sits on.
func TestHoldPausesTheClockEverywhere(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Agent runs")
	col := mkColumnWithClock(t, h, boardID, "In Progress", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Risky migration", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/hold", map[string]string{"reason": "wait for me"}); w.Code != 200 {
		t.Fatalf("hold: %d %s", w.Code, w.Body.String())
	}
	var events []model.CardEvent
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events", nil), &events)
	held := events[len(events)-1]
	if held.Kind != model.EventCardHeld || held.ClockState != model.ClockPaused {
		t.Errorf("hold recorded as %s/%s, want card_held/paused", held.Kind, held.ClockState)
	}
	if held.BoardID != "" {
		t.Errorf("hold event was scoped to board %q; a hold applies wherever the card sits", held.BoardID)
	}
	if held.Summary != "wait for me" {
		t.Errorf("hold reason = %q, want it carried onto the timeline", held.Summary)
	}

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/unhold", nil); w.Code != 200 {
		t.Fatalf("unhold: %d %s", w.Code, w.Body.String())
	}
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events", nil), &events)
	if resumed := events[len(events)-1]; resumed.ClockState != model.ClockRunning {
		t.Errorf("clock after play = %q, want running", resumed.ClockState)
	}
}

// TestMailAndAgentLinksLandOnTheTimeline pins which links are actions. Attaching
// mail or handing the card to an agent is something that happened; tagging it
// with the repo it concerns is not.
func TestMailAndAgentLinksLandOnTheTimeline(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Email")
	col := mkColumnWithClock(t, h, boardID, "Action needed", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Invoice", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	for _, l := range []model.CreateCardLinkRequest{
		{EntityType: "email", EntityRef: "acct/msg-1"},
		{EntityType: "email_msgid", EntityRef: "<abc@example.com>"},
		{EntityType: "email_sender", EntityRef: "billing@example.com"},
		{EntityType: "session", EntityRef: "sess-1"},
		{EntityType: "repo", EntityRef: "/home/me/repos/thing"},
	} {
		if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", l); w.Code != 201 {
			t.Fatalf("link %s: %d %s", l.EntityType, w.Code, w.Body.String())
		}
	}

	var events []model.CardEvent
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events", nil), &events)
	kinds := map[model.EventKind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds[model.EventEmailReceived] != 1 {
		t.Errorf("email arrivals on the timeline = %d, want 1: the msgid and sender links are the same arrival and an affinity, not events", kinds[model.EventEmailReceived])
	}
	if kinds[model.EventAgentDispatched] != 1 {
		t.Errorf("agent dispatches = %d, want 1", kinds[model.EventAgentDispatched])
	}
	if len(events) != 3 {
		t.Errorf("events = %d, want creation + email + dispatch; a repo link is a fact, not an action", len(events))
	}
}

// TestCompletionStopsTheClockHoweverItIsExpressed pins that a card patched
// straight to done stops accruing, not only one dragged into a Done column.
func TestCompletionStopsTheClockHoweverItIsExpressed(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Projects")
	col := mkColumnWithClock(t, h, boardID, "In Progress", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Ship it", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	if w := do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"status": "done"}); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var events []model.CardEvent
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events", nil), &events)
	last := events[len(events)-1]
	if last.Kind != model.EventCardCompleted || last.ClockState != model.ClockStopped {
		t.Errorf("completion recorded as %s/%s, want card_completed/stopped", last.Kind, last.ClockState)
	}
}

// TestUnknownEventKindDemandsAClockState pins the refusal: kanban-store will
// record an action it has never heard of, but it will not invent what that action
// means for the budget.
func TestUnknownEventKindDemandsAClockState(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	w := do(t, h, "POST", "/api/cards/whatever/events", model.CreateCardEventRequest{Kind: "shipped_a_pigeon"})
	if w.Code != 400 {
		t.Fatalf("unknown kind without a clock state: %d %s, want 400", w.Code, w.Body.String())
	}
	w = do(t, h, "POST", "/api/cards/whatever/events", model.CreateCardEventRequest{
		Kind: "shipped_a_pigeon", ClockState: model.ClockPaused,
	})
	if w.Code != 201 {
		t.Fatalf("unknown kind with a clock state: %d %s, want 201", w.Code, w.Body.String())
	}
}

// TestLadderRefusesALevelAtZeroOverHTTP pins the rule at the edge, not only in
// the validator: a board cannot define P0 as the literal zero, because that is
// where every unranked card already sits.
func TestLadderRefusesALevelAtZeroOverHTTP(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Email")
	w := do(t, h, "PUT", "/api/boards/"+boardID+"/priority-levels", model.SetPriorityLadderRequest{
		Levels: []model.BoardPriorityLevel{{PriorityValue: 0, Label: "P0", BudgetSeconds: seconds(7200)}},
	})
	if w.Code != 400 {
		t.Fatalf("level at 0: %d %s, want 400", w.Code, w.Body.String())
	}
}

// TestBoardWithoutALadderIgnoresPriorities pins that priorities are opt-in per
// board: a board with no ladder reports no rung and no limit, whatever a card's
// stored priority is.
func TestBoardWithoutALadderIgnoresPriorities(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Ideas")
	col := mkColumnWithClock(t, h, boardID, "Open", model.ClockRunning)
	priority := 5
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "Something urgent-looking", ColumnID: col, Priority: &priority,
	})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)

	var view model.BoardView
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil), &view)
	card := view.Columns[0].Cards[0]
	if card.Time.BudgetSeconds != nil || card.Time.PriorityLabel != "" {
		t.Errorf("board with no ladder produced %+v, want no rung and no limit", card.Time)
	}
	if view.PriorityLadder == nil || len(view.PriorityLadder.Levels) != 0 {
		t.Errorf("ladder = %v, want an empty one", view.PriorityLadder)
	}
}

// TestBusinessHoursAreReportedSeparately pins that the working-week figure is an
// extra alongside the wall-clock ones, and that a board without hours reports
// none rather than a guessed nine-to-five.
func TestBusinessHoursAreReportedSeparately(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Email")
	col := mkColumnWithClock(t, h, boardID, "Action needed", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Weekend mail", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	var view model.BoardView
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil), &view)
	if view.Columns[0].Cards[0].Time.BusinessHoursElapsedSeconds != nil {
		t.Error("a board with no business hours reported a business figure")
	}

	if w := do(t, h, "PATCH", "/api/boards/"+boardID, map[string]any{
		"business_hours": model.BusinessHours{
			TZID: "America/Los_Angeles", Days: []string{"MO", "TU", "WE", "TH", "FR"}, Start: "09:00", End: "17:00",
		},
	}); w.Code != 200 {
		t.Fatalf("set business hours: %d %s", w.Code, w.Body.String())
	}
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil), &view)
	if view.Columns[0].Cards[0].Time.BusinessHoursElapsedSeconds == nil {
		t.Error("board with business hours reported no business figure")
	}
	if view.Board.BusinessHours == nil || view.Board.BusinessHours.TZID != "America/Los_Angeles" {
		t.Errorf("board came back with hours %v", view.Board.BusinessHours)
	}

	// A zone is mandatory, because hours without one drift twice a year.
	if w := do(t, h, "PATCH", "/api/boards/"+boardID, map[string]any{
		"business_hours": map[string]any{"tzid": "Nowhere/Nothing", "days": []string{"MO"}, "start": "09:00", "end": "17:00"},
	}); w.Code != 400 {
		t.Fatalf("unknown zone: %d %s, want 400", w.Code, w.Body.String())
	}
	_ = cardID
}

// TestDeletingANoteLeavesItsEventStanding pins that removing the text of a note
// does not rewrite history or silently move every figure derived from it.
func TestDeletingANoteLeavesItsEventStanding(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Projects")
	col := mkColumnWithClock(t, h, boardID, "In Progress", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Thing", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	w = do(t, h, "POST", "/api/cards/"+cardID+"/notes", model.CreateCardNoteRequest{BoardID: boardID, Body: "half done"})
	var note model.CardNote
	decodeSuccessfulResponse(t, w, &note)

	if w := do(t, h, "DELETE", "/api/notes/"+note.ID, nil); w.Code != 200 {
		t.Fatalf("delete note: %d %s", w.Code, w.Body.String())
	}
	var events []model.CardEvent
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/events", nil), &events)
	var noteEvents int
	for _, e := range events {
		if e.Kind == model.EventNoteAdded {
			noteEvents++
		}
	}
	if noteEvents != 1 {
		t.Errorf("note_added events after deleting the note = %d, want it left standing", noteEvents)
	}
	var notes []model.CardNote
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/notes", nil), &notes)
	if len(notes) != 0 {
		t.Errorf("notes after delete = %d, want 0", len(notes))
	}
}

// TestNotesCanHandOverTheBall pins that a note may itself be the moment the clock
// changes hands, which is how "replied, waiting on them" is recorded in one step.
func TestNotesCanHandOverTheBall(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Email")
	col := mkColumnWithClock(t, h, boardID, "Action needed", model.ClockRunning)
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "Question", ColumnID: col})
	var created model.CardView
	decodeSuccessfulResponse(t, w, &created)
	cardID := created.Placement.CardID

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/notes", model.CreateCardNoteRequest{
		BoardID: boardID, Body: "Replied, waiting on them", ClockState: model.ClockPaused,
	}); w.Code != 201 {
		t.Fatalf("note: %d %s", w.Code, w.Body.String())
	}
	var timeline struct {
		Summary *model.CardTimeSummary `json:"summary"`
	}
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/cards/"+cardID+"/timeline?board_id="+boardID, nil), &timeline)
	if timeline.Summary.ClockState != model.ClockPaused {
		t.Errorf("clock after the handover note = %q, want paused", timeline.Summary.ClockState)
	}
}

// TestEventLogIsAppendOnly pins that there is no way to edit or delete an event
// through the API. A rewritable log is not a record of what happened.
func TestEventLogIsAppendOnly(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	for _, method := range []string{"PATCH", "DELETE", "PUT"} {
		w := do(t, h, method, "/api/cards/some-card/events", json.RawMessage(`{}`))
		if w.Code != 405 {
			t.Errorf("%s on the event log = %d, want 405", method, w.Code)
		}
	}
}

// TestBoardViewPagesEachColumn pins the cap that exists because this host's
// largest board answered twelve megabytes per read, on a page that polls every
// fifteen seconds.
func TestBoardViewPagesEachColumn(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Agent runs")
	col := mkColumnWithClock(t, h, boardID, "Queued", model.ClockRunning)
	for i := 0; i < 7; i++ {
		if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
			Title: "card " + itoa(i), ColumnID: col,
		}); w.Code != 201 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
	}

	var view model.BoardView
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards?limit=3", nil), &view)
	if got := len(view.Columns[0].Cards); got != 3 {
		t.Errorf("page carried %d cards, want 3", got)
	}
	// The total is what lets a client say "showing 3 of 7" instead of presenting
	// a page as the whole column.
	if got := view.Columns[0].Total; got != 7 {
		t.Errorf("total = %d, want 7", got)
	}

	// No limit still means the whole board, so every existing caller is unaffected.
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards", nil), &view)
	if got := len(view.Columns[0].Cards); got != 7 {
		t.Errorf("unpaged read carried %d cards, want all 7", got)
	}
}

// TestColumnCardsServesTheRest pins "show more": the next page of one column,
// in the same stored order the board view paged in, so appending it cannot
// duplicate or skip a card.
func TestColumnCardsServesTheRest(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()

	boardID := mkBoard(t, h, "Agent runs")
	col := mkColumnWithClock(t, h, boardID, "Queued", model.ClockRunning)
	for i := 0; i < 5; i++ {
		if w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
			Title: "card " + itoa(i), ColumnID: col,
		}); w.Code != 201 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
	}

	var first model.BoardView
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/boards/"+boardID+"/cards?limit=2", nil), &first)
	var next model.ColumnView
	decodeSuccessfulResponse(t, do(t, h, "GET", "/api/columns/"+col+"/cards?limit=2&offset=2", nil), &next)

	if len(next.Cards) != 2 || next.Total != 5 {
		t.Fatalf("next page carried %d of %d, want 2 of 5", len(next.Cards), next.Total)
	}
	seen := map[string]bool{}
	for _, c := range first.Columns[0].Cards {
		seen[c.Placement.CardID] = true
	}
	for _, c := range next.Cards {
		if seen[c.Placement.CardID] {
			t.Errorf("card %s appeared on both pages; paging must not overlap", c.Placement.CardID)
		}
	}
	// A page carries the same per-card time a board view does, or "show more"
	// would hand back cards that render without their clock.
	if next.Cards[0].Time == nil {
		t.Error("paged card carried no time summary")
	}
}

// linkTestCard builds a board with one column and one card on it, which is the
// minimum a link test needs.
func linkTestCard(t *testing.T, h http.Handler) string {
	t.Helper()
	boardID := mkBoard(t, h, "Links")
	columnID := mkColumn(t, h, boardID, "Todo", "")
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{
		Title: "A card with links", ColumnID: columnID,
	})
	if w.Code != 201 {
		t.Fatalf("create card: %d %s", w.Code, w.Body.String())
	}
	var card model.CardView
	decodeSuccessfulResponse(t, w, &card)
	return card.Placement.CardID
}

// A link that records an action can be backdated, because the action and the
// filing of it are two different moments. This is the path a classifier takes
// when it works through a backlog: the mail arrived days before anything here
// heard about it.
func TestABackdatedEmailLinkFilesTheArrivalWhenItHappened(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	arrived := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "email",
		EntityRef:  "demo-work:lm0001",
		OccurredAt: &arrived,
	})
	if w.Code != 201 {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}

	var link model.CardLink
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
		t.Fatalf("decode link: %v", err)
	}
	// The link itself records when we filed it, not when the mail arrived.
	if time.Since(link.CreatedAt) > time.Minute {
		t.Fatalf("link created_at %s was backdated; it should record the filing", link.CreatedAt)
	}

	var events []model.CardEvent
	w = do(t, h, "GET", "/api/cards/"+cardID+"/events", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var arrival *model.CardEvent
	for i := range events {
		if events[i].Kind == model.EventEmailReceived {
			arrival = &events[i]
		}
	}
	if arrival == nil {
		t.Fatal("no email_received event recorded for the link")
	}
	if !arrival.OccurredAt.Equal(arrived) {
		t.Fatalf("arrival occurred_at %s, want %s", arrival.OccurredAt, arrived)
	}
	if time.Since(arrival.RecordedAt) > time.Minute {
		t.Fatalf("recorded_at %s was backdated too; it must keep saying when we heard", arrival.RecordedAt)
	}
}

// Backdating a link that records no action is refused. Answering 201 would let
// the caller believe it had moved something on a timeline it never touched.
func TestBackdatingAFactLinkIsRefused(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	when := time.Now().UTC().Add(-time.Hour)
	for _, entityType := range []string{"repo", "git_repo", "email_msgid", "email_sender"} {
		t.Run(entityType, func(t *testing.T) {
			w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
				EntityType: entityType,
				EntityRef:  "something-" + entityType,
				OccurredAt: &when,
			})
			if w.Code != 400 {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
			}
		})
	}
}

// Without occurred_at the behavior is unchanged: the arrival is stamped when
// the link was made.
func TestALinkWithoutOccurredAtStillStampsNow(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "email", EntityRef: "demo-work:lm0002",
	})
	if w.Code != 201 {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var events []model.CardEvent
	w = do(t, h, "GET", "/api/cards/"+cardID+"/events", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	for _, e := range events {
		if e.Kind == model.EventEmailReceived && time.Since(e.OccurredAt) > time.Minute {
			t.Fatalf("arrival stamped %s without being asked to backdate", e.OccurredAt)
		}
	}
}

// A bucket card is the reason clock_state exists on a link. Mail arriving means
// work landed in front of you — on a card someone works. A hundred build
// digests filed on one No-action card are not a hundred arrivals of work, and
// letting them run the clock reports a card nobody has touched as having
// consumed weeks of budget.
func TestAnArrivalCanBeFiledWithoutStartingTheClock(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	arrived := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "email",
		EntityRef:  "demo-work:lm-digest",
		OccurredAt: &arrived,
		ClockState: model.ClockStopped,
	})
	if w.Code != 201 {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}

	var events []model.CardEvent
	w = do(t, h, "GET", "/api/cards/"+cardID+"/events", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	for _, e := range events {
		if e.Kind == model.EventEmailReceived {
			if e.ClockState != model.ClockStopped {
				t.Fatalf("arrival clock state %q, want stopped", e.ClockState)
			}
			return
		}
	}
	t.Fatal("no email_received event recorded")
}

func TestAnArrivalStillRunsTheClockByDefault(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "email", EntityRef: "demo-work:lm-work",
	}); w.Code != 201 {
		t.Fatalf("status %d, want 201: %s", w.Code, w.Body)
	}
	var events []model.CardEvent
	w := do(t, h, "GET", "/api/cards/"+cardID+"/events", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	for _, e := range events {
		if e.Kind == model.EventEmailReceived && e.ClockState != model.ClockRunning {
			t.Fatalf("arrival clock state %q, want running by default", e.ClockState)
		}
	}
}

func TestAnUnknownClockStateOnALinkIsRefused(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "email", EntityRef: "demo-work:lm-bad", ClockState: model.ClockState("dawdling"),
	}); w.Code != 400 {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestClockStateOnAFactLinkIsRefused(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	cardID := linkTestCard(t, h)

	if w := do(t, h, "POST", "/api/cards/"+cardID+"/links", model.CreateCardLinkRequest{
		EntityType: "repo", EntityRef: "/tmp/x", ClockState: model.ClockStopped,
	}); w.Code != 400 {
		t.Fatalf("status %d, want 400", w.Code)
	}
}
