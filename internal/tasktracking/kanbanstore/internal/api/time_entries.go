package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kayushkin/kanban-store/internal/boundedtext"
	"github.com/kayushkin/kanban-store/internal/config"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// Hand-logged time. An entry is a time_logged event on the card's timeline; a
// correction is a new entry that names the one it replaces. See
// model.EventTimeLogged for why it happens when it is logged and moves no clock.

func (a *API) cardTimeEntries(w http.ResponseWriter, r *http.Request, cardID string) {
	switch r.Method {
	case http.MethodGet:
		activeOnly := false
		switch value := r.URL.Query().Get("active"); value {
		case "", "false":
		case "true":
			activeOnly = true
		default:
			writeError(w, 400, fmt.Sprintf("active must be true or false, got %q", value))
			return
		}
		entries, err := a.timeEntriesOfCard(cardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		answer := model.CardTimeEntries{CardID: cardID, Entries: []model.TimeEntry{}}
		for _, entry := range entries {
			if entry.Active {
				answer.ActiveSeconds += entry.Seconds
			}
			if entry.Active || !activeOnly {
				answer.Entries = append(answer.Entries, entry)
			}
		}
		writeJSON(w, 200, answer)
	case http.MethodPost:
		a.createTimeEntry(w, r, cardID)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// timeEntriesOfCard reads the card's time_logged events as entries, oldest
// first, each marked with the entry that replaced it.
func (a *API) timeEntriesOfCard(cardID string) ([]model.TimeEntry, error) {
	events, err := a.store.ListCardEvents(cardID, "")
	if err != nil {
		return nil, err
	}
	supersededBy := model.SupersededByEventID(events)
	entries := []model.TimeEntry{}
	for _, event := range events {
		if event.Kind != model.EventTimeLogged {
			continue
		}
		entry, err := timeEntryOfEvent(event)
		if err != nil {
			return nil, err
		}
		entry.SupersededByEventID = supersededBy[event.ID]
		entry.Active = entry.SupersededByEventID == ""
		entries = append(entries, entry)
	}
	return entries, nil
}

func timeEntryOfEvent(event model.CardEvent) (model.TimeEntry, error) {
	var detail model.TimeLoggedEventDetail
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		// Only createTimeEntry writes this kind, so an unreadable detail is a
		// corrupt row, and guessing its seconds would misstate the total.
		return model.TimeEntry{}, fmt.Errorf("time_logged event %s has unreadable detail: %w", event.ID, err)
	}
	return model.TimeEntry{
		EventID: event.ID, CardID: event.CardID, BoardID: event.BoardID,
		Seconds: detail.Seconds, WorkerPrincipalID: detail.WorkerPrincipalID, WorkedAt: detail.WorkedAt,
		Summary: event.Summary, LoggedBy: event.Actor, LoggedAt: event.OccurredAt,
		SupersedesEventID: event.SupersedesEventID, Active: true,
	}, nil
}

func (a *API) createTimeEntry(w http.ResponseWriter, r *http.Request, cardID string) {
	var request model.CreateTimeEntryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := request.Validate(time.Now()); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	placements, err := a.store.ListPlacementsByCard(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(placements) == 0 {
		writeError(w, 404, "not found")
		return
	}

	// Who did the work: named, or the caller. Checked with principal-store, as
	// an assignment is, and never written on a guess.
	workerID := request.WorkerPrincipalID
	if workerID == "" {
		if access := principalBoardAccessFrom(r); access != nil {
			workerID = access.PrincipalID
		} else {
			workerID = r.Header.Get(PrincipalIDHeader) // an administrator
		}
	}
	if workerID == "" {
		writeError(w, 400, "worker_principal_id is required when the caller is not a principal: a time entry says who did the work")
		return
	}
	if !principalIDShape.MatchString(workerID) {
		writeError(w, 400, fmt.Sprintf("worker_principal_id must match ^%s$ (for example principal_000001), got %q", config.PrincipalIDPattern, boundedtext.Value(workerID)))
		return
	}
	worker, err := a.principals.Get(workerID)
	if errors.Is(err, principalstore.ErrNotFound) {
		writeError(w, 400, fmt.Sprintf("worker_principal_id %s does not exist in principal-store", boundedtext.Value(workerID)))
		return
	}
	if err != nil {
		writeError(w, 502, "principal-store check failed: "+err.Error())
		return
	}
	if worker.Disabled() {
		writeError(w, 400, fmt.Sprintf("worker_principal_id %s is disabled in principal-store", boundedtext.Value(workerID)))
		return
	}
	if worker.Kind != principalstore.KindHuman {
		writeError(w, 400, fmt.Sprintf("worker_principal_id %s is a %q principal; time is logged for a %q", workerID, worker.Kind, principalstore.KindHuman))
		return
	}

	boardID := request.BoardID
	if request.SupersedesEventID != "" {
		replaced, err := a.store.GetCardEvent(request.SupersedesEventID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && (replaced.CardID != cardID || replaced.Kind != model.EventTimeLogged)) {
			writeError(w, 400, fmt.Sprintf("supersedes_event_id %s is not a time entry on this card", request.SupersedesEventID))
			return
		}
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// A correction belongs to the timeline its entry is on. Filed on another
		// board, one board would count both and the other neither.
		if boardID != "" && boardID != replaced.BoardID {
			writeError(w, 400, fmt.Sprintf("board_id %s differs from the entry being replaced, which is on board %q; leave it out and the correction takes that entry's board", boardID, replaced.BoardID))
			return
		}
		boardID = replaced.BoardID
	}

	state, err := a.clockStateCarriedForward(cardID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	detail, err := json.Marshal(model.TimeLoggedEventDetail{Seconds: request.Seconds, WorkerPrincipalID: workerID, WorkedAt: request.WorkedAt})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	stored, err := a.store.RecordCardEvent(&model.CardEvent{
		CardID: cardID, BoardID: boardID, Kind: model.EventTimeLogged, ClockState: state,
		Actor: actorFrom(r), Summary: request.Summary, Detail: detail,
		SupersedesEventID: request.SupersedesEventID,
	})
	if errors.Is(err, db.ErrEventAlreadySuperseded) {
		// Someone corrected this entry first. Name their correction, so the
		// caller can read it and correct that one instead.
		message := fmt.Sprintf("time entry %s has already been replaced", request.SupersedesEventID)
		if successor, lookupErr := a.store.SupersederOfCardEvent(request.SupersedesEventID); lookupErr == nil {
			message += fmt.Sprintf(" by %s; correct that entry instead", successor.ID)
		}
		writeError(w, 409, message)
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	a.fireMessageTriggers(stored)
	entry, err := timeEntryOfEvent(*stored)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, entry)
}
