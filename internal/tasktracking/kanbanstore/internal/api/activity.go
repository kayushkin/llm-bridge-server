package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/timeaccounting"
)

// ============================ Recording ============================

// recordEventAndFireMessageTriggers appends an action to a card's log and reports the failure rather
// than hiding it: a move that succeeded but went unlogged leaves the card's
// figures quietly wrong from then on, and a caller that never hears about it
// cannot know the timeline it later reads is short an entry.
//
// The write paths call this after the change they describe has succeeded, so a
// rejected move never lands in the log as one that happened.
//
// A recorded event is also handed to the board's message triggers, which may
// send a text message through multichat. That runs in the background: the
// write never waits on it, and its outcome is a delivery row, not this error.
func (a *API) recordEventAndFireMessageTriggers(e *model.CardEvent) error {
	stored, err := a.store.RecordCardEvent(e)
	if err != nil {
		return fmt.Errorf("record %s event for card %s: %w", e.Kind, e.CardID, err)
	}
	a.fireMessageTriggers(stored)
	return nil
}

// clockStateForColumn says what landing in a column means for the budget clock.
//
// This is a state machine, not a chain of guesses. A column that has been
// classified decides. A column that has not says nothing about the clock, so the
// card keeps the state its last action left it in — moving an unclassified card
// between unclassified columns must not invent a change. With no history at all
// the card is starting its life on a board, and work that exists and is not
// declared blocked is work that can be done, so the clock runs.
func (a *API) clockStateForColumn(cardID, boardID, columnID string) model.ClockState {
	if col, err := a.store.GetColumn(columnID); err == nil && col.BudgetClockState != nil && *col.BudgetClockState != "" {
		return *col.BudgetClockState
	}
	if state, ok, err := a.lastRecordedClockState(cardID, boardID); err == nil && ok {
		return state
	}
	return model.ClockRunning
}

// lastRecordedClockState is the state the card's most recent action left it in
// — on one board, or anywhere when boardID is empty. ok is false when the card
// has no history there.
func (a *API) lastRecordedClockState(cardID, boardID string) (model.ClockState, bool, error) {
	events, err := a.store.ListCardEvents(cardID, boardID)
	if err != nil {
		return "", false, err
	}
	if len(events) == 0 {
		return "", false, nil
	}
	return events[len(events)-1].ClockState, true, nil
}

// clockStateCarriedForward is what a card-wide action that says nothing about
// the clock records — a principal being assigned or unassigned — so that the
// segment after it is attributed exactly as the segment before it was. In
// order:
//
//  1. The state the card's most recent action left it in, on any board.
//  2. With no history, the classification of the column the card sits in, when
//     it sits in exactly one. Most cards on this host predate the event log —
//     7,245 of 9,222 placed cards had no events when this was written, 6,928 of
//     them in a stopped column — and their column is the one thing that says
//     what state they are in. Defaulting them to running would have restarted
//     every finished card the moment someone was assigned to it.
//  3. Running. A card with no history that sits nowhere classified is work that
//     exists and is not declared blocked, which is the same answer the store
//     gives any first action on such a card.
func (a *API) clockStateCarriedForward(cardID string) (model.ClockState, error) {
	if state, ok, err := a.lastRecordedClockState(cardID, ""); err != nil {
		return "", err
	} else if ok {
		return state, nil
	}
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		return "", err
	}
	if len(placements) == 1 {
		col, err := a.store.GetColumn(placements[0].ColumnID)
		if err != nil {
			return "", err
		}
		if col.BudgetClockState != nil && *col.BudgetClockState != "" {
			return *col.BudgetClockState, nil
		}
	}
	return model.ClockRunning, nil
}

// clockStateForEntityLink maps a link onto what it means for the clock, and
// reports ok=false for links that are not actions at all.
//
// Only two entity types are actions. 'email_msgid' is the same arrival recorded
// under its RFC identity and would double-count it; 'email_sender' is a learned
// affinity between a sender and a card rather than an event; a repo or a machine
// link is a fact about the card, not something that happened to it.
func clockStateForEntityLink(entityType string) (model.EventKind, model.ClockState, bool) {
	switch entityType {
	case "email":
		return model.EventEmailReceived, model.ClockRunning, true
	case "session":
		return model.EventAgentDispatched, model.ClockRunning, true
	}
	return "", "", false
}

