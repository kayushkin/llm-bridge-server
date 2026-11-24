package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/messaging"
	"github.com/kayushkin/kanban-store/internal/model"
)

// Message triggers — board settings that send a text message through
// multichat when a card action happens on the board.
//
//	GET, POST          /api/boards/{id}/message-triggers
//	GET                /api/boards/{id}/message-deliveries?limit=100
//	GET, PATCH, DELETE /api/message-triggers/{id}
//	GET                /api/message-trigger-options

// SetMessageSender connects the message-trigger dispatcher to a sender. Until
// it is called every matching trigger records a not_configured delivery.
func (a *API) SetMessageSender(sender messaging.Sender) {
	a.messages.SetSender(sender)
}

// fireMessageTriggers hands a stored event to the dispatcher without making
// the card write wait on multichat.
func (a *API) fireMessageTriggers(event *model.CardEvent) {
	if event == nil || !model.IsMessageTriggerEventKind(event.Kind) {
		return
	}
	stored := *event
	a.messageTriggersInFlight.Add(1)
	go func() {
		defer a.messageTriggersInFlight.Done()
		a.messages.FireTriggersFor(stored)
	}()
}

// WaitForMessageTriggers returns once every dispatch already started has
// finished. The dispatch outlives the response that started it and reads the
// store, so call this before closing the store: a dispatch still running then
// fails with "database is closed", or re-creates SQLite's journal in a
// directory that is being removed.
func (a *API) WaitForMessageTriggers() {
	a.messageTriggersInFlight.Wait()
}

func (a *API) boardMessageTriggers(w http.ResponseWriter, r *http.Request, boardID string) {
	switch r.Method {
	case "GET":
		if _, err := a.store.GetBoard(boardID); err != nil {
			mapDBErr(w, err)
			return
		}
		triggers, err := a.store.ListMessageTriggers(boardID, false)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		ladder, err := a.store.GetPriorityLadder(boardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		for _, trigger := range triggers {
			labelPriority(trigger, ladder)
		}
		writeJSON(w, 200, triggers)
	case "POST":
		if _, err := a.store.GetBoard(boardID); err != nil {
			mapDBErr(w, err)
			return
		}
		var req model.UpsertMessageTriggerRequest
		if err := decodeStrictJSON(r, &req); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		trigger := &model.MessageTrigger{BoardID: boardID, Enabled: true}
		req.ApplyTo(trigger)
		ladder, status, err := a.checkMessageTrigger(trigger)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		created, err := a.store.CreateMessageTrigger(trigger)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		labelPriority(created, ladder)
		writeJSON(w, 201, created)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) messageTriggerByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/message-triggers/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, 404, "not found")
		return
	}
	switch r.Method {
	case "GET":
		trigger, err := a.store.GetMessageTrigger(id)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		ladder, err := a.store.GetPriorityLadder(trigger.BoardID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		labelPriority(trigger, ladder)
		writeJSON(w, 200, trigger)
	case "PATCH":
		trigger, err := a.store.GetMessageTrigger(id)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		var req model.UpsertMessageTriggerRequest
		if err := decodeStrictJSON(r, &req); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		req.ApplyTo(trigger)
		ladder, status, err := a.checkMessageTrigger(trigger)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		updated, err := a.store.UpdateMessageTrigger(trigger)
		if err != nil {
			mapDBErr(w, err)
			return
		}
		labelPriority(updated, ladder)
		writeJSON(w, 200, updated)
	case "DELETE":
		if err := a.store.DeleteMessageTrigger(id); err != nil {
			mapDBErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) boardMessageDeliveries(w http.ResponseWriter, r *http.Request, boardID string) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, 400, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if _, err := a.store.GetBoard(boardID); err != nil {
		mapDBErr(w, err)
		return
	}
	deliveries, err := a.store.ListMessageDeliveries(boardID, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, deliveries)
}

// messageTriggerOptions serves what the settings page needs to build a
// trigger without hardcoding anything: the event kinds a trigger may name,
// the fields a template may use, and whether this kanban-store can deliver
// at all.
func (a *API) messageTriggerOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	fieldType := reflect.TypeOf(model.MessageTemplateFields{})
	fields := make([]string, 0, fieldType.NumField())
	for i := 0; i < fieldType.NumField(); i++ {
		fields = append(fields, fieldType.Field(i).Name)
	}
	writeJSON(w, 200, model.MessageTriggerOptions{
		EventKinds:             model.MessageTriggerEventKinds,
		ColumnFilterEventKinds: model.MessageTriggerColumnFilterEventKinds,
		TemplateFields:         fields,
		DeliveryConfigured:     a.messages.DeliveryConfigured(),
	})
}

// checkMessageTrigger validates a merged trigger, including the two things
// only the store can answer: the column belongs to this board, and the
// priority value is a rung on this board's ladder. It returns the ladder so
// the caller can label the response.
func (a *API) checkMessageTrigger(trigger *model.MessageTrigger) (*model.PriorityLadder, int, error) {
	if err := trigger.Validate(); err != nil {
		return nil, 400, err
	}
	if trigger.ToColumnID != "" {
		column, err := a.store.GetColumn(trigger.ToColumnID)
		if errors.Is(err, db.ErrNotFound) {
			return nil, 400, fmt.Errorf("to_column_id %s does not exist", trigger.ToColumnID)
		}
		if err != nil {
			return nil, 500, err
		}
		if column.BoardID != trigger.BoardID {
			return nil, 400, fmt.Errorf("to_column_id %s is a column of board %s, not of this board", trigger.ToColumnID, column.BoardID)
		}
	}
	ladder, err := a.store.GetPriorityLadder(trigger.BoardID)
	if err != nil {
		return nil, 500, err
	}
	if trigger.PriorityValue != nil && ladder.LevelFor(*trigger.PriorityValue) == nil {
		rungs := make([]string, 0, len(ladder.Levels))
		for _, level := range ladder.Levels {
			rungs = append(rungs, fmt.Sprintf("%s=%d", level.Label, level.PriorityValue))
		}
		if len(rungs) == 0 {
			return nil, 400, fmt.Errorf("priority_value %d: this board has no priority ladder, so it cannot filter by priority", *trigger.PriorityValue)
		}
		return nil, 400, fmt.Errorf("priority_value %d is not a rung on this board's ladder (%s)", *trigger.PriorityValue, strings.Join(rungs, ", "))
	}
	return ladder, 0, nil
}

func labelPriority(trigger *model.MessageTrigger, ladder *model.PriorityLadder) {
	trigger.PriorityLabel = ""
	if trigger.PriorityValue == nil {
		return
	}
	if level := ladder.LevelFor(*trigger.PriorityValue); level != nil {
		trigger.PriorityLabel = level.Label
	}
}

func decodeStrictJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	return nil
}