// actorFrom reads who is acting from the request. Clients that do not say stay
// anonymous — an empty actor is an honest "we were not told", where a guess from
// the user agent would put a name on the timeline that nobody typed.
// actorFrom names who made a change for the event log. Under principal
// enforcement it is the calling principal, whatever ?actor= says, so the log
// cannot be written in someone else's name.
func actorFrom(r *http.Request) string {
	if access := principalBoardAccessFrom(r); access != nil {
		return access.PrincipalID
	}
	return r.URL.Query().Get("actor")
}

// ============================ Card events ============================

func (a *API) cardEvents(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case "GET":
		events, err := a.store.ListCardEvents(cardID, r.URL.Query().Get("board_id"))
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, events)
	case "POST":
		var req model.CreateCardEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if req.Kind == model.EventTimeLogged {
			// An entry may replace another, and that has rules this route does
			// not check.
			writeError(w, 400, "a time_logged event is written through POST /api/cards/{id}/time-entries, which checks who worked and which entry it replaces")
			return
		}
		state, _ := req.ResolveClockState()
		actor := req.Actor
		if actor == "" {
			actor = actorFrom(r)
		}
		stored, err := a.store.RecordCardEvent(&model.CardEvent{
			CardID: cardID, BoardID: req.BoardID, Kind: req.Kind, ClockState: state,
			Actor: actor, Summary: req.Summary, Detail: req.Detail,
			OccurredAt: db.EventTime(req.OccurredAt),
		})
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		a.fireMessageTriggers(stored)
		writeJSON(w, 201, stored)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// ============================ Card notes ============================

func (a *API) noteVisibilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.NoteVisibilities)
}

func (a *API) cardNotes(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case "GET":
		visibility := model.NoteVisibility(r.URL.Query().Get("visibility"))
		if visibility != "" && !model.ValidNoteVisibility(visibility) {
			// A filter that was dropped would answer every note, internal ones
			// included, to a reader that asked for the requester's.
			writeError(w, 400, fmt.Sprintf("visibility %q is not one of %v (GET /api/note-visibilities)", visibility, model.NoteVisibilities))
			return
		}
		notes, err := a.store.ListCardNotes(cardID, r.URL.Query().Get("board_id"), visibility)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, notes)
	case "POST":
		var req model.CreateCardNoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if req.Actor == "" {
			req.Actor = actorFrom(r)
		}
		note, err := a.store.CreateCardNote(cardID, &req)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// A note is an action, so it lands on the timeline. Some notes are the exact
		// moment the ball changes hands ("replied, waiting on them"), which is why a
		// note may carry its own clock state.
		state := model.ClockRunning
		if def, ok := model.DefaultClockStateForEventKind(model.EventNoteAdded); ok {
			state = def
		}
		if req.ClockState != "" {
			state = req.ClockState
		}
		// The summary is the note's text, so the event says who that text is for.
		detail, err := json.Marshal(model.NoteAddedEventDetail{Visibility: note.Visibility})
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if err := a.recordEventAndFireMessageTriggers(&model.CardEvent{
			CardID: cardID, BoardID: req.BoardID, Kind: model.EventNoteAdded, ClockState: state,
			Actor: req.Actor, Summary: note.Body, NoteID: note.ID, OccurredAt: note.CreatedAt,
			Detail: detail,
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, note)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) noteByID(w http.ResponseWriter, r *http.Request, noteID string) {
	if r.Method != "DELETE" {
		writeError(w, 405, "method not allowed")
		return
	}
	if err := a.store.DeleteCardNote(noteID); err != nil {
		mapDBErr(w, err)
		return
	}
	// The note is gone; the note_added event stays. The note was the content, the
	// event was the fact that something happened at that moment, and deleting the
	// first does not unmake the second — nor should it silently move every figure
	// computed from the timeline.
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ============================ Timeline ============================

func (a *API) cardTimeline(w http.ResponseWriter, r *http.Request, cardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	boardID := r.URL.Query().Get("board_id")
	// With no board named, the card's timeline is read on whichever board it sits
	// on, if it sits on exactly one. A card on several boards is asked about per
	// board, because the ladder and the working week are the board's, not the
	// card's.
	if boardID == "" {
		placements, err := a.store.ListPlacementsByCard(cardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if len(placements) == 1 {
			boardID = placements[0].BoardID
		}
	}
	events, err := a.store.ListCardEvents(cardID, boardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	notes, err := a.store.ListCardNotes(cardID, boardID, "")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	notesByID := map[string]*model.CardNote{}
	for i := range notes {
		notesByID[notes[i].ID] = &notes[i]
	}

	var level *model.BoardPriorityLevel
	var hours *model.BusinessHours
	if boardID != "" {
		board, err := a.store.GetBoard(boardID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			writeError(w, 500, err.Error())
			return
		}
		if board != nil {
			hours = board.BusinessHours
		}
		ladder, err := a.store.GetPriorityLadder(boardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if item, err := a.noteboard.GetItem(cardID); err == nil {
			level = ladder.LevelFor(model.PriorityOfNoteboardItem(item))
		}
	}

	summary, entries := timeaccounting.Compute(timeaccounting.Input{
		Events: events, Notes: notesByID, Level: level, Hours: hours, Now: time.Now().UTC(),
	})
	writeJSON(w, 200, model.CardTimeline{
		CardID: cardID, BoardID: boardID, Summary: summary, Entries: entries, Notes: notes, Level: level,
	})
}

// ============================ Priority ladder ============================

func (a *API) boardPriorityLevels(w http.ResponseWriter, r *http.Request, boardID string) {
	switch r.Method {
	case "GET":
		ladder, err := a.store.GetPriorityLadder(boardID)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, ladder)
	case "PUT":
		var req model.SetPriorityLadderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		ladder, err := a.store.SetPriorityLadder(boardID, req.Levels)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, ladder)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// notesByID routes /api/notes/{id}. Notes are addressed globally because a note
// belongs to a card, not to a board.
func (a *API) notesByID(w http.ResponseWriter, r *http.Request) {
	noteID := strings.TrimPrefix(r.URL.Path, "/api/notes/")
	if noteID == "" {
		writeError(w, 400, "missing note id")
		return
	}
	a.noteByID(w, r, noteID)
}

// columnCards serves one column's cards a page at a time — what "show more"
// calls once a board view has been capped.
//
// The page is in STORED order, the same order the board view pages in, because
// it is the only order kanban-store owns. A caller appending a page to what it
// already has re-sorts the result itself.
func (a *API) columnCards(w http.ResponseWriter, r *http.Request, columnID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	col, err := a.store.GetColumn(columnID)
	if err != nil {
		mapDBErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	filter, err := cardFilterFromQuery(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	content, err := cardContentQueryFromRequest(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	placements, items, total, err := a.columnPage(columnID, filter, content, limit, offset)
	if err != nil {
		writeCardReadError(w, err)
		return
	}
	ids := make([]string, len(placements))
	for i, p := range placements {
		ids[i] = p.CardID
	}
	linksByCard, err := a.store.ListCardLinksForCards(ids)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	assignmentsByCard, err := a.store.ListCardAssignmentsForCards(ids)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	eventsByCard, err := a.store.ListCardEventsForCards(ids, col.BoardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	board, err := a.store.GetBoard(col.BoardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ladder, err := a.store.GetPriorityLadder(col.BoardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// A page of a column is the same cards the board view shows, so it carries
	// the same ticket: without it, page two of a ticket list has no requester.
	ticketsByCard, err := a.store.ListTicketsForCards(ids)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	workStatesByCard, err := a.cardWorkStatesForCaller(ids, principalBoardAccessFrom(r))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	asOf := time.Now().UTC()
	cards := make([]model.CardView, 0, len(placements))
	for i, p := range placements {
		summary, _ := timeaccounting.Compute(timeaccounting.Input{
			Events: eventsByCard[p.CardID],
			Level:  ladder.LevelFor(model.PriorityOfNoteboardItem(items[i])),
			Hours:  board.BusinessHours,
			Now:    asOf,
		})
		summary.Segments = nil
		cards = append(cards, model.CardView{
			Placement: p, Item: items[i], Links: linksByCard[p.CardID],
			Assignments: assignmentsByCard[p.CardID], Ticket: ticketsByCard[p.CardID], Time: summary,
			WorkState: workStatesByCard[p.CardID].State, WorkStateSource: workStatesByCard[p.CardID].Source,
		})
	}
	writeJSON(w, 200, model.ColumnView{Column: col, Cards: cards, Total: total})
}
